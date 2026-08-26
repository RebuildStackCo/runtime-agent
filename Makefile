SHELL := /bin/bash

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

E2E_CLUSTER ?= runtime-agent-e2e
SMOKE_SECONDS ?= 5

IMAGE ?= runtime-agent
IMAGE_TAG ?= $(VERSION)
SAMPLE_IMAGE ?= rebuildstack-e2e-goworkload:latest
# A shell-bearing image loaded into kind as a sidecar so the inventory e2e can
# read the controller's spool (the agent image is distroless — no shell).
SPOOL_READER_IMAGE ?= busybox:1.37

.PHONY: build test lint chart-lint tidy clean cluster-up cluster-down e2e smoke image sample-image kind-load node-e2e inventory-e2e restarts-e2e lifecycle-e2e policy-e2e watch-e2e profile-gate-e2e profile-capture-e2e

build: ## Build the agent binary into bin/
	go build -ldflags '$(LDFLAGS)' -o bin/agent ./cmd/agent

test: ## Run all tests with the race detector, and type-check the e2e-tagged ones
	go test -race ./...
	# The e2e tests are behind a build tag, so `go test ./...` never compiles
	# them and a changed signature can rot there unnoticed. Type-check them on
	# every run; they still only execute against a kind cluster.
	go vet -tags e2e ./...

lint: ## Run golangci-lint
	golangci-lint run

chart-lint: ## Lint the Helm chart with the helm CLI, once per install profile
	go tool helm lint charts/runtime-agent --set profile=metrics-only
	go tool helm lint charts/runtime-agent --set profile=inventory
	go tool helm lint charts/runtime-agent \
		--set profile=ebpf --set profiling.allowedModulePrefixes={example.com/app}

tidy: ## Sync go.mod/go.sum
	go mod tidy

cluster-up: ## Create the disposable kind cluster for e2e
	go tool kind create cluster --name $(E2E_CLUSTER) --wait 120s

cluster-down: ## Delete the e2e kind cluster
	go tool kind delete cluster --name $(E2E_CLUSTER)

e2e: ## Run e2e tests against the kind cluster (see cluster-up); log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	set -o pipefail; E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) go test -tags e2e -count=1 -timeout 15m -v ./test/e2e/ 2>&1 \
		| tee test/e2e/logs/e2e-$$(date +%Y%m%d-%H%M%S).log

smoke: build ## Run the agent for SMOKE_SECONDS against the kind cluster (see cluster-up); log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	@kubeconfig=$$(mktemp); \
	trap 'rm -f "$$kubeconfig"' EXIT; \
	go tool kind export kubeconfig --name $(E2E_CLUSTER) --kubeconfig "$$kubeconfig" || exit 1; \
	log=test/e2e/logs/smoke-$$(date +%Y%m%d-%H%M%S).log; \
	KUBECONFIG=$$kubeconfig ./bin/agent >/dev/null 2>"$$log" & pid=$$!; \
	sleep $(SMOKE_SECONDS); kill -TERM $$pid; wait $$pid; \
	echo "smoke log: $$log"

image: ## Build the agent container image (controller + node roles, one binary)
	docker build -t $(IMAGE):$(IMAGE_TAG) --build-arg VERSION=$(VERSION) .

sample-image: ## Build the e2e "known Go process" workload image
	docker build -t $(SAMPLE_IMAGE) test/e2e/sample

kind-load: image sample-image ## Load the agent and sample images into the kind cluster
	go tool kind load docker-image $(IMAGE):$(IMAGE_TAG) $(SAMPLE_IMAGE) --name $(E2E_CLUSTER)

node-e2e: kind-load ## Deploy the node DaemonSet in kind and assert Go-binary detection; log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	set -o pipefail; \
	E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) \
	E2E_AGENT_IMAGE=$(IMAGE):$(IMAGE_TAG) \
	E2E_SAMPLE_IMAGE=$(SAMPLE_IMAGE) \
	go test -tags e2e -count=1 -timeout 15m -v ./test/e2e/ -run TestNodeScanner 2>&1 \
		| tee test/e2e/logs/node-e2e-$$(date +%Y%m%d-%H%M%S).log

inventory-e2e: kind-load ## Deploy controller + node DaemonSet + sample in kind and assert the go_inventory payload in the controller spool; log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	docker pull $(SPOOL_READER_IMAGE)
	go tool kind load docker-image $(SPOOL_READER_IMAGE) --name $(E2E_CLUSTER)
	set -o pipefail; \
	E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) \
	E2E_AGENT_IMAGE=$(IMAGE):$(IMAGE_TAG) \
	E2E_SAMPLE_IMAGE=$(SAMPLE_IMAGE) \
	E2E_SPOOL_READER_IMAGE=$(SPOOL_READER_IMAGE) \
	go test -tags e2e -count=1 -timeout 15m -v ./test/e2e/ -run TestGoInventoryEndToEnd 2>&1 \
		| tee test/e2e/logs/inventory-e2e-$$(date +%Y%m%d-%H%M%S).log

policy-e2e: kind-load ## Deploy the controller in kind and assert job_runs, deployment_revisions, workload_policy and cluster_policy in the spool — including that the widened ClusterRole grants what ADR 0032 says; log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	docker pull $(SPOOL_READER_IMAGE)
	go tool kind load docker-image $(SPOOL_READER_IMAGE) --name $(E2E_CLUSTER)
	set -o pipefail; \
	E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) \
	E2E_AGENT_IMAGE=$(IMAGE):$(IMAGE_TAG) \
	E2E_SPOOL_READER_IMAGE=$(SPOOL_READER_IMAGE) \
	go test -tags e2e -count=1 -timeout 20m -v ./test/e2e/ -run TestPolicyAndJournalsEndToEnd 2>&1 \
		| tee test/e2e/logs/policy-e2e-$$(date +%Y%m%d-%H%M%S).log

profile-gate-e2e: kind-load ## Deploy the ebpf node variant in kind and assert the eBPF gate refuses gracefully while the scanner keeps running; log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	set -o pipefail; \
	E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) \
	E2E_AGENT_IMAGE=$(IMAGE):$(IMAGE_TAG) \
	go test -tags e2e -count=1 -timeout 15m -v ./test/e2e/ -run TestEBPFGateRefusesGracefully 2>&1 \
		| tee test/e2e/logs/profile-gate-e2e-$$(date +%Y%m%d-%H%M%S).log

profile-capture-e2e: kind-load ## Deploy controller + ebpf node + CPU-hot sample in kind and assert the full eBPF capture→filter→ship→spool path (ADR 0011); skips where the host cannot run eBPF (no BTF / no CAP_BPF, e.g. Docker Desktop); log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	docker pull $(SPOOL_READER_IMAGE)
	go tool kind load docker-image $(SPOOL_READER_IMAGE) --name $(E2E_CLUSTER)
	set -o pipefail; \
	E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) \
	E2E_AGENT_IMAGE=$(IMAGE):$(IMAGE_TAG) \
	E2E_SAMPLE_IMAGE=$(SAMPLE_IMAGE) \
	E2E_SPOOL_READER_IMAGE=$(SPOOL_READER_IMAGE) \
	go test -tags e2e -count=1 -timeout 20m -v ./test/e2e/ -run TestEBPFCaptureEndToEnd 2>&1 \
		| tee test/e2e/logs/profile-capture-e2e-$$(date +%Y%m%d-%H%M%S).log

restarts-e2e: kind-load ## Deploy the controller in kind next to a crash-looping workload and assert both the container_restarts windows (ADR 0020) and the restart_counters reading a late-arriving agent recovers (ADR 0034); log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	docker pull $(SPOOL_READER_IMAGE)
	go tool kind load docker-image $(SPOOL_READER_IMAGE) --name $(E2E_CLUSTER)
	set -o pipefail; \
	E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) \
	E2E_AGENT_IMAGE=$(IMAGE):$(IMAGE_TAG) \
	E2E_SPOOL_READER_IMAGE=$(SPOOL_READER_IMAGE) \
	go test -tags e2e -count=1 -timeout 25m -v ./test/e2e/ \
		-run 'TestContainerRestartsEndToEnd|TestRestartCountersCarryTheHistoryTheAgentDidNotWatch' 2>&1 \
		| tee test/e2e/logs/restarts-e2e-$$(date +%Y%m%d-%H%M%S).log

watch-e2e: kind-load ## Deny the pods grant, then revoke a policy grant from a running agent, and assert the first stops the agent while the second only degrades its payload (ADR 0035); slow by nature — an established watch is re-authorized only when client-go re-establishes it, every 5–10 minutes; log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	docker pull $(SPOOL_READER_IMAGE)
	go tool kind load docker-image $(SPOOL_READER_IMAGE) --name $(E2E_CLUSTER)
	set -o pipefail; \
	E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) \
	E2E_AGENT_IMAGE=$(IMAGE):$(IMAGE_TAG) \
	E2E_SPOOL_READER_IMAGE=$(SPOOL_READER_IMAGE) \
	go test -tags e2e -count=1 -timeout 45m -v ./test/e2e/ \
		-run 'TestAnAgentDeniedAGatingPermissionStopsInsteadOfWaitingForever|TestAPermissionRevokedFromTheRunningAgentReachesThePayload' 2>&1 \
		| tee test/e2e/logs/watch-e2e-$$(date +%Y%m%d-%H%M%S).log

lifecycle-e2e: kind-load ## Deploy the controller in kind, stage an unschedulable pod and a scheduler preemption, and assert both reach the payloads (ADR 0021); log goes to test/e2e/logs/
	@mkdir -p test/e2e/logs
	docker pull $(SPOOL_READER_IMAGE)
	go tool kind load docker-image $(SPOOL_READER_IMAGE) --name $(E2E_CLUSTER)
	set -o pipefail; \
	E2E_KUBE_CONTEXT=kind-$(E2E_CLUSTER) \
	E2E_AGENT_IMAGE=$(IMAGE):$(IMAGE_TAG) \
	E2E_SPOOL_READER_IMAGE=$(SPOOL_READER_IMAGE) \
	go test -tags e2e -count=1 -timeout 20m -v ./test/e2e/ -run TestPodLifecycleEndToEnd 2>&1 \
		| tee test/e2e/logs/lifecycle-e2e-$$(date +%Y%m%d-%H%M%S).log

clean:
	rm -rf bin
