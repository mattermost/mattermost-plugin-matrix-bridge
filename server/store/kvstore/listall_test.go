package kvstore

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawPageKVStore simulates pluginapi's ListKeys/ListKeysWithPrefix semantics precisely
// enough to exercise the paging bug this package's helpers exist to avoid: it pages the
// RAW (unfiltered) keyspace in fixed-size batches and filters each raw page to the
// requested prefix client-side, exactly like pluginapi.KVService.ListKeys does. A naive
// caller that pages on the FILTERED result's length would see a short page and stop
// early whenever matches are sparse relative to the rest of the keyspace.
type rawPageKVStore struct {
	keys []string // insertion order, simulating the store's own ordering
}

func (r *rawPageKVStore) GetTemplateData(_ string) (string, error) { return "", nil }
func (r *rawPageKVStore) Get(_ string) ([]byte, error)             { return nil, nil }
func (r *rawPageKVStore) Set(_ string, _ []byte) error             { return nil }
func (r *rawPageKVStore) Delete(_ string) error                    { return nil }

func (r *rawPageKVStore) ListKeys(page, perPage int) ([]string, error) {
	start := page * perPage
	if start >= len(r.keys) {
		return []string{}, nil
	}
	end := min(start+perPage, len(r.keys))
	return r.keys[start:end], nil
}

func (r *rawPageKVStore) ListKeysWithPrefix(page, perPage int, prefix string) ([]string, error) {
	rawPage, err := r.ListKeys(page, perPage)
	if err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(rawPage))
	for _, k := range rawPage {
		if strings.HasPrefix(k, prefix) {
			filtered = append(filtered, k)
		}
	}
	return filtered, nil
}

func (r *rawPageKVStore) SetAtomicWithRetries(_ string, _ func([]byte) ([]byte, error)) error {
	return nil
}

func TestListAllKeysWithPrefix(t *testing.T) {
	t.Run("matches beyond the first raw page are still found", func(t *testing.T) {
		// Construct a keyspace where the target prefix's matches are sparse in early
		// raw pages and dense in a later one - this is the exact regression the raw-page
		// paging design exists to catch. A batchSize of 10 with matches concentrated at
		// the end reproduces "few matches in this raw page" without "no more pages".
		store := &rawPageKVStore{}
		for i := range 50 {
			store.keys = append(store.keys, fmt.Sprintf("other_key_%02d", i))
		}
		var expected []string
		for i := range 15 {
			key := fmt.Sprintf("matrix_user_srv1_alice%02d", i)
			store.keys = append(store.keys, key)
			expected = append(expected, key)
		}

		matches, err := ListAllKeysWithPrefix(store, "matrix_user_", 10)
		require.NoError(t, err)

		sort.Strings(matches)
		sort.Strings(expected)
		assert.Equal(t, expected, matches)
	})

	t.Run("no matches returns an empty, not nil-vs-error, result", func(t *testing.T) {
		store := &rawPageKVStore{keys: []string{"a", "b", "c"}}
		matches, err := ListAllKeysWithPrefix(store, "nonexistent_", 10)
		require.NoError(t, err)
		assert.Empty(t, matches)
	})

	t.Run("exact multiple of batch size does not loop forever", func(t *testing.T) {
		store := &rawPageKVStore{}
		for i := range 20 {
			store.keys = append(store.keys, fmt.Sprintf("matrix_user_srv1_u%02d", i))
		}

		matches, err := ListAllKeysWithPrefix(store, "matrix_user_", 10)
		require.NoError(t, err)
		assert.Len(t, matches, 20)
	})
}

func TestListAllKeysByPrefix(t *testing.T) {
	store := &rawPageKVStore{}
	for i := range 30 {
		store.keys = append(store.keys, fmt.Sprintf("noise_%02d", i))
	}
	store.keys = append(store.keys, "matrix_user_srv1_a", "matrix_user_srv1_b")
	store.keys = append(store.keys, "ghost_user_srv1_x")

	result, err := ListAllKeysByPrefix(store, 10, "matrix_user_", "ghost_user_", "room_mapping_")
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"matrix_user_srv1_a", "matrix_user_srv1_b"}, result["matrix_user_"])
	assert.ElementsMatch(t, []string{"ghost_user_srv1_x"}, result["ghost_user_"])
	assert.Empty(t, result["room_mapping_"])
}
