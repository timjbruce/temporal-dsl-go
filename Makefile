.PHONY: help check fmt vet test test-integration worker server tidy

help:
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

check: fmt vet test ## Format check, vet, and the hermetic test suite

fmt: ## Fail if anything is unformatted
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

vet: ## go vet, including the integration-tagged files
	go vet ./...
	go vet -tags integration ./test/

test: ## Hermetic tests: no Temporal server required
	go test ./...

test-integration: ## End-to-end against a real Temporal dev server
	go test -tags integration -count=1 -timeout 5m ./test/

worker: ## Run the Temporal worker
	go run ./cmd/worker

server: ## Run the web service on :8080
	go run ./cmd/server

tidy: ## Sync go.mod / go.sum
	go mod tidy
