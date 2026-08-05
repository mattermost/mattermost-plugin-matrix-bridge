package kvstore

import (
	"strings"

	"github.com/pkg/errors"
)

// ListAllKeysWithPrefix pages through the raw keyspace and returns every key matching
// prefix.
//
// pluginapi's ListKeysWithPrefix fetches an unfiltered page of size batchSize from the
// underlying store and filters it, returning only the filtered subset - the raw page's
// actual length is never visible to the caller. A short *filtered* result therefore
// means "few matches in this raw page", not "no more pages": paging on the filtered
// result's length silently stops after page 0 whenever matches are sparse relative to
// the rest of the keyspace. The only reliable end-of-keyspace signal is the raw page
// length, so this pages the unfiltered keyspace with ListKeys and filters client-side
// itself (delegating to ListAllKeysByPrefix, which does exactly that).
func ListAllKeysWithPrefix(store KVStore, prefix string, batchSize int) ([]string, error) {
	byPrefix, err := ListAllKeysByPrefix(store, batchSize, prefix)
	if err != nil {
		return nil, err
	}
	return byPrefix[prefix], nil
}

// ListAllKeysByPrefix does a single pass over the entire unfiltered keyspace and buckets
// every key by whichever of prefixes it matches, instead of doing one prefix-filtered
// scan per prefix (which would re-read the whole keyspace once per prefix).
func ListAllKeysByPrefix(store KVStore, batchSize int, prefixes ...string) (map[string][]string, error) {
	if batchSize <= 0 {
		return nil, errors.Errorf("batchSize must be positive, got %d", batchSize)
	}

	result := make(map[string][]string, len(prefixes))
	for _, prefix := range prefixes {
		result[prefix] = nil
	}

	page := 0
	for {
		keys, err := store.ListKeys(page, batchSize)
		if err != nil {
			return nil, errors.Wrap(err, "failed to list keys")
		}

		for _, key := range keys {
			for _, prefix := range prefixes {
				if strings.HasPrefix(key, prefix) {
					result[prefix] = append(result[prefix], key)
					break
				}
			}
		}

		if len(keys) < batchSize {
			break
		}

		page++
	}

	return result, nil
}
