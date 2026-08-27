package main

import (
	"strconv"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

type faultyKVStore struct {
	kvstore.KVStore
	getErr    map[string]error
	deleteErr map[string]error
	listErr   error
}

func (f *faultyKVStore) Get(key string) ([]byte, error) {
	if err := f.getErr[key]; err != nil {
		return nil, err
	}
	return f.KVStore.Get(key)
}

func (f *faultyKVStore) Delete(key string) error {
	if err := f.deleteErr[key]; err != nil {
		return err
	}
	return f.KVStore.Delete(key)
}

func (f *faultyKVStore) ListKeys(page, perPage int) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.KVStore.ListKeys(page, perPage)
}

func newFaultyKVStore(inner kvstore.KVStore) *faultyKVStore {
	return &faultyKVStore{
		KVStore:   inner,
		getErr:    map[string]error{},
		deleteErr: map[string]error{},
	}
}

func preMultiServerRecords() map[string][]byte {
	return map[string][]byte{
		kvstore.KeyLegacyStoreVersion:            []byte("2"),
		"matrix_user_@alice:example.com":         []byte("mmuserid1"),
		"mattermost_user_mmuserid1":              []byte("@alice:example.com"),
		"channel_mapping_channel1":               []byte("!room:example.com"),
		"room_mapping_!room:example.com":         []byte("channel1"),
		"ghost_user_mmuserid1":                   []byte("@_mattermost_mmuserid1:example.com"),
		"ghost_room_mmuserid1_!room:example.com": []byte("true"),
		"matrix_event_post_$event1:example.com":  []byte("postid1"),
		"matrix_reaction_$reaction1:example.com": []byte(`{"emoji":"+1"}`),
		"dm_mapping_dmchannel1":                  []byte("!dmroom:example.com"),
		"matrix_dm_mapping_!dmroom:example.com":  []byte("dmchannel1"),
	}
}

func seedRecords(t *testing.T, store kvstore.KVStore, records map[string][]byte) {
	t.Helper()
	for key, value := range records {
		require.NoError(t, store.Set(key, value))
	}
}

func schemaVersion(t *testing.T, store kvstore.KVStore) string {
	t.Helper()
	data, err := store.Get(kvstore.KeySchemaVersion)
	require.NoError(t, err)
	return string(data)
}

func TestRunKVStoreMigrations(t *testing.T) {
	t.Run("fresh install stamps the marker and deletes nothing", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()

		require.NoError(t, plugin.runKVStoreMigrations())

		assert.Equal(t, strconv.Itoa(kvstore.CurrentKVStoreVersion), schemaVersion(t, plugin.kvstore))
	})

	t.Run("purges every pre-multi-server record and its marker", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		records := preMultiServerRecords()
		seedRecords(t, plugin.kvstore, records)

		require.NoError(t, plugin.runKVStoreMigrations())

		for key := range records {
			value, err := plugin.kvstore.Get(key)
			require.NoError(t, err)
			assert.Empty(t, value, "%q must not survive the purge", key)
		}
		assert.Equal(t, strconv.Itoa(kvstore.CurrentKVStoreVersion), schemaVersion(t, plugin.kvstore))
	})

	t.Run("never deletes the server registry", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		seedRecords(t, plugin.kvstore, preMultiServerRecords())
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, []byte(`[{"server_id":"abc"}]`)))

		require.NoError(t, plugin.runKVStoreMigrations())

		registry, err := plugin.kvstore.Get(kvstore.KeyServersConfig)
		require.NoError(t, err)
		assert.Equal(t, `[{"server_id":"abc"}]`, string(registry))
	})

	t.Run("is a no-op once the marker is current", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		require.NoError(t, plugin.kvstore.Set(kvstore.KeySchemaVersion, []byte(strconv.Itoa(kvstore.CurrentKVStoreVersion))))
		live, err := buildSingleChannelMapping("serverA", "!room:example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), live))

		require.NoError(t, plugin.runKVStoreMigrations())

		stored, err := plugin.kvstore.Get(kvstore.BuildChannelMappingKey("channel1"))
		require.NoError(t, err)
		assert.Equal(t, live, stored, "an already-migrated store must not be purged")
	})

	t.Run("a failed delete leaves the marker unset", func(t *testing.T) {
		plugin := setupPluginForTest()
		store := newFaultyKVStore(NewMemoryKVStore())
		plugin.kvstore = store
		seedRecords(t, store, preMultiServerRecords())
		store.deleteErr["room_mapping_!room:example.com"] = errors.New("boom")

		err := plugin.runKVStoreMigrations()

		require.Error(t, err)
		assert.Empty(t, schemaVersion(t, store))
	})

	t.Run("a failed keyspace scan leaves the marker unset", func(t *testing.T) {
		plugin := setupPluginForTest()
		store := newFaultyKVStore(NewMemoryKVStore())
		plugin.kvstore = store
		seedRecords(t, store, preMultiServerRecords())
		store.listErr = errors.New("boom")

		err := plugin.runKVStoreMigrations()

		require.Error(t, err)
		assert.Empty(t, schemaVersion(t, store))
	})

	t.Run("an unreadable marker fails instead of purging", func(t *testing.T) {
		plugin := setupPluginForTest()
		store := newFaultyKVStore(NewMemoryKVStore())
		plugin.kvstore = store
		seedRecords(t, store, preMultiServerRecords())
		store.getErr[kvstore.KeySchemaVersion] = errors.New("boom")

		err := plugin.runKVStoreMigrations()

		require.Error(t, err)
		survivor, getErr := store.Get("channel_mapping_channel1")
		require.NoError(t, getErr)
		assert.NotEmpty(t, survivor, "a marker that could not be read must not license a purge")
	})

	t.Run("a non-numeric marker fails instead of purging", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		seedRecords(t, plugin.kvstore, preMultiServerRecords())
		require.NoError(t, plugin.kvstore.Set(kvstore.KeySchemaVersion, []byte("not-a-number")))

		err := plugin.runKVStoreMigrations()

		require.Error(t, err)
		survivor, getErr := plugin.kvstore.Get("channel_mapping_channel1")
		require.NoError(t, getErr)
		assert.NotEmpty(t, survivor, "an unparseable marker must not license a purge")
	})

	t.Run("retries the purge after a failed run", func(t *testing.T) {
		plugin := setupPluginForTest()
		store := newFaultyKVStore(NewMemoryKVStore())
		plugin.kvstore = store
		seedRecords(t, store, preMultiServerRecords())
		store.deleteErr["room_mapping_!room:example.com"] = errors.New("boom")
		require.Error(t, plugin.runKVStoreMigrations())

		delete(store.deleteErr, "room_mapping_!room:example.com")
		require.NoError(t, plugin.runKVStoreMigrations())

		survivor, err := store.Get("room_mapping_!room:example.com")
		require.NoError(t, err)
		assert.Empty(t, survivor)
		assert.Equal(t, strconv.Itoa(kvstore.CurrentKVStoreVersion), schemaVersion(t, store))
	})
}
