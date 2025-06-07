package kvstore

type KVStore interface {
	// Define your methods here. This package is used to access the KVStore pluginapi methods.
	GetTemplateData(userID string) (string, error)
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
	Delete(key string) error
	ListKeys(page, perPage int) ([]string, error)
}
