// Package matrix provides testcontainer utilities for Matrix server testing
package matrix

// MatrixTestConfig contains configuration for Matrix test setup
//
//nolint:revive // MatrixTestConfig is intentionally named to be descriptive in test context
type MatrixTestConfig struct {
	ServerName string
	ASToken    string
	HSToken    string
}

// DefaultMatrixConfig returns a default test configuration
func DefaultMatrixConfig() MatrixTestConfig {
	return MatrixTestConfig{
		ServerName: "test.matrix.local",
		ASToken:    "test_as_token_12345",
		HSToken:    "test_hs_token_67890",
	}
}
