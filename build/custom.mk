# Include custom targets and environment variables here

## Runs the whole Go test suite, including container-backed e2e tests (requires Docker).
.PHONY: e2e
e2e: dist install-go-tools
	E2E_PLUGIN_BUNDLE=$(abspath dist/$(BUNDLE_NAME)) $(GOBIN)/gotestsum -- -v -count=1 -p 2 -timeout=45m ./...
