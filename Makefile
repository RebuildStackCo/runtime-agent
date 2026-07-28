VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test lint tidy clean

build: ## Build the agent binary into bin/
	go build -ldflags '$(LDFLAGS)' -o bin/agent ./cmd/agent

test: ## Run all tests with the race detector
	go test -race ./...

lint: ## Run golangci-lint
	golangci-lint run

tidy: ## Sync go.mod/go.sum
	go mod tidy

clean:
	rm -rf bin
