# dp1-feed-v2 — local development helpers
# Requires: Go 1.24+ (module uses tool/mockgen), Docker for integration tests & compose.
# Lint: golangci-lint v2 on PATH (https://golangci-lint.run/welcome/install/);
#       markdown: npx markdownlint-cli2 (same as .github/workflows/lint.yaml)

GOLANGCI_LINT ?= golangci-lint

.PHONY: help
help:
	@echo "Targets:"
	@echo "  make lint              - golangci-lint (see .golangci.yml) + markdownlint-cli2 on *.md"
	@echo "  make lint-fix          - golangci-lint run --fix (format + safe fixes)"
	@echo "  make test              - unit tests (all packages, -race)"
	@echo "  make test-integration  - store contract tests (Docker + -tags=integration)"
	@echo "  make test-coverage     - unit + integration tests with merged coverage"
	@echo "  make verify            - lint + test + test-integration (full local gate)"
	@echo "  make check             - lint + test (no Docker)"
	@echo "  make generate          - go generate ./... (mocks)"
	@echo "  make build             - build API server to bin/server"
	@echo "  make run               - go run server (config + migrations)"
	@echo "  make fmt               - go fmt ./..."
	@echo "  make vet               - go vet ./..."
	@echo "  make tidy              - go mod tidy"
	@echo "  make docker-up         - docker compose up -d postgres"
	@echo "  make docker-down       - docker compose down"
	@echo "  make clean             - remove bin/"
	@echo ""
	@echo "GitHub Actions (local with act):"
	@echo "  make act-lint          - run lint workflow locally"
	@echo "  make act-test          - run test workflow locally (unit + integration + merged coverage)"
	@echo "  make act-all           - run all workflows locally"
	@echo "  make act-list          - list all available workflows"
	@echo "  make act-clean         - clean up act containers, volumes, and artifacts"

CONFIG      ?= config/config.yaml
MIGRATIONS  ?= db/migrations
BIN_DIR     ?= bin
SERVER_BIN  ?= $(BIN_DIR)/server

.PHONY: lint
lint:
	@$(GOLANGCI_LINT) version >/dev/null 2>&1 || { echo "golangci-lint not found; install: https://golangci-lint.run/welcome/install/"; exit 1; }
	$(GOLANGCI_LINT) run ./...
	@command -v npx >/dev/null 2>&1 || { echo "npx not found; install Node.js for markdown lint (https://nodejs.org/)"; exit 1; }
	npx --yes markdownlint-cli2 "**/*.md" "#node_modules"

.PHONY: lint-fix
lint-fix:
	@$(GOLANGCI_LINT) version >/dev/null 2>&1 || { echo "golangci-lint not found; install: https://golangci-lint.run/welcome/install/"; exit 1; }
	$(GOLANGCI_LINT) run ./... --fix

.PHONY: test
test:
	go test ./... -race -count=1

.PHONY: test-integration
test-integration:
	go test -tags=integration -count=1 -v ./internal/store/...

.PHONY: test-coverage
test-coverage:
	@echo "Running unit tests with coverage..."
	@go test -race -count=1 -timeout=5m \
		-coverprofile=coverage-unit.out \
		-covermode=atomic \
		-coverpkg=$$(go list ./... | grep -v '/mocks' | grep -v '/cmd/' | grep -v '/internal/store' | paste -sd, -) \
		./...
	@cat coverage-unit.out | grep -v "/mocks/" | grep -v "/cmd/" | grep -v "/internal/store" > coverage-unit.filtered.out
	@mv coverage-unit.filtered.out coverage-unit.out
	@echo ""
	@echo "Running integration tests with coverage..."
	@go test -tags=integration -count=1 -timeout=10m \
		-coverprofile=coverage-integration.out \
		-covermode=atomic \
		-coverpkg=github.com/display-protocol/dp1-feed-v2/internal/store,github.com/display-protocol/dp1-feed-v2/internal/store/pg \
		./internal/store/
	@echo ""
	@echo "Merging coverage profiles..."
	@echo "mode: atomic" > coverage-merged.out
	@tail -n +2 coverage-unit.out >> coverage-merged.out
	@tail -n +2 coverage-integration.out >> coverage-merged.out
	@echo ""
	@echo "=== Merged Coverage Report ==="
	@go tool cover -func=coverage-merged.out | tail -20
	@echo ""
	@echo "Coverage files: coverage-unit.out, coverage-integration.out, coverage-merged.out"

.PHONY: verify
verify: lint test test-integration

.PHONY: check
check: lint test

.PHONY: generate
generate:
	go generate ./...

.PHONY: build
build:
	mkdir -p $(BIN_DIR)
	go build -o $(SERVER_BIN) ./cmd/server

.PHONY: run
run:
	go run ./cmd/server -config $(CONFIG) -migrations $(MIGRATIONS)

.PHONY: fmt
fmt:
	go fmt ./... && goimports -w -local "github.com/display-protocol/dp1-feed-v2" .

.PHONY: vet
vet:
	go vet ./...

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: docker-up
docker-up:
	docker compose up -d postgres

.PHONY: docker-down
docker-down:
	docker compose down

.PHONY: clean
clean:
	rm -rf $(BIN_DIR)

# =============================================================================
# GitHub Actions - Local Testing with act
# =============================================================================
# Requires: act (https://github.com/nektos/act)
#   Installation: brew install act
#                 or: curl -s https://raw.githubusercontent.com/nektos/act/master/install.sh | sudo bash
#
# Note: act runs workflows in Docker containers, so Docker must be running.
#       Use -P flag to specify runner image: ubuntu-latest=catthehacker/ubuntu:act-latest
#       Artifacts are stored locally in /tmp/act-artifacts to avoid ACTIONS_RUNTIME_TOKEN errors

ACT ?= act
ACT_FLAGS ?= -P ubuntu-latest=catthehacker/ubuntu:act-latest --container-architecture linux/amd64
ACT_ARTIFACT_DIR ?= /tmp/act-artifacts

.PHONY: act-check
act-check:
	@command -v $(ACT) >/dev/null 2>&1 || { \
		echo "❌ act not found. Install from https://github.com/nektos/act"; \
		echo "   macOS: brew install act"; \
		echo "   Linux: curl -s https://raw.githubusercontent.com/nektos/act/master/install.sh | sudo bash"; \
		exit 1; \
	}
	@echo "✓ act is installed: $$($(ACT) --version)"

.PHONY: act-lint
act-lint: act-check
	@echo "Running lint workflow locally..."
	$(ACT) $(ACT_FLAGS) -W .github/workflows/lint.yaml

.PHONY: act-test
act-test: act-check
	@echo "Running test workflow locally (unit + integration + merged coverage)..."
	@echo "⚠️  Note: Integration tests require Docker-in-Docker for testcontainers"
	@mkdir -p $(ACT_ARTIFACT_DIR)
	$(ACT) $(ACT_FLAGS) -W .github/workflows/test.yaml --privileged --artifact-server-path $(ACT_ARTIFACT_DIR)

.PHONY: act-all
act-all: act-check
	@echo "Running all workflows locally..."
	$(ACT) $(ACT_FLAGS) -l
	@echo ""
	@echo "Running lint..."
	$(MAKE) act-lint
	@echo ""
	@echo "Running tests (unit + integration + coverage)..."
	$(MAKE) act-test

.PHONY: act-list
act-list: act-check
	@echo "Available workflows:"
	$(ACT) -l

.PHONY: act-dry-run
act-dry-run: act-check
	@echo "Dry-run of all workflows (shows what would run):"
	$(ACT) $(ACT_FLAGS) -n

.PHONY: act-clean
act-clean:
	@echo "Cleaning up act containers and volumes..."
	@docker ps -a | grep act- | awk '{print $$1}' | xargs -r docker rm -f || true
	@docker volume ls | grep act- | awk '{print $$2}' | xargs -r docker volume rm || true
	@echo "Cleaning up act artifacts..."
	@rm -rf $(ACT_ARTIFACT_DIR)
