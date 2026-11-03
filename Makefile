# SPDX-License-Identifier: AGPL-3.0-only
# Run `make` to see the targets.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --no-builtin-rules --no-print-directory

GOTOOL    := go tool -modfile=tools/go.mod
BIN       := bin/dockwell
GENERATED := gen packages/api/src/gen

.PHONY: help
help: ## Show the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

node_modules/.modules.yaml: package.json pnpm-lock.yaml pnpm-workspace.yaml $(wildcard packages/*/package.json web/package.json)
	pnpm install --frozen-lockfile
	@touch $@

.PHONY: deps
deps: node_modules/.modules.yaml ## Install the JavaScript dependencies

.PHONY: build
build: ## Build bin/dockwell
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/dockwell

.PHONY: run
run: build ## Run Dockwell locally on 127.0.0.1:7600
	$(BIN) run --log-level debug

.PHONY: generate
generate: deps ## Regenerate the Go and TypeScript code from proto/
	$(GOTOOL) buf generate

.PHONY: test
test: ## Run the Go tests with the race detector
	go test -race ./...

.PHONY: lint
lint: deps ## Lint Go, the API schema, TypeScript, license headers and doc links
	go mod tidy -diff
	cd tools && go mod tidy -diff
	go vet ./...
	$(GOTOOL) golangci-lint run
	$(GOTOOL) golangci-lint fmt --diff
	$(GOTOOL) buf lint
	$(GOTOOL) buf format --diff --exit-code
	pnpm -r --if-present typecheck
	scripts/check-spdx.sh
	go run ./scripts/checklinks

.PHONY: fmt
fmt: ## Format Go and proto files
	$(GOTOOL) golangci-lint fmt
	$(GOTOOL) buf format --write

.PHONY: check-generated
check-generated: generate ## Fail if generated code is out of date
	@if [ -n "$$(git status --porcelain -- $(GENERATED))" ]; then \
		git status --short -- $(GENERATED); \
		echo "Generated code is out of date. Run make generate and commit the result." >&2; \
		exit 1; \
	fi

.PHONY: check-breaking
check-breaking: ## Fail if the API schema breaks compatibility with main
	@if git cat-file -e main:proto 2>/dev/null; then \
		$(GOTOOL) buf breaking --against '.git#branch=main'; \
	else \
		echo "main has no schema yet, so there is nothing to break."; \
	fi

.PHONY: check
check: lint test check-generated check-breaking ## Run everything CI runs

.PHONY: clean
clean: ## Remove build output
	rm -rf bin
