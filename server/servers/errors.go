package servers

import "errors"

// Sentinel errors returned by the registry. Named without a redundant "servers"
// prefix since callers always qualify them (servers.ErrNotRegistered). Wrapped with
// errors.Wrap at their call sites while keeping the original message text, so
// errors.Is still matches after the error has travelled out of a CAS callback
// (through kvstore.SetAtomicWithRetries) and command output does not change.
var (
	// ErrNotRegistered is returned when a lookup or mutation targets a server_id that
	// is not (or no longer) present in the registry.
	ErrNotRegistered = errors.New("server is not registered")

	// ErrEndpointTaken is returned when the normalized endpoint (host:port) of a
	// server being added or edited is already used by a different entry.
	ErrEndpointTaken = errors.New("a server is already registered at this endpoint")

	// ErrNameTaken is returned when a server's resolved ServerName collides with a
	// different entry's.
	ErrNameTaken = errors.New("server name conflicts with an existing server")

	// ErrIDTaken is returned when a caller-supplied server_id (re-adoption) collides
	// with a live entry.
	ErrIDTaken = errors.New("server ID is already registered")

	// ErrMigratedImmutable is returned by Remove for an entry migrated from the
	// legacy single-server configuration (SiteURL == ""), which cannot be
	// re-registered with the same shared-channels remote identity.
	ErrMigratedImmutable = errors.New("server was migrated from the legacy configuration and cannot be removed")

	// ErrInvalidInput is returned for malformed input the registry itself validates
	// (a malformed server_id, an empty token on Update, etc).
	ErrInvalidInput = errors.New("invalid server input")
)
