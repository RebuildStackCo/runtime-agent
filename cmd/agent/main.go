// Command agent is the RebuildStack runtime agent. It collects resource-usage
// rollups and workload metadata inside a Kubernetes cluster and ships them
// one-way to a backend. See docs/ for the architecture.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	clientset, host, err := connect()
	if err != nil {
		stop()
		logger.Error("connecting to cluster", "error", err)
		os.Exit(1)
	}
	logger.Info("connected to cluster", "host", host)

	err = run(ctx, logger, clientset)
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

// run is the agent's lifecycle: it starts, works until ctx is canceled, and
// returns.
func run(ctx context.Context, logger *slog.Logger, clientset kubernetes.Interface) error {
	logger.Info("agent starting", "version", version)
	defer logger.Info("agent stopping")

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

	// Watchers run until ctx is canceled; a failing watcher must bring the
	// agent down rather than leave it half-blind.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	return errors.Join(errs...)
}
