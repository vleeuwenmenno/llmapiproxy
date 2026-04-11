.PHONY: help run build clean up down logs docker-build

help: ## Show this help message
	@echo ''
	@echo 'LLM API Proxy'
	@echo '============='
	@echo ''
	@echo 'Usage: make <target>'
	@echo ''
	@echo 'Targets:'
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo ''

run: ## Run the application locally
	go run ./cmd/llmapiproxy -config config.yaml

build: ## Build the binary
	go build -o llmapiproxy ./cmd/llmapiproxy

clean: ## Remove build artifacts
	rm -f llmapiproxy

docker-build: ## Build the Docker image locally (tagged llmapiproxy:dev)
	docker build -t llmapiproxy:dev .

up: ## Build and start the Docker container (dev)
	docker compose up --build

down: ## Stop and remove the Docker container
	docker compose down

logs: ## Tail logs from the running Docker container
	docker compose logs -f
