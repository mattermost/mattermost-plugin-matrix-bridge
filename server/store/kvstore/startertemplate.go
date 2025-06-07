package kvstore

import (
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/pkg/errors"
)

// We expose our calls to the KVStore pluginapi methods through this interface for testability and stability.
// This allows us to better control which values are stored with which keys.

type Client struct {
	client *pluginapi.Client
}

func NewKVStore(client *pluginapi.Client) KVStore {
	return Client{
		client: client,
	}
}

// Sample method to get a key-value pair in the KV store
func (kv Client) GetTemplateData(userID string) (string, error) {
	var templateData string
	err := kv.client.KV.Get("template_key-"+userID, &templateData)
	if err != nil {
		return "", errors.Wrap(err, "failed to get template data")
	}
	return templateData, nil
}

func (kv Client) Get(key string) ([]byte, error) {
	var data []byte
	err := kv.client.KV.Get(key, &data)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get key from KV store")
	}
	return data, nil
}

func (kv Client) Set(key string, value []byte) error {
	_, appErr := kv.client.KV.Set(key, value)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to set key in KV store")
	}
	return nil
}

func (kv Client) Delete(key string) error {
	appErr := kv.client.KV.Delete(key)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to delete key from KV store")
	}
	return nil
}

func (kv Client) ListKeys(page, perPage int) ([]string, error) {
	keys, appErr := kv.client.KV.ListKeys(page, perPage)
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to list keys from KV store")
	}
	return keys, nil
}
