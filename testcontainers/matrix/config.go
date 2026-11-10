// Package matrix provides testcontainer utilities for Matrix server testing
package matrix

import "github.com/testcontainers/testcontainers-go"

const defaultAppServiceURL = "http://localhost:8080"

// MatrixTestConfig contains configuration for Matrix test setup
//
//nolint:revive // MatrixTestConfig is intentionally named to be descriptive in test context
type MatrixTestConfig struct {
	ServerName string
	ASToken    string
	HSToken    string

	// Network attaches Synapse to a shared Docker network so other containers can reach it.
	Network *testcontainers.DockerNetwork
	// NetworkAlias is Synapse's hostname on Network. Defaults to "synapse"; ignored without Network.
	NetworkAlias string
	// AppServiceURL is where Synapse delivers Application Service transactions.
	// Defaults to http://localhost:8080.
	AppServiceURL string
}

// DefaultMatrixConfig returns a default test configuration
func DefaultMatrixConfig() MatrixTestConfig {
	return MatrixTestConfig{
		ServerName: "test.matrix.local",
		ASToken:    "test_as_token_12345",
		HSToken:    "test_hs_token_67890",
	}
}
