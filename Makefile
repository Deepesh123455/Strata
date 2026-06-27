# Powerhouse Cache — developer & CI entrypoints.
# Run `make help` for the menu. Targets mirror exactly what CI runs, so "green
# locally" means "green in CI" (modulo the AppLocker quirk on the Windows dev
# box — run `go test` in CI / WSL / git-bash, not under AppLocker).

SHELL := bash
.ONESHELL:

MODULE_DIR := DataPlane
IMAGE      := powerhouse-cache
TAG        ?= local

# Provenance, stamped into the binary and image labels.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_SHA    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.gitSHA=$(GIT_SHA) -X main.buildDate=$(BUILD_DATE)

# Export these so `docker compose` (in `up`/`smoke`) can substitute them into the
# compose build args (${VERSION:-dev} etc.). Without this, compose can't see the
# values computed above and falls back to the dev/local/unknown defaults.
export VERSION
export GIT_SHA
export BUILD_DATE

.DEFAULT_GOAL := help

## ── Go ───────────────────────────────────────────────────────────────────────

.PHONY: fmt
fmt: ## Format the code (gofmt -w)
	cd $(MODULE_DIR) && gofmt -w .

.PHONY: vet
vet: ## go vet
	cd $(MODULE_DIR) && go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (install: https://golangci-lint.run)
	cd $(MODULE_DIR) && golangci-lint run --timeout=5m

.PHONY: test
test: ## Unit tests
	cd $(MODULE_DIR) && go test ./...

.PHONY: race
race: ## Tests under the race detector (what CI gates on)
	cd $(MODULE_DIR) && go test -race -shuffle=on ./...

.PHONY: cover
cover: ## Tests with a coverage report
	cd $(MODULE_DIR) && go test -race -covermode=atomic -coverprofile=coverage.out ./... && \
		go tool cover -func=coverage.out | tail -1

.PHONY: vuln
vuln: ## Scan deps + stdlib for known vulnerabilities
	cd $(MODULE_DIR) && go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: tidy
tidy: ## go mod tidy
	cd $(MODULE_DIR) && go mod tidy

.PHONY: tidy-check
tidy-check: ## Fail if go.mod/go.sum are untidy or uncommitted (CI parity)
	cd $(MODULE_DIR) && go mod verify && go mod tidy && \
		if [ -n "$$(git status --porcelain -- go.mod go.sum)" ]; then \
			echo "go.mod/go.sum not tidy or not committed — run 'make tidy' and commit"; \
			git --no-pager diff -- go.mod go.sum; exit 1; \
		else echo "modules tidy & in sync"; fi

.PHONY: build
build: ## Build the server binary locally → ./bin/powerhoused
	cd $(MODULE_DIR) && go build -trimpath -ldflags="$(LDFLAGS)" -o ../bin/powerhoused ./cmd/powerhoused

.PHONY: run
run: build ## Build then run the server (Ctrl-C to stop)
	./bin/powerhoused -maxmemory 700

## ── Docker / Compose ─────────────────────────────────────────────────────────

.PHONY: docker
docker: ## Build the production image for the local arch
	docker build -f $(MODULE_DIR)/Dockerfile -t $(IMAGE):$(TAG) \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_SHA=$(GIT_SHA) \
		--build-arg BUILD_DATE=$(BUILD_DATE) .

.PHONY: docker-scan
docker-scan: docker ## Build then Trivy-scan the image (HIGH/CRITICAL gate)
	trivy image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 $(IMAGE):$(TAG)

.PHONY: up
up: ## Run the cache via docker compose
	docker compose up --build

.PHONY: smoke
smoke: ## Build + run the end-to-end smoke test, then exit
	docker compose --profile smoke up --build --abort-on-container-exit --exit-code-from smoke
	docker compose --profile smoke down -v

.PHONY: down
down: ## Stop compose (keeps the WAL volume)
	docker compose down

.PHONY: clean
clean: ## Remove local build artifacts
	rm -rf bin dist $(MODULE_DIR)/coverage.out

.PHONY: ci
ci: vet race vuln tidy-check ## What CI runs on the Go side (fast feedback locally)

## ── Help ─────────────────────────────────────────────────────────────────────

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
