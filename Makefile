.PHONY: help build test vet clean up down restart logs ps shell check

# ── Configuration ──────────────────────────────────────────────
IMAGE      ?= llmapiproxy
TAG        ?= dev
CONTAINER  ?= llmapiproxy
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# ── Help ───────────────────────────────────────────────────────

help: ## Show this help message
	@echo ''
	@echo ' ▗▖   ▗▖   ▗▖  ▗▖     ▗▄▖ ▗▄▄▖▗▄▄▄▖    ▗▄▄▖ ▗▄▄▖  ▗▄▖ ▗▖  ▗▖▗▖  ▗▖'
	@echo ' ▐▌   ▐▌   ▐▛▚▞▜▌    ▐▌ ▐▌▐▌ ▐▌ █      ▐▌ ▐▌▐▌ ▐▌▐▌ ▐▌ ▝▚▞▘  ▝▚▞▘ '
	@echo ' ▐▌   ▐▌   ▐▌  ▐▌    ▐▛▀▜▌▐▛▀▘  █      ▐▛▀▘ ▐▛▀▚▖▐▌ ▐▌  ▐▌    ▐▌  '
	@echo ' ▐▙▄▄▖▐▙▄▄▖▐▌  ▐▌    ▐▌ ▐▌▐▌  ▗▄█▄▖    ▐▌   ▐▌ ▐▌▝▚▄▞▘▗▞▘▝▚▖  ▐▌  '
	@echo ''
	@echo '  Unify LLM providers behind a single OpenAI-compatible endpoint'
	@echo ''
	@printf '  \033[1mUsage:\033[0m make <target>\n'
	@echo ''
	@printf '  \033[1mDocker\033[0m\n'
	@grep -E '^(up|down|restart|logs|ps|shell|docker-build):.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "    \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo ''
	@printf '  \033[1mDevelopment\033[0m\n'
	@grep -E '^(run|build|test|vet|check|clean):.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "    \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo ''

# ── Docker ─────────────────────────────────────────────────────

docker-build: ## Build the Docker image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .

up: ## Start the container (rebuild if needed)
	@make docker-build && docker compose up -d --force-recreate || (printf '\n\033[31mBuild failed, no restart!\033[0m\n\n' && exit 1)

down: ## Stop and remove the container
	docker compose down

restart: ## Restart the container (rebuild if needed)
	docker compose up --build -d --force-recreate

logs: ## Tail container logs
	docker compose logs -f

ps: ## Show running containers
	docker compose ps

shell: ## Open a shell inside the running container
	docker compose exec $(CONTAINER) /bin/sh || docker compose run --rm $(CONTAINER) /bin/sh

# ── Development ────────────────────────────────────────────────

run: ## Run the proxy locally
	go run ./cmd/llmapiproxy serve --config data/config.yaml

build: ## Build the binary locally
	go build -ldflags "-X main.version=$(VERSION)" -o llmapiproxy ./cmd/llmapiproxy

test: ## Run all tests
	go test -count=1 ./...

vet: ## Run static analysis
	go vet ./...

check: ## Run vet + test
	go vet ./... && go test -count=1 ./...

clean: ## Remove build artifacts
	rm -f llmapiproxy
