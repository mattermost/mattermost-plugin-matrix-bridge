package kvstore

import (
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/pkg/errors"
)

// We expose our calls to the KVStore pluginapi methods through this interface for testability and stability.
// This allows us to better control which values are stored with which keys.

// Client wraps the Mattermost plugin API client for KV store operations.
type Client struct {
	client *pluginapi.Client
}

// NewKVStore creates a new KVStore implementation using the provided plugin API client.
func NewKVStore(client *pluginapi.Client) KVStore {
	return Client{
		client: client,
	}
}

// GetTemplateData retrieves template data for a specific user from the KV store.
func (kv Client) GetTemplateData(userID string) (string, error) {
	var templateData string
	err := kv.client.KV.Get("template_key-"+userID, &templateData)
	if err != nil {
		return "", errors.Wrap(err, "failed to get template data")
	}
	return templateData, nil
}

// Get retrieves a value from the KV store by key.
func (kv Client) Get(key string) ([]byte, error) {
	var data []byte
	err := kv.client.KV.Get(key, &data)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get key from KV store")
	}
	return data, nil
}

// Set stores a key-value pair in the KV store.
func (kv Client) Set(key string, value []byte) error {
	_, appErr := kv.client.KV.Set(key, value)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to set key in KV store")
	}
	return nil
}

// Delete removes a key-value pair from the KV store.
func (kv Client) Delete(key string) error {
	appErr := kv.client.KV.Delete(key)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to delete key from KV store")
	}
	return nil
}

// ListKeys retrieves a paginated list of keys from the KV store.
func (kv Client) ListKeys(page, perPage int) ([]string, error) {
	keys, appErr := kv.client.KV.ListKeys(page, perPage)
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to list keys from KV store")
	}
	return keys, nil
}

// ListKeysWithPrefix retrieves a paginated list of keys with a specific prefix from the KV store.
func (kv Client) ListKeysWithPrefix(page, perPage int, prefix string) ([]string, error) {
	keys, appErr := kv.client.KV.ListKeys(page, perPage, pluginapi.WithPrefix(prefix))
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to list keys with prefix from KV store")
	}
	return keys, nil
}
