# SPDX-License-Identifier: AGPL-3.0-only
# Run `make` to see the targets.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --no-builtin-rules --no-print-directory

GOTOOL    := go tool -modfile=tools/go.mod
BIN       := bin/dockwell
GENERATED := gen packages/api/src/gen internal/control/apidoc/openapi.yaml packages/ui/src/tokens.ts packages/ui/src/styles/tokens.css web/src/routeTree.gen.ts
# The OpenAPI document for the REST paths (docs/adr/0015-rest-and-openapi-from-the-schema.md).
OPENAPI   := gen/openapi/openapi.yaml

.PHONY: help
help: ## Show the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

node_modules/.modules.yaml: package.json pnpm-lock.yaml pnpm-workspace.yaml $(wildcard packages/*/package.json web/package.json)
	pnpm install --frozen-lockfile
	@touch $@

.PHONY: deps
deps: node_modules/.modules.yaml ## Install the JavaScript dependencies

.PHONY: web
web: deps ## Build the panel into internal/panel/dist/app
	pnpm --filter @dockwell/web build

.PHONY: build
build: web build-go ## Build bin/dockwell with the panel built in

.PHONY: build-go
build-go: ## Build bin/dockwell with whatever panel build is already there
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/dockwell

.PHONY: run
run: build ## Build everything and run Dockwell on 127.0.0.1:7600
	$(BIN) run --log-level debug

.PHONY: dev
dev: deps build-go ## Run the API on :7600 and the panel with hot reload on :5173
	@$(BIN) run --log-level debug & api=$$!; \
	trap 'kill $$api 2>/dev/null; wait $$api 2>/dev/null' EXIT; \
	pnpm --filter @dockwell/web dev

.PHONY: mocks
mocks: deps ## Browse the screen mocks in design/mocks on 127.0.0.1:5174
	pnpm --filter @dockwell/mockview dev

.PHONY: mock-snapshots
mock-snapshots: deps ## Write PNGs of every mock, light and dark, to packages/mockview/snapshots
	pnpm --filter @dockwell/mockview snapshots $(if $(ONLY),$(foreach o,$(ONLY),--only $(o)))

.PHONY: generate
generate: deps ## Regenerate code and the OpenAPI document from proto/, and the design tokens
	$(GOTOOL) buf generate
# buf empties gen/ first, so the schema's license goes back in beside the code.
# buf can't leave a file in an output directory it cleans, which is why buf
# generate on its own deletes gen/LICENSE; scripts/check-spdx.sh names it then.
	cp proto/LICENSE gen/LICENSE
# The TypeScript generated from googleapis carries Google's Apache-2.0 notice
# but no SPDX line, which every source file has (ADR 0012); it goes on top.
	find packages/api/src/gen/google -name '*.ts' -print0 | while IFS= read -r -d '' f; do \
		if ! grep -q 'SPDX-License-Identifier' "$$f"; then \
			{ printf '// SPDX-License-Identifier: Apache-2.0\n\n'; cat "$$f"; } >"$$f.spdx" && mv "$$f.spdx" "$$f"; \
		fi; \
	done
# The control plane serves the OpenAPI document, and go:embed can't reach gen/.
	cp $(OPENAPI) internal/control/apidoc/openapi.yaml
	pnpm --filter @dockwell/ui tokens

.PHONY: test
test: deps ## Run the Go and TypeScript tests
	go test -race ./...
	pnpm -r --if-present test

.PHONY: lint
lint: deps ## Lint Go, the schema and its OpenAPI document, TypeScript, CSS, shell scripts, translations, license headers and doc links
	go mod tidy -diff
	cd tools && go mod tidy -diff
	go vet ./...
	$(GOTOOL) golangci-lint run
	$(GOTOOL) golangci-lint fmt --diff
	$(GOTOOL) buf lint
	$(GOTOOL) buf format --diff --exit-code
	$(MAKE) check-openapi
	pnpm exec biome ci .
	pnpm -r --if-present typecheck
	pnpm --filter @dockwell/ui tokens:check
	pnpm --filter @dockwell/web i18n:check
	scripts/check-spdx.sh
	@if command -v shellcheck >/dev/null; then git ls-files -z '*.sh' | xargs -0 shellcheck; \
	else echo "shellcheck isn't installed, so the shell scripts weren't linted. CI lints them."; fi
	go run ./scripts/checklinks

.PHONY: fmt
fmt: deps ## Format Go, proto, TypeScript and CSS
	$(GOTOOL) golangci-lint fmt
	$(GOTOOL) buf format --write
	pnpm exec biome check --write .

.PHONY: i18n
i18n: deps ## Extract new messages into the translation catalogs
	pnpm --filter @dockwell/web i18n:extract

.PHONY: check-generated
check-generated: generate web ## Fail if generated code is out of date
	@if [ -n "$$(git status --porcelain -- $(GENERATED))" ]; then \
		git status --short -- $(GENERATED); \
		echo "Generated code is out of date. Run make generate and make web, then commit the result." >&2; \
		exit 1; \
	fi

.PHONY: check-openapi
check-openapi: ## Fail if the OpenAPI document isn't valid OpenAPI 3
	$(GOTOOL) validate --multi $(OPENAPI)
	@echo "$(OPENAPI) is valid OpenAPI 3."

# The schema is compared with main upstream, not a local branch that may be stale.
BREAKING_BASE ?= origin/main

.PHONY: check-breaking
check-breaking: ## Fail if the API schema breaks compatibility with origin/main
	@base=$$(git rev-parse --verify --quiet "$(BREAKING_BASE)^{commit}" || true); \
	if [ -z "$$base" ]; then \
		echo "Can't find $(BREAKING_BASE) to compare the schema with. Run git fetch origin main, or set BREAKING_BASE." >&2; \
		exit 1; \
	elif git cat-file -e "$$base:proto" 2>/dev/null; then \
		$(GOTOOL) buf breaking --against ".git#ref=$$base"; \
		echo "The schema is compatible with $(BREAKING_BASE) at $$(git rev-parse --short $$base)."; \
	else \
		echo "$(BREAKING_BASE) has no schema yet, so there is nothing to break."; \
	fi

.PHONY: smoke
smoke: build ## Start the built binary and check it serves the API and the panel
	scripts/smoke.sh $(BIN)

.PHONY: e2e
e2e: build ## Claim, sign out and sign in again in headless Chromium with a virtual passkey (SHOTS=dir saves screenshots)
	node web/e2e/sign-in.ts $(BIN) $(if $(SHOTS),--shots $(SHOTS))

.PHONY: budgets
budgets: build ## Fail if the binary is over its idle memory budgets; BASELINE=other/dockwell measures that too (about 7 minutes)
	scripts/budgets.sh $(BIN)

.PHONY: check
check: lint test check-generated check-breaking smoke ## Run everything CI runs, except the Install workflow

.PHONY: release-check
release-check: ## Validate the release configuration
	$(GOTOOL) goreleaser check

.PHONY: release-snapshot
release-snapshot: deps ## Build release archives into dist/, as CI does for main (SBOMs need syft)
	$(GOTOOL) goreleaser release --snapshot --clean $(if $(shell command -v syft),,--skip=sbom)

# The install script's tests (scripts/install-test/): a snapshot and an older
# build, laid out and signed as releases, installed in stock containers.
INSTALL_TEST_IMAGES ?= debian:13 ubuntu:26.04 almalinux:9 alpine:3.24
COSIGN ?= cosign

.PHONY: install-test-releases
install-test-releases: release-snapshot ## Lay out signed test releases for the install script in dist/install-test (needs cosign v3)
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w -X github.com/dockwell/dockwell/internal/buildinfo.version=v0.0.0-test" \
			-o dist/old/linux_$$arch/dockwell ./cmd/dockwell; \
	done
	scripts/install-test/releases.sh dist dist/old dist/install-test $(COSIGN)

.PHONY: install-test
install-test: install-test-releases ## Test the install script in Debian, Ubuntu, AlmaLinux and Alpine containers (needs Docker and cosign v3)
	scripts/install-test/test.sh dist/install-test "$$(command -v $(COSIGN))" $(INSTALL_TEST_IMAGES)

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist internal/panel/dist/app
