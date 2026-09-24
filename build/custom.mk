# Include custom targets and environment variables here

## Runs the whole Go test suite, including container-backed e2e tests (requires Docker).
.PHONY: e2e
e2e: dist install-go-tools
	E2E_PLUGIN_BUNDLE=$(abspath dist/$(BUNDLE_NAME)) $(GOBIN)/gotestsum -- -v -count=1 -p 2 -timeout=45m ./...

## Runs the Playwright System Console tests against a real Mattermost + Synapse (requires Docker).
.PHONY: e2e-ui
e2e-ui: dist e2e/playwright/node_modules
	cd e2e/playwright && $(NPM) exec -- playwright install chromium
	cd e2e/playwright && $(NPM) run check-types
	cd e2e/playwright && E2E_PLUGIN_BUNDLE=$(abspath dist/$(BUNDLE_NAME)) $(NPM) test

e2e/playwright/node_modules: e2e/playwright/package.json e2e/playwright/package-lock.json
	cd e2e/playwright && $(NPM) ci
	touch $@
