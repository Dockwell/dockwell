# SPDX-License-Identifier: AGPL-3.0-only
# Run `make` to see the targets.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --no-builtin-rules --no-print-directory

GOTOOL    := go tool -modfile=tools/go.mod
BIN       := bin/dockwell
GENERATED := gen packages/api/src/gen packages/ui/src/tokens.ts packages/ui/src/styles/tokens.css web/src/routeTree.gen.ts

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
	@trap 'kill 0' EXIT; $(BIN) run --log-level debug & pnpm --filter @dockwell/web dev

.PHONY: generate
generate: deps ## Regenerate code from proto/, and the design tokens
	$(GOTOOL) buf generate
	pnpm --filter @dockwell/ui tokens

.PHONY: test
test: deps ## Run the Go and TypeScript tests
	go test -race ./...
	pnpm -r --if-present test

.PHONY: lint
lint: deps ## Lint Go, the schema, TypeScript, CSS, translations, license headers and doc links
	go mod tidy -diff
	cd tools && go mod tidy -diff
	go vet ./...
	$(GOTOOL) golangci-lint run
	$(GOTOOL) golangci-lint fmt --diff
	$(GOTOOL) buf lint
	$(GOTOOL) buf format --diff --exit-code
	pnpm exec biome ci .
	pnpm -r --if-present typecheck
	pnpm --filter @dockwell/ui tokens:check
	pnpm --filter @dockwell/web i18n:check
	scripts/check-spdx.sh
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

.PHONY: check-breaking
check-breaking: ## Fail if the API schema breaks compatibility with main
	@if git cat-file -e main:proto 2>/dev/null; then \
		$(GOTOOL) buf breaking --against '.git#branch=main'; \
	else \
		echo "main has no schema yet, so there is nothing to break."; \
	fi

.PHONY: smoke
smoke: build ## Start the built binary and check it serves the API and the panel
	scripts/smoke.sh $(BIN)

.PHONY: check
check: lint test check-generated check-breaking smoke ## Run everything CI runs

.PHONY: clean
clean: ## Remove build output
	rm -rf bin internal/panel/dist/app
