// Command agent is the RebuildStack runtime agent. One binary, two roles
// (ADR 0009):
//
//	agent [controller]   the default role — a cluster-wide collector that talks
//	                     to the Kubernetes API, aggregates usage rollups and
//	                     workload metadata, and ships them one-way to a backend.
//	agent node           a per-node DaemonSet role that scans on-node processes
//	                     for Go build information. It holds NO Kubernetes client
//	                     and opens no external connection.
//
// The role is selected by the first argument; absent one, the controller runs.
// See docs/ for the architecture.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// version is set at build time via -ldflags.
var version = "dev"

// Roles the binary can run as. The controller is the default so that existing
// invocations (`agent -config …`) keep working unchanged.
const (
	roleController = "controller"
	roleNode       = "node"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	role, rest := parseRole(os.Args[1:])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	var err error
	switch role {
	case roleController:
		err = runController(ctx, logger, rest)
	case roleNode:
		err = runNode(ctx, logger, rest)
	default:
		stop()
		logger.Error("unknown role; expected 'controller' or 'node'", "role", role)
		os.Exit(2)
	}
	stop()
	if err != nil {
		logger.Error("agent exited with error", "role", role, "error", err)
		os.Exit(1)
	}
}

// parseRole splits an optional leading role subcommand off the argument list.
// A first argument that does not look like a flag is the role; otherwise the
// controller role is assumed and all arguments are flags.
func parseRole(args []string) (role string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return roleController, args
}

// runController parses the controller flags, loads configuration, connects to
// the cluster, and runs the collection lifecycle.
func runController(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet(roleController, flag.ExitOnError)
	configPath := fs.String("config", "", "path to the agent configuration file (YAML)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	clientset, restConfig, err := connect()
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}
	logger.Info("connected to cluster", "host", restConfig.Host)

	return run(ctx, logger, clientset, restConfig, cfg)
}

// connect builds a clientset from the in-cluster service account when
// running as a pod, falling back to the local kubeconfig otherwise. It returns
// the REST config too: the node-intake receiver builds its JWKS HTTP client
// from it, reusing the in-cluster CA and bearer credential (ADR 0010).
func connect() (kubernetes.Interface, *rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("no in-cluster config and no kubeconfig: %w", err)
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}
	return clientset, config, nil
}

// coverageInterval is how often the aggregate coverage counters are logged.
const coverageInterval = time.Minute

// run is the agent's lifecycle: it starts, works until ctx is canceled, and
// returns.
func run(ctx context.Context, logger *slog.Logger, clientset kubernetes.Interface, restConfig *rest.Config, cfg config.Config) error {
	logger.Info("agent starting", "version", version)
	defer logger.Info("agent stopping")

	// The local sink is optional: without a spool directory the agent runs
	// log-only (development mode). Durability of the directory is the
	// installation's choice (ADR 0007).
	var spool *sink.Spool
	if cfg.Spool.Dir != "" {
		var err error
		spool, err = sink.NewSpool(cfg.Spool.Dir, time.Duration(cfg.Spool.MaxAgeHours)*time.Hour)
		if err != nil {
			return fmt.Errorf("opening spool: %w", err)
		}
		logger.Info("spool open", "dir", cfg.Spool.Dir)
	}

	filter := collector.NewPodFilter(cfg.Filters.Namespaces.Allow, cfg.Filters.Namespaces.Deny)

	podWatcher := collector.NewPodWatcher(clientset, func(p collector.PodInfo) {
		logger.Info("pod observed",
			"namespace", p.Namespace,
			"pod", p.Name,
			"node", p.Node,
			"phase", p.Phase,
			"qos", p.QOSClass,
			"workload_kind", p.Workload.Kind,
			"workload_name", p.Workload.Name,
			"containers", p.Containers,
		)
	})
	podWatcher.OnOOMKill(func(o collector.OOMKill) {
		logger.Warn("oom kill observed",
			"namespace", o.Namespace,
			"pod", o.Pod,
			"container", o.Container,
			"workload_kind", o.Workload.Kind,
			"workload_name", o.Workload.Name,
			"finished_at", o.FinishedAt,
			"exit_code", o.ExitCode,
			"restart_count", o.RestartCount,
			"memory_limit_bytes", o.MemoryLimitBytes,
		)
		if spool != nil {
			if err := spool.WriteOOMKill(o); err != nil {
				logger.Error("spooling oom event", "error", err)
			}
		}
	})
	podWatcher.SetFilter(filter)
	nodeWatcher := collector.NewNodeWatcher(clientset, func(n collector.NodeInfo) {
		logger.Info("node observed",
			"node", n.Name,
			"instance_type", n.InstanceType,
			"capacity_type", n.CapacityType,
			"kernel_version", n.KernelVersion,
			"allocatable_cpu_milli", n.AllocatableCPUMilli,
			"allocatable_memory_bytes", n.AllocatableMemoryBytes,
			"capacity_cpu_milli", n.CapacityCPUMilli,
			"capacity_memory_bytes", n.CapacityMemoryBytes,
		)
	})

	// logRecords writes rollup records one line each; the JSON body is the
	// exact record shape (kind marks snapshots vs. final closed windows).
	logRecords := func(kind string, sequence int64, records []*rollup.Record) {
		for _, r := range records {
			encoded, err := json.Marshal(r)
			if err != nil {
				logger.Error("encoding usage record", "error", err)
				continue
			}
			logger.Info(kind,
				"sequence", sequence,
				"record", json.RawMessage(encoded),
			)
		}
	}
	usagePoller := collector.NewUsagePoller(clientset, nodeWatcher.Names, podWatcher,
		func(sequence int64, records []*rollup.Record) {
			logRecords("usage rollup snapshot", sequence, records)
			if spool != nil {
				if err := spool.WriteUsageSnapshot(sequence, records); err != nil {
					logger.Error("spooling usage snapshot", "error", err)
				}
			}
		},
		func(records []*rollup.Record) {
			logRecords("usage rollup closed", 0, records)
			if spool != nil {
				if err := spool.WriteClosedWindows(records); err != nil {
					logger.Error("spooling closed windows", "error", err)
				}
			}
		},
		func(node string, err error) {
			// Routine during node lifecycle events; counters recover the
			// full interval on the next successful poll.
			logger.Warn("kubelet poll failed", "node", node, "error", err)
		},
	)

	// Excluded pods are reported as aggregate counts only, never by name
	// (docs/security.md §8).
	logCoverage := func() {
		c := filter.Snapshot()
		logger.Info("coverage",
			"pods_observed", c.PodsObserved,
			"excluded_namespace_filter", c.ExcludedNamespaceFilter,
			"excluded_namespace_annotation", c.ExcludedNamespaceAnnotation,
			"excluded_pod_annotation", c.ExcludedPodAnnotation,
			"usage_signals", usagePoller.Signals(),
		)
	}

	// Watchers run until ctx is canceled; a failing watcher must bring the
	// agent down rather than leave it half-blind.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		ticker := time.NewTicker(coverageInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logCoverage()
			}
		}
	}()

	tasks := map[string]func(context.Context) error{
		"pods":  podWatcher.Run,
		"nodes": nodeWatcher.Run,
		"usage": usagePoller.Run,
	}

	// The node-intake receiver is optional (only the ebpf/node profile ships a
	// DaemonSet). When enabled it becomes one more lifecycle task, validating
	// node tokens locally against the cluster JWKS (ADR 0010).
	if cfg.NodeIntake.Enabled {
		intake, err := buildNodeIntake(logger, restConfig, cfg.NodeIntake)
		if err != nil {
			return fmt.Errorf("configuring node intake: %w", err)
		}
		tasks["node-intake"] = intake.Run
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for name, run := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := run(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s watcher: %w", name, err))
				mu.Unlock()
			}
			cancel()
		}()
	}
	wg.Wait()
	logCoverage()
	return errors.Join(errs...)
}

// buildNodeIntake constructs the node-intake receiver: a token verifier backed
// by the cluster JWKS (fetched over the API server's in-cluster transport, no
// TokenReview) and an HTTP server that decodes node reports. For this slice the
// report is authenticated, decoded, and logged in aggregate; the join into a
// payload lands in the next slice.
func buildNodeIntake(logger *slog.Logger, restConfig *rest.Config, cfg config.NodeIntake) (*nodeintake.Server, error) {
	addr := cfg.ListenAddress
	if addr == "" {
		addr = config.DefaultNodeIntakeListenAddress
	}
	audience := cfg.Audience
	if audience == "" {
		audience = config.DefaultNodeIntakeAudience
	}

	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building JWKS http client: %w", err)
	}
	keys := &nodeauth.CachingKeySource{
		Source: &nodeauth.HTTPKeySource{IssuerBaseURL: restConfig.Host, Client: httpClient},
	}
	verifier := nodeauth.NewVerifier(keys, audience, nodeauth.WithExpectedSubject(cfg.ExpectedSubject))

	onReport := func(id nodeauth.Identity, report nodescan.Report) {
		// Aggregate-only: identities of filtered-out binaries are already gone
		// (the node filtered them, ADR 0009); here we log what arrived and from
		// whom. The per-workload join and payload come in the next slice.
		logger.Info("node report received",
			"from_subject", id.Subject,
			"node", report.Node,
			"binaries", len(report.Binaries),
			"processes_scanned", report.Counters.ProcessesScanned,
			"go_found", report.Counters.GoFound,
			"filtered_infra", report.Counters.FilteredInfra,
			"unreadable", report.Counters.Unreadable,
		)
	}

	handler := nodeintake.NewHandler(verifier, logger, onReport)
	logger.Info("node intake enabled", "addr", addr, "audience", audience, "expected_subject", cfg.ExpectedSubject)
	return nodeintake.NewServer(addr, handler, logger), nil
}
