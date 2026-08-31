// Command agent is the RebuildStack runtime agent. One binary, two roles
// (ADR 0009). The default controller role is a cluster-wide collector that talks
// to the Kubernetes API and ships rollups and metadata one-way to a backend.
// `agent node` is a per-node DaemonSet role that scans on-node processes for Go
// build information, holds no Kubernetes client and opens no external connection.
//
// The role is selected by the first argument; absent one, the controller runs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/inventory"
	"github.com/RebuildStackCo/runtime-agent/internal/journal"
	"github.com/RebuildStackCo/runtime-agent/internal/metadata"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeintake"
	"github.com/RebuildStackCo/runtime-agent/internal/nodeprofile"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofprobe"
	"github.com/RebuildStackCo/runtime-agent/internal/pprofpull"
	"github.com/RebuildStackCo/runtime-agent/internal/revisions"
	"github.com/RebuildStackCo/runtime-agent/internal/rollup"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
	"github.com/RebuildStackCo/runtime-agent/internal/targeting"
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

	startedAt := time.Now()
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}
	// Counts and switches, never a name from the file: a digest of a handful of
	// short namespace names is reversible, and the list it would expose is the
	// one the customer chose to hide (ADR 0054 §2).
	configShape := cfg.Describe(*configPath, startedAt)

	clientset, restConfig, err := connect()
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}
	logger.Info("connected to cluster", "host", restConfig.Host)

	return run(ctx, logger, clientset, restConfig, cfg, configShape, startedAt)
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
	// Client-side throttling defaults to 5 QPS, which one usage poll of a
	// 75-node cluster already exceeds — and a request that waits on the limiter
	// spends its own deadline waiting (ADR 0045).
	config.QPS, config.Burst = 50, 100
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}
	return clientset, config, nil
}

// coverageInterval is how often the aggregate coverage counters are logged and
// written as a payload. Unlike every other flush it runs whether or not there is
// anything to report: staleness here is a fact about the agent (ADR 0054 §5).
const coverageInterval = time.Minute

// pprofProbeInterval is how often endpoint discovery asks about targets it has
// no answer for. A round on a steady cluster does nothing — every target already
// has one — so the cadence costs a map walk and buys a new image being confirmed
// within a minute of the node reporting it (ADR 0057 §2).
const pprofProbeInterval = time.Minute

// pprofPullInterval is how often a round of profiles is fetched. One round
// visits the least recently profiled workloads, one at a time, so the interval
// and the per-round bound together are what the cluster pays: at most ten
// ten-second captures, consecutively, per interval (ADR 0058 §2).
const pprofPullInterval = 5 * time.Minute

// run is the agent's lifecycle: it starts, works until ctx is canceled, and
// returns.
func run(ctx context.Context, logger *slog.Logger, clientset kubernetes.Interface, restConfig *rest.Config, cfg config.Config, configShape config.Shape, startedAt time.Time) error {
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

	filter := collector.NewFilter(cfg.Filters.Namespaces.Allow, cfg.Filters.Namespaces.Deny)

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
	// The restart journal aggregates per container per window rather than
	// emitting one payload per restart: a crash loop must not put the spool's
	// file count under its own control (ADR 0020).
	restartJournal := journal.NewRestarts(collector.UsageWindowLength)
	podWatcher.OnContainerRestart(func(r collector.ContainerRestart) {
		logger.Info("container restarted",
			"namespace", r.Namespace,
			"pod", r.Pod,
			"container", r.Container,
			"workload_kind", r.Workload.Kind,
			"workload_name", r.Workload.Name,
			"restarts", r.Restarts,
			"reason", r.Reason,
			"exit_code", r.ExitCode,
		)
		restartJournal.Observe(r)
	})
	// Disruptions share the journal's windows but not its arithmetic: a pod is
	// preempted or evicted once, so the accumulator holds records rather than
	// counters (ADR 0021).
	disruptionJournal := journal.NewDisruptions(collector.UsageWindowLength)
	podWatcher.OnPodDisruption(func(d collector.PodDisruption) {
		logger.Warn("pod disrupted",
			"namespace", d.Namespace,
			"pod", d.Pod,
			"workload_kind", d.Workload.Kind,
			"workload_name", d.Workload.Name,
			"node", d.Node,
			"reason", d.Reason,
			"disrupted_at", d.DisruptedAt,
		)
		disruptionJournal.Observe(d)
	})

	// Finished Job runs. Same window shape as the journals above; the facts are
	// the run's own instants and outcome, which is what a usage rollup cannot
	// supply for a workload that ran for part of a window (ADR 0029).
	jobJournal := journal.NewJobRuns(collector.UsageWindowLength)
	podWatcher.OnJobFinished(func(r collector.JobRun) {
		logger.Info("job run finished",
			"namespace", r.Namespace,
			"workload_kind", r.Workload.Kind,
			"workload_name", r.Workload.Name,
			"job", r.Name,
			"result", r.Result,
			"fail_reason", r.FailReason,
			"started_at", r.StartedAt,
			"finished_at", r.FinishedAt,
			"succeeded", r.Succeeded,
			"failed", r.Failed,
		)
		jobJournal.Observe(r)
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
	logRecords := func(kind string, records []*rollup.Record) {
		for _, r := range records {
			encoded, err := json.Marshal(r)
			if err != nil {
				logger.Error("encoding usage record", "error", err)
				continue
			}
			logger.Info(kind, "record", json.RawMessage(encoded))
		}
	}
	// The targeting publisher (ADR 0011 §3, §6b) ranks collected workloads by
	// consumption from each usage snapshot and publishes the top N for the node
	// targets query. Created only when profiling is enabled; the snapshot
	// callback feeds it deep-copied records, so it never races the Accumulator.
	var targetsPublisher *targeting.Publisher
	if cfg.Profiling.Enabled {
		pc := cfg.Profiling.Normalized()
		targetsPublisher = targeting.NewPublisher(pc.TopN)
	}
	// Declared before construction so the callbacks can read the poller's own
	// observation state; they run on the poll goroutine, and Observation() is
	// synchronized regardless.
	var usagePoller *collector.UsagePoller
	usagePoller = collector.NewUsagePoller(clientset, nodeWatcher.Names, podWatcher,
		func(records []*rollup.Record) {
			logRecords("usage rollup snapshot", records)
			if targetsPublisher != nil {
				targetsPublisher.Publish(records)
			}
			if spool != nil {
				if err := spool.WriteUsageSnapshot(records, usagePoller.Observation()); err != nil {
					logger.Error("spooling usage snapshot", "error", err)
				}
			}
		},
		func(records []*rollup.Record) {
			logRecords("usage rollup closed", records)
			if spool != nil {
				if err := spool.WriteClosedWindows(records, usagePoller.Observation()); err != nil {
					logger.Error("spooling closed windows", "error", err)
				}
			}
		},
		func(records []*rollup.NetworkRecord) {
			logger.Info("network windows closed", "records", len(records))
			if spool != nil {
				if err := spool.WriteNetworkWindows(records, usagePoller.Observation()); err != nil {
					logger.Error("spooling network windows", "error", err)
				}
			}
		},
		func(node string, err error) {
			// Routine during node lifecycle events; counters recover the
			// full interval on the next successful poll.
			logger.Warn("kubelet poll failed", "node", node, "error", err)
		},
	)

	// The Go inventory joins node-role build-info facts against the workload
	// index (ADR 0010). It exists only when the receiver does; the receiver's
	// callback ingests into it, and the coverage goroutine flushes it to the
	// spool on the same cadence as everything else. Loss-harmless: it is
	// rebuilt from the next node scan (ADR 0003).
	var goStore *inventory.Store
	if cfg.NodeIntake.Enabled {
		goStore = inventory.NewStore(time.Now())
	}

	// Endpoint discovery, which exists only where the node role does: the two
	// facts it funnels on — the linked package and the bound port — are read on
	// the node and arrive nowhere else (ADR 0057 §1).
	var prober *pprofprobe.Prober
	if goStore != nil && cfg.Profiling.Pprof.Enabled {
		prober = pprofprobe.New(func(c pprofprobe.Candidate) (string, bool) {
			ip, ok := podWatcher.PodAddress(c.Namespace,
				collector.WorkloadRef{Kind: c.WorkloadKind, Name: c.WorkloadName},
				c.Container, c.ImageDigest)
			if !ok {
				return "", false
			}
			return pprofprobe.HostPort(ip, c.Port), true
		}, logger)
	}

	// The puller, which exists only where a confirmed endpoint can: it profiles
	// what the prober found and nothing else (ADR 0058).
	var puller *pprofpull.Puller
	if prober != nil && cfg.Profiling.Pprof.Pull && spool != nil {
		address := func(c pprofprobe.Candidate) (string, bool) {
			ip, ok := podWatcher.PodAddress(c.Namespace,
				collector.WorkloadRef{Kind: c.WorkloadKind, Name: c.WorkloadName},
				c.Container, c.ImageDigest)
			if !ok {
				return "", false
			}
			return pprofprobe.HostPort(ip, c.Port), true
		}
		puller = pprofpull.New(cfg.Profiling.AllowedModulePrefixes,
			nodeprofile.ThirdPartyPolicy(cfg.Profiling.ThirdPartySymbols),
			address, func(p pprofpull.Pulled) error {
				return spool.WritePulledProfile(sink.ProfileKey{
					Namespace:    p.Namespace,
					Workload:     p.WorkloadName,
					Container:    p.Container,
					ImageDigest:  p.ImageDigest,
					CaptureStart: p.Start,
					CaptureEnd:   p.End,
				}, p.Pprof, sink.ProfileDrops{
					ThirdPartyFrames:   p.Dropped.ThirdPartyDropped,
					UnsymbolizedFrames: p.Dropped.UnsymbolizedDropped,
					SamplesTouched:     p.Dropped.SamplesFiltered,
				})
			}, logger)
	}
	var inventoryMu sync.Mutex
	flushInventory := func() {
		if goStore == nil || spool == nil {
			return
		}
		inventoryMu.Lock()
		defer inventoryMu.Unlock()
		// Forget first, then publish. The inventory accumulates pushed facts, so
		// nothing else would ever remove a record whose workload is gone — or
		// whose pod the customer opted out of, which stops new facts arriving
		// without removing the ones already held (ADR 0018). The live index is
		// the same one workload metadata is built from, so the two payloads
		// cannot disagree about which workloads exist.
		evicted := goStore.Retain(inventory.LiveKeys(podWatcher.Pods()))
		goStore.RetainNodes(nodeWatcher.Names())
		if evicted.Records > 0 || evicted.Builds > 0 || evicted.Peaks > 0 || evicted.Ports > 0 {
			logger.Info("go inventory forgot departed workloads",
				"records", evicted.Records, "builds", evicted.Builds,
				"peaks", evicted.Peaks, "ports", evicted.Ports)
		}
		if err := spool.WriteGoInventory(time.Now(), goStore.Coverage(), goStore.Snapshot()); err != nil {
			logger.Error("spooling go inventory", "error", err)
		}
		// The measured half of the same reports, in its own payload: a peak is
		// not the kind of claim the inventory beside it makes (ADR 0052).
		if err := spool.WriteProcessPeaks(time.Now(), goStore.PeakSnapshot()); err != nil {
			logger.Error("spooling process peaks", "error", err)
		}
		// Where those processes accept connections. Structural like the
		// inventory, and separate from it because it changes without the build
		// changing (ADR 0056 §3).
		if err := spool.WriteListeningPorts(time.Now(), goStore.PortSnapshot()); err != nil {
			logger.Error("spooling listening ports", "error", err)
		}
		// Build facts are keyed by image digest and immutable for it, so each is
		// written once and marked only after the write succeeded; a failed write
		// is retried on the next flush (ADR 0017).
		for _, b := range goStore.PendingBuilds() {
			if err := spool.WriteGoBuild(b); err != nil {
				logger.Error("spooling go build", "image_digest", b.ImageDigest, "error", err)
				continue
			}
			goStore.MarkBuildWritten(b.ImageDigest)
		}
	}

	// Workload and node metadata are derived from the watchers' live indexes on
	// each flush rather than accumulated from events: a snapshot is the current
	// state of the cluster, so it drops what the cluster dropped and needs no
	// state of its own (ADR 0003, loss-harmless). Both are superseding batches
	// under a fixed key, and neither carries anything that orders it: each write
	// replaces the previous file, so there is never a second version to rank
	// (ADR 0027).
	flushMetadata := func() {
		if spool == nil {
			return
		}
		// One capture instant for both payloads of a flush: they describe the
		// same cluster state and are joined against each other downstream.
		capturedAt := time.Now()
		records := metadata.Aggregate(podWatcher.Pods(), podWatcher.UpdateStrategies())
		if err := spool.WriteWorkloadMetadata(capturedAt, records); err != nil {
			logger.Error("spooling workload metadata", "error", err)
		}
		nodes := nodeWatcher.Nodes()
		if err := spool.WriteNodeMetadata(capturedAt, nodes); err != nil {
			logger.Error("spooling node metadata", "error", err)
		}
		// Revisions ride the same instant: they are joined against workload
		// metadata under the same workload key, and two capture times would let
		// a consumer pair a revision with a shape from a different moment
		// (ADR 0030).
		revisionRecords := revisions.Aggregate(podWatcher.ReplicaSets())
		if err := spool.WriteWorkloadRevisions(capturedAt, revisionRecords); err != nil {
			logger.Error("spooling deployment revisions", "error", err)
		}
		// Policy rides the same instant for the same reason: what bounds a
		// workload is only meaningful beside the shape it bounds, and two
		// capture times would let a consumer pair a budget with a replica
		// count from a different moment (ADR 0032).
		policies, policyGaps := podWatcher.WorkloadPolicies()
		if err := spool.WriteWorkloadPolicy(capturedAt, policies, policyGaps); err != nil {
			logger.Error("spooling workload policy", "error", err)
		}
		clusterPolicy, clusterGaps := podWatcher.ClusterPolicy()
		if err := spool.WriteClusterPolicy(capturedAt, clusterPolicy, clusterGaps); err != nil {
			logger.Error("spooling cluster policy", "error", err)
		}
		// The restart counters ride the metadata flush rather than the journal
		// one, because they are a snapshot and not a window: they are read from
		// the live pod index at an instant, exactly as workload metadata is, and
		// they carry that same instant so a reading can be laid beside the
		// replica counts it belongs to (ADR 0034).
		restartCounters := podWatcher.RestartCounters()
		if err := spool.WriteRestartCounters(capturedAt, restartCounters); err != nil {
			logger.Error("spooling restart counters", "error", err)
		}
		// A source the agent was not permitted to read is not an error it can
		// fix, and not a reason to stop collecting everything else. It is
		// logged once per flush and declared in the payload (ADR 0033).
		if len(policyGaps) > 0 || len(clusterGaps) > 0 {
			logger.Warn("policy sources unavailable at this capture",
				"workload_policy", policyGaps, "cluster_policy", clusterGaps)
		}
		logger.Info("metadata flushed",
			"captured_at", capturedAt,
			"workload_records", len(records),
			"nodes", len(nodes),
			"revisions", len(revisionRecords),
			"policy_records", len(policies),
			"policy_namespaces", len(clusterPolicy.Namespaces),
			"restart_counters", len(restartCounters),
		)
	}

	// Restart windows ride the same cadence: the open window is re-written each
	// flush (it supersedes under its own key) and a window that has ended is
	// written one last time and dropped from memory. Same goroutine as the
	// metadata flush.
	flushRestarts := func() {
		if spool == nil {
			return
		}
		records := append(restartJournal.CloseBefore(time.Now()), restartJournal.Snapshots()...)
		if len(records) == 0 {
			return // a cluster where nothing restarted writes nothing
		}
		if err := spool.WriteContainerRestarts(records); err != nil {
			logger.Error("spooling container restarts", "error", err)
			return
		}
		logger.Info("container restarts flushed", "records", len(records))
	}

	flushDisruptions := func() {
		if spool == nil {
			return
		}
		records := append(disruptionJournal.CloseBefore(time.Now()), disruptionJournal.Snapshots()...)
		if len(records) == 0 {
			return // a cluster where nothing was preempted or evicted writes nothing
		}
		if err := spool.WritePodDisruptions(records); err != nil {
			logger.Error("spooling pod disruptions", "error", err)
			return
		}
		logger.Info("pod disruptions flushed", "records", len(records))
	}

	flushJobRuns := func() {
		if spool == nil {
			return
		}
		records := append(jobJournal.CloseBefore(time.Now()), jobJournal.Snapshots()...)
		if len(records) == 0 {
			return // a cluster with no batch workloads writes nothing
		}
		if err := spool.WriteJobRuns(records); err != nil {
			logger.Error("spooling job runs", "error", err)
			return
		}
		logger.Info("job runs flushed", "records", len(records))
	}

	// What the controller itself learns about arriving profiles: how many nodes
	// delivered, and how many it could not attribute to a pod. Declared here
	// because the coverage report reads them and the intake below writes them
	// (ADR 0060 §3).
	var profilesReceived, profilesUnjoined atomic.Uint64

	// Excluded pods are reported as aggregate counts only, never by name
	// (docs/security.md §8). Inventory counters are aggregate too: no identity
	// of an unjoined fact appears, only a count (CLAUDE.md invariant 6).
	logCoverage := func() {
		c := filter.Snapshot()
		attrs := []any{
			"pods_observed", c.PodsObserved,
			"excluded_namespace_filter", c.ExcludedNamespaceFilter,
			"excluded_namespace_annotation", c.ExcludedNamespaceAnnotation,
			"excluded_workload_annotation", c.ExcludedWorkloadAnnotation,
			"excluded_pod_annotation", c.ExcludedPodAnnotation,
			// Jobs are counted apart from pods: one Job makes many pods, so a
			// shared number would answer neither question (ADR 0029).
			"jobs_observed", c.JobsObserved,
			"jobs_excluded_namespace_filter", c.JobsExcludedNamespaceFilter,
			"jobs_excluded_namespace_annotation", c.JobsExcludedNamespaceAnnotation,
			"jobs_excluded_workload_annotation", c.JobsExcludedWorkloadAnnotation,
			"jobs_excluded_annotation", c.JobsExcludedAnnotation,
			// The blind spot, not an exclusion: pods collected without their
			// workload-level opt-out being checked (ADR 0028). The first is a
			// standing property of a cluster running operators the agent does
			// not read; the second should stay at zero.
			"workload_unknown_kind", c.WorkloadUnknownKind,
			"workload_not_cached", c.WorkloadNotCached,
			"usage_signals", usagePoller.Signals(),
		}
		// Placement terms the reduction refused to carry (ADR 0031). Not an
		// exclusion — the workload is collected — but it is something the
		// customer's manifests contain and the payload does not, so it is
		// counted here rather than left silent.
		if pd := podWatcher.PlacementDrops(); pd.Values != 0 || pd.Terms != 0 {
			attrs = append(attrs,
				"placement_values_dropped", pd.Values,
				"placement_terms_dropped", pd.Terms,
			)
		}
		if goStore != nil {
			ic := goStore.Counters()
			attrs = append(attrs,
				"go_inventory_records", ic.Records,
				"go_versions", ic.GoVersions,
				"go_pgo_builds", ic.PGOBuilds,
				"go_builds", ic.Builds,
				"go_facts_received", ic.FactsReceived,
				"go_facts_joined", ic.FactsJoined,
				"go_facts_unjoined", ic.FactsUnjoined,
				"go_facts_undigested", ic.FactsUndigested,
				"go_nodes_reported", ic.NodesReported,
			)
		}
		logger.Info("coverage", attrs...)

		// The same counters as a payload. The log serves whoever is reading the
		// agent's own output; this serves the report, which otherwise cannot
		// tell an empty cluster from a blind agent (ADR 0054).
		if spool == nil {
			return
		}
		var inv *inventory.Counters
		var scan *inventory.ScanCoverage
		var ebpf *inventory.ProfileCoverage
		if goStore != nil {
			ic := goStore.Counters()
			sc := goStore.ScanCoverage()
			inv, scan = &ic, &sc
			// Present once a node has said what its profiler did; the two
			// controller-side counts are added here because only this side
			// sees a profile arrive (ADR 0060 §3).
			if pc := goStore.ProfileCoverage(); pc.Nodes > 0 {
				pc.ProfilesReceived = profilesReceived.Load()
				pc.ProfilesUnjoined = profilesUnjoined.Load()
				ebpf = &pc
			}
		}
		agentInfo := sink.AgentInfo{
			Version:      version,
			Config:       configShape,
			UsageSignals: usagePoller.Signals(),
		}
		var probeCoverage *pprofprobe.Coverage
		if prober != nil {
			pc := prober.Snapshot()
			probeCoverage = &pc
		}
		var pullCoverage *pprofpull.Coverage
		if puller != nil {
			pc := puller.Snapshot()
			pullCoverage = &pc
		}
		if err := spool.WriteCollectionCoverage(time.Now(), startedAt, agentInfo,
			podWatcher.SourceHealths(), c, podWatcher.PlacementDrops(), inv, scan,
			ebpf, probeCoverage, pullCoverage); err != nil {
			logger.Error("spooling collection coverage", "error", err)
		}
	}

	// Watchers run until ctx is canceled; a failing watcher must bring the
	// agent down rather than leave it half-blind.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if prober != nil {
		candidates := func() []pprofprobe.Candidate {
			return pprofprobe.Candidates(goStore.PortSnapshot(), goStore.PprofBuilds())
		}
		go prober.Run(ctx, pprofProbeInterval, candidates)
		if puller != nil {
			go puller.Run(ctx, pprofPullInterval, func() []pprofprobe.Candidate {
				return prober.Confirmed(candidates())
			})
		}
	}

	go func() {
		ticker := time.NewTicker(coverageInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logCoverage()
				flushMetadata()
				flushInventory()
				flushRestarts()
				flushDisruptions()
				flushJobRuns()
				// Unconditionally, on the agent's own cadence rather than the
				// usage poller's: the spool's bounds must hold on a cluster
				// where no usage record is ever written, which is exactly the
				// cluster where they used to not hold at all (ADR 0042).
				if spool != nil {
					if err := spool.Sweep(time.Now()); err != nil {
						logger.Error("sweeping the spool failed", "error", err)
					}
				}
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
	// node tokens locally against the cluster JWKS (ADR 0010) and joining each
	// report into the Go inventory.
	if cfg.NodeIntake.Enabled {
		onReport := func(id nodeauth.Identity, report nodescan.Report) {
			// The resolver is bound to the node the token names, not to the
			// node the report names — the handler has already required the two
			// to agree, and this is the half that survives if that check is
			// ever loosened (ADR 0040).
			goStore.Ingest(report, podContainerResolver{pw: podWatcher, node: id.Node})
			ic := goStore.Counters()
			logger.Info("node report received",
				"from_subject", id.Subject,
				"node", id.Node,
				"binaries", len(report.Binaries),
				"go_found", report.Counters.GoFound,
				"filtered_infra", report.Counters.FilteredInfra,
				"unreadable", report.Counters.Unreadable,
				"inventory_records", ic.Records,
				"facts_unjoined", ic.FactsUnjoined,
			)
		}
		// Captured profiles arrive on a second endpoint. The node ships already
		// allow-list-filtered pprof bytes with only what it can see locally
		// (pod UID / container ID); the controller joins those to a workload via
		// PodWatcher — exactly as it joins inventory facts — and spools the
		// profile. An unjoinable profile (informer lag, or a pod outside the
		// controller's filters) is counted and dropped, never guessed (ADR 0010
		// §5, ADR 0011 §6).
		onProfile := func(id nodeauth.Identity, report nodeintake.ProfileReport) {
			ns, workload, container, digest, ok := podWatcher.LookupContainerOnNode(types.UID(report.PodUID), report.ContainerID, id.Node)
			if !ok {
				n := profilesUnjoined.Add(1)
				logger.Warn("profile dropped: pod/container not in inventory for the reporting node",
					"from_subject", id.Subject, "node", id.Node, "profiles_unjoined", n)
				return
			}
			key := sink.ProfileKey{
				Namespace:    ns,
				Workload:     workload.Name,
				Container:    container,
				ImageDigest:  digest,
				CaptureStart: report.CaptureStart,
				CaptureEnd:   report.CaptureEnd,
			}
			if spool != nil {
				if err := spool.WriteProfile(key, report.Pprof); err != nil {
					logger.Error("writing profile to spool failed", "error", err,
						"namespace", ns, "workload", workload.Name)
					return
				}
			}
			n := profilesReceived.Add(1)
			logger.Info("profile received",
				"from_subject", id.Subject, "node", id.Node,
				"namespace", ns, "workload", workload.Name, "container", container,
				"profiles_received", n)
		}

		// The targeter expands the publisher's top-N workloads to the container
		// IDs of their pods on the querying node (the node can't do that — no API
		// access). A nil-safe interface keeps the route absent when profiling is
		// off (a typed nil pointer would make a non-nil interface).
		var targeter nodeintake.NodeTargeter
		if targetsPublisher != nil {
			targeter = nodeTargeter{publisher: targetsPublisher, pw: podWatcher}
		}
		intake, err := buildNodeIntake(logger, restConfig, cfg.NodeIntake, onReport, onProfile, targeter, podWatcher)
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
	// A final flush lands whatever arrived since the last periodic write: an
	// inventory fact joined, a container restarted, a pod preempted. Without it
	// a graceful shutdown silently drops up to one coverage interval of the
	// journals, which is exactly the minute a rolling upgrade or a node drain
	// tends to be interesting in. The coverage goroutine has returned (ctx is
	// canceled), so this is the only writer; the accumulators serialize
	// regardless.
	flushInventory()
	flushRestarts()
	flushDisruptions()
	flushJobRuns()
	return errors.Join(errs...)
}

// buildNodeIntake constructs the node-intake receiver: a token verifier backed
// by the cluster JWKS (fetched over the API server's in-cluster transport, no
// TokenReview) and an HTTP server that decodes node reports and hands each to
// onReport (which joins it into the Go inventory, ADR 0010).
func buildNodeIntake(logger *slog.Logger, restConfig *rest.Config, cfg config.NodeIntake, onReport func(nodeauth.Identity, nodescan.Report), onProfile func(nodeauth.Identity, nodeintake.ProfileReport), targeter nodeintake.NodeTargeter, scoper nodeintake.NodeScoper) (*nodeintake.Server, error) {
	addr := cfg.ListenAddress
	if addr == "" {
		addr = config.DefaultNodeIntakeListenAddress
	}
	audience := cfg.Audience
	if audience == "" {
		audience = config.DefaultNodeIntakeAudience
	}
	// No default for the subject, and no starting without it (ADR 0040). The
	// two fields above degrade safely when unset; this one degraded to "accept
	// any subject that satisfies the audience", and the audience is not a
	// secret — the kubelet mints a token for any audience a pod asks for, with
	// no RBAC on serviceaccounts/token. So an unset subject meant any pod in
	// the cluster could speak as the node role. The chart has always rendered
	// it; what changes is that an install which does not is a startup failure
	// rather than a silently open door.
	if cfg.ExpectedSubject == "" {
		return nil, fmt.Errorf("nodeIntake.expectedSubject is required when nodeIntake.enabled is true: " +
			"without it any token bearing the audience is accepted, and any pod can obtain one")
	}

	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building JWKS http client: %w", err)
	}
	keys := &nodeauth.CachingKeySource{
		Source: &nodeauth.HTTPKeySource{IssuerBaseURL: restConfig.Host, Client: httpClient},
	}
	verifier := nodeauth.NewVerifier(keys, audience, nodeauth.WithExpectedSubject(cfg.ExpectedSubject))

	handler := nodeintake.NewHandler(verifier, logger, onReport)
	profileHandler := nodeintake.NewProfileHandler(verifier, logger, onProfile)
	// The targets query endpoint is mounted only when a source is configured
	// (profiling enabled); a nil interface leaves the route absent.
	var targetsHandler http.Handler
	if targeter != nil {
		targetsHandler = nodeintake.NewTargetsHandler(verifier, logger, targeter)
	}
	// The scope endpoint is always mounted when node intake is on: without it a
	// node cannot know which pods pass the customer's filters, and so — failing
	// closed — scans nothing at all (ADR 0015).
	scopeHandler := nodeintake.NewScopeHandler(verifier, logger, scoper)
	logger.Info("node intake enabled", "addr", addr, "audience", audience,
		"expected_subject", cfg.ExpectedSubject, "targeting", targeter != nil)
	return nodeintake.NewServer(addr, handler, profileHandler, targetsHandler, scopeHandler, logger), nil
}

// nodeTargeter answers a node's targets query: it intersects the publisher's
// top-N collected workloads with the containers PodWatcher sees on that node, and
// returns their container IDs. This is where a cluster-wide workload ranking
// becomes a node-actionable set (ADR 0011 §3): the node profiles the processes
// whose cgroup container ID is returned here.
type nodeTargeter struct {
	publisher *targeting.Publisher
	pw        *collector.PodWatcher
}

func (t nodeTargeter) ContainersForNode(node string) []string {
	top := t.publisher.Snapshot()
	if len(top) == 0 {
		return nil
	}
	want := make(map[targeting.Target]struct{}, len(top))
	for _, w := range top {
		want[w] = struct{}{}
	}
	var out []string
	for _, c := range t.pw.ContainersOnNode(node) {
		key := targeting.Target{Namespace: c.Namespace, WorkloadKind: c.Workload.Kind, WorkloadName: c.Workload.Name}
		if _, ok := want[key]; ok {
			out = append(out, c.ContainerID)
		}
	}
	return out
}

// podContainerResolver adapts PodWatcher's container lookup to the inventory
// join's resolver interface (ADR 0010). It is built per report and carries the
// reporting node, taken from that report's verified token, so a fact can only
// join to a pod on the node that sent it (ADR 0040).
type podContainerResolver struct {
	pw   *collector.PodWatcher
	node string
}

func (r podContainerResolver) LookupContainer(podUID types.UID, containerID string) (inventory.Resolved, bool) {
	ns, workload, container, digest, ok := r.pw.LookupContainerOnNode(podUID, containerID, r.node)
	if !ok {
		return inventory.Resolved{}, false
	}
	return inventory.Resolved{
		Namespace:    ns,
		WorkloadKind: workload.Kind,
		WorkloadName: workload.Name,
		Container:    container,
		ImageDigest:  digest,
	}, true
}
