// Package kvstore provides a key-value store interface for plugin data persistence.
package kvstore

// KVStore provides an interface for key-value storage operations.
type KVStore interface {
	// Define your methods here. This package is used to access the KVStore pluginapi methods.
	GetTemplateData(userID string) (string, error)
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
	// SetAtomicWithRetries reads the current value for key, passes it to valueFunc
	// to compute the new value, and writes it back using compare-and-set semantics,
	// retrying on conflict. This makes a read-modify-write on a shared key safe
	// against concurrent writers. valueFunc receives nil when the key is absent.
	SetAtomicWithRetries(key string, valueFunc func(oldValue []byte) (newValue []byte, err error)) error
	Delete(key string) error
	ListKeys(page, perPage int) ([]string, error)
	ListKeysWithPrefix(page, perPage int, prefix string) ([]string, error)
}
