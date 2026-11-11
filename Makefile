# SPDX-License-Identifier: AGPL-3.0-only
# Run `make` to see the targets.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --no-builtin-rules --no-print-directory

# Whatever a target starts asks nothing beyond this machine: no STUN server and no
# metadata service (ADR 0034). make run and make dev are a real server.
export DOCKWELL_TEST := 1
run dev: export DOCKWELL_TEST :=

GOTOOL    := go tool -modfile=tools/go.mod
# The two builds of dockwell (docs/adr/0023-a-full-build-and-an-agent-build.md):
# the full build, which the control server runs, and the agent build, built
# with -tags agent, which every server that joins runs. Both are called
# dockwell on a server.
BIN       := bin/dockwell
AGENT_BIN := bin/dockwell-agent
# The packages the agent build links, which vet, lint and test again with its
# tag. go list finds them once per make run, the first time a target needs
# them: that expansion makes AGENT_PKGS simply expanded, holding what it found.
# A make run that needs none, such as make build, never runs it.
AGENT_PKGS = $(eval AGENT_PKGS := $$(shell scripts/agent-packages.sh))$(AGENT_PKGS)
GENERATED := gen packages/api/src/gen internal/cli/apicmd internal/cli/apinames internal/control/apidoc/openapi.yaml packages/ui/src/tokens.ts packages/ui/src/styles/tokens.css web/src/routeTree.gen.ts
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

.PHONY: build-agent
build-agent: ## Build the agent build, which every server that joins runs, as bin/dockwell-agent
	CGO_ENABLED=0 go build -trimpath -tags agent -o $(AGENT_BIN) ./cmd/dockwell

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
# protoc-gen-openapi writes a route that streams bytes as JSON: this says what it
# carries, and writes those routes for the TypeScript client (ADR 0045).
	go run ./scripts/openapistreams $(OPENAPI) packages/api/src/gen/streams.ts
# The control plane serves the OpenAPI document, and go:embed can't reach gen/.
	cp $(OPENAPI) internal/control/apidoc/openapi.yaml
	pnpm --filter @dockwell/ui tokens

.PHONY: test
test: deps ## Run the Go and TypeScript tests, the agent build's with its tag too
	go test -timeout 20m -race -coverprofile=coverage.out ./...
	go test -race -tags agent $(AGENT_PKGS)
	pnpm -r --if-present test

.PHONY: lint
lint: deps ## Lint Go, the schema and its OpenAPI document, TypeScript, CSS, shell scripts, translations, license headers and doc links
	go mod tidy -diff
	cd tools && go mod tidy -diff
	go vet ./...
	go vet -tags agent $(AGENT_PKGS)
	$(GOTOOL) golangci-lint run
	$(GOTOOL) golangci-lint run --build-tags agent $(AGENT_PKGS)
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

.PHONY: adrs
adrs: ## List the architecture decision records, with their titles and statuses
	@scripts/adrs.sh

.PHONY: i18n-merge
i18n-merge: deps ## Merge the translation catalogs after a merge or rebase, conflicted or not; after one that finished, FROM=ORIG_HEAD
	pnpm --filter @dockwell/web i18n:merge $(if $(FROM),--from $(FROM))

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
smoke: build build-agent ## Start the built binaries: the full build serves the API and the panel, and the agent build joins it
	scripts/smoke.sh $(BIN)
	scripts/smoke-agent.sh $(BIN) $(AGENT_BIN)

.PHONY: e2e
e2e: build ## Claim, sign in, run the panel's flows, follow logs, walk a server's files, read its updates, use its terminal and invite someone in headless Chromium, with a virtual passkey and a fake Docker Engine (SHOTS=dir saves screenshots)
	go build -o bin/fakedocker ./scripts/fakedocker
	node web/e2e/sign-in.ts $(BIN) --fake-docker bin/fakedocker $(if $(SHOTS),--shots $(SHOTS))
	node web/e2e/logs.ts $(BIN) $(if $(SHOTS),--shots $(SHOTS))
	node web/e2e/files.ts $(BIN) $(if $(SHOTS),--shots $(SHOTS))
	node web/e2e/updates.ts $(BIN) $(if $(SHOTS),--shots $(SHOTS))
	node web/e2e/terminal.ts $(BIN) --fake-docker bin/fakedocker $(if $(SHOTS),--shots $(SHOTS))
	node web/e2e/invites.ts $(BIN) $(if $(SHOTS),--shots $(SHOTS))

.PHONY: chaos
chaos: build build-agent ## Break a change and local reconciliation, then check recovery on Docker (CHAOS_FAKE_DOCKER=1 uses the fake Engine)
	scripts/chaos.sh $(BIN) $(AGENT_BIN)

.PHONY: docker-panel-e2e
docker-panel-e2e: build ## Create, deploy, roll back and export an app in the panel on this machine's Docker, in headless Chromium
	node web/e2e/apps.ts $(BIN)

.PHONY: containers-load
containers-load: build-go build-agent ## Measure an agent's memory on a server of hundreds of containers, on a fake Docker
	scripts/containers-load.sh $(BIN) $(AGENT_BIN)

.PHONY: logs-load
logs-load: build-go build-agent ## Measure an agent's memory while a container floods its log, on a fake Docker
	scripts/logs-load.sh $(BIN) $(AGENT_BIN)

.PHONY: transfers-load
transfers-load: build-go build-agent ## Measure an agent's memory while four files upload and four download at once
	scripts/transfers-load.sh $(BIN) $(AGENT_BIN)

.PHONY: forwards-load
forwards-load: build-go build-agent ## Measure an agent's memory while four apps opened through the panel carry 16 connections, on a fake Docker
	scripts/forwards-load.sh $(BIN) $(AGENT_BIN)

.PHONY: docker-e2e
docker-e2e: build-go build-agent ## Run, change, undo and export a service through the CLI on this machine's Docker, import a Compose file, and run a container through the agent build
	scripts/docker-e2e.sh $(BIN) $(AGENT_BIN)

.PHONY: catalog-e2e
catalog-e2e: build-go ## Install, check, upgrade and remove catalog apps on this machine's Docker, as APPS="pgadmin redis" names them
	@test -n "$(APPS)" || { echo 'catalog-e2e: name the apps to try, such as make catalog-e2e APPS="pgadmin redis"' >&2; exit 1; }
	scripts/catalog-e2e.sh $(BIN) $(APPS)

.PHONY: budgets
budgets: build build-agent ## Fail if the builds are over their idle memory budgets; BASELINE=other/dockwell and BASELINE_AGENT=other/dockwell-agent measure those too (about 7 minutes)
	scripts/budgets.sh $(BIN) $(AGENT_BIN)

.PHONY: check
check: lint test check-generated check-breaking smoke ## Run everything CI runs, except the Install workflow

.PHONY: release-check
release-check: ## Validate the release configuration
	$(GOTOOL) goreleaser check

.PHONY: release-snapshot
release-snapshot: deps ## Build release archives into dist/, as CI does for main (SBOMs need syft)
	$(GOTOOL) goreleaser release --snapshot --clean $(if $(shell command -v syft),,--skip=sbom)

# The install script's tests (scripts/install-test/): a snapshot and an older
# build, laid out and signed as releases, installed in stock containers. The
# older build stands in for a release from before the agent build: it has
# only the full build, linked to name no build, as those releases do.
INSTALL_TEST_IMAGES ?= debian:13 ubuntu:26.04 almalinux:9 alpine:3.24
COSIGN ?= cosign

.PHONY: install-test-releases
install-test-releases: release-snapshot ## Lay out signed test releases for the install script in dist/install-test (needs cosign v3)
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w -X github.com/dockwell/dockwell/internal/buildinfo.version=v0.0.0-test -X github.com/dockwell/dockwell/internal/buildinfo.edition=" \
			-o dist/old/linux_$$arch/dockwell ./cmd/dockwell; \
	done
	scripts/install-test/releases.sh dist dist/old dist/install-test $(COSIGN)

.PHONY: install-test
install-test: install-test-releases ## Test the install script in Debian, Ubuntu, AlmaLinux and Alpine containers (needs Docker and cosign v3)
	scripts/install-test/test.sh dist/install-test "$$(command -v $(COSIGN))" $(INSTALL_TEST_IMAGES)

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist internal/panel/dist/app
