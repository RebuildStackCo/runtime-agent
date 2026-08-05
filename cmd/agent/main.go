// Command agent is the RebuildStack runtime agent. It collects resource-usage
// rollups and workload metadata inside a Kubernetes cluster and ships them
// one-way to a backend. See docs/ for the architecture.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/config"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to the agent configuration file (YAML)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		stop()
		logger.Error("loading configuration", "error", err)
		os.Exit(1)
	}

	clientset, host, err := connect()
	if err != nil {
		stop()
		logger.Error("connecting to cluster", "error", err)
		os.Exit(1)
	}
	logger.Info("connected to cluster", "host", host)

	err = run(ctx, logger, clientset, cfg)
	stop()
	if err != nil {
		logger.Error("agent exited with error", "error", err)
		os.Exit(1)
	}
}

// connect builds a clientset from the in-cluster service account when
// running as a pod, falling back to the local kubeconfig otherwise.
func connect() (kubernetes.Interface, string, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, "", fmt.Errorf("no in-cluster config and no kubeconfig: %w", err)
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, "", err
	}
	return clientset, config.Host, nil
}

// coverageInterval is how often the aggregate coverage counters are logged.
const coverageInterval = time.Minute

// run is the agent's lifecycle: it starts, works until ctx is canceled, and
// returns.
func run(ctx context.Context, logger *slog.Logger, clientset kubernetes.Interface, cfg config.Config) error {
	logger.Info("agent starting", "version", version)
	defer logger.Info("agent stopping")

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
	})
	podWatcher.SetFilter(filter)
	nodeWatcher := collector.NewNodeWatcher(clientset, func(n collector.NodeInfo) {
		logger.Info("node observed",
			"node", n.Name,
			"instance_type", n.InstanceType,
			"capacity_type", n.CapacityType,
			"allocatable_cpu_milli", n.AllocatableCPUMilli,
			"allocatable_memory_bytes", n.AllocatableMemoryBytes,
			"capacity_cpu_milli", n.CapacityCPUMilli,
			"capacity_memory_bytes", n.CapacityMemoryBytes,
		)
	})

	// Excluded pods are reported as aggregate counts only, never by name
	// (docs/security.md §8).
	logCoverage := func() {
		c := filter.Snapshot()
		logger.Info("coverage",
			"pods_observed", c.PodsObserved,
			"excluded_namespace_filter", c.ExcludedNamespaceFilter,
			"excluded_namespace_annotation", c.ExcludedNamespaceAnnotation,
			"excluded_pod_annotation", c.ExcludedPodAnnotation,
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
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for name, run := range map[string]func(context.Context) error{
		"pods":  podWatcher.Run,
		"nodes": nodeWatcher.Run,
	} {
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
