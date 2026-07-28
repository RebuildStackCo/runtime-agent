VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

E2E_CLUSTER ?= runtime-agent-e2e

.PHONY: build test lint tidy clean cluster-up cluster-down e2e

build: ## Build the agent binary into bin/
	go build -ldflags '$(LDFLAGS)' -o bin/agent ./cmd/agent

test: ## Run all tests with the race detector
	go test -race ./...

lint: ## Run golangci-lint
	golangci-lint run

tidy: ## Sync go.mod/go.sum
	go mod tidy

cluster-up: ## Create the disposable kind cluster for e2e
	go tool kind create cluster --name $(E2E_CLUSTER) --wait 120s

cluster-down: ## Delete the e2e kind cluster
	go tool kind delete cluster --name $(E2E_CLUSTER)

e2e: ## Run e2e tests against the kind cluster (see cluster-up)
	E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) go test -tags e2e -count=1 -timeout 10m ./test/e2e/

clean:
	rm -rf bin
