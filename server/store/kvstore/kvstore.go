// Package kvstore provides a key-value store interface for plugin data persistence.
package kvstore

// KVStore provides an interface for key-value storage operations.
type KVStore interface {
	// Define your methods here. This package is used to access the KVStore pluginapi methods.
	GetTemplateData(userID string) (string, error)
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
	Delete(key string) error
	ListKeys(page, perPage int) ([]string, error)
	ListKeysWithPrefix(page, perPage int, prefix string) ([]string, error)
}
