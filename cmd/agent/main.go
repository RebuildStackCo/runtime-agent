// Command agent is the RebuildStack runtime agent. It collects resource-usage
// rollups and workload metadata inside a Kubernetes cluster and ships them
// one-way to a backend. See docs/ for the architecture.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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

	watcher := collector.NewPodWatcher(clientset, func(p collector.PodInfo) {
		logger.Info("pod observed",
			"namespace", p.Namespace,
			"pod", p.Name,
			"node", p.Node,
			"phase", p.Phase,
			"workload_kind", p.Workload.Kind,
			"workload_name", p.Workload.Name,
			"containers", p.Containers,
		)
	})
	return watcher.Run(ctx)
}
