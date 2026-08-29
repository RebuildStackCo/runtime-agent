package inventory

import (
	"maps"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// testSince is the fixed base of every cumulative counter in these tests.
var testSince = time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)

// fakeResolver resolves a fixed set of (pod UID, container ID) pairs; anything
// else is unjoinable.
type fakeResolver map[string]Resolved

func (f fakeResolver) LookupContainer(podUID types.UID, containerID string) (Resolved, bool) {
	r, ok := f[string(podUID)+"/"+containerID]
	return r, ok
}

func binary(podUID, containerID, goVersion, module string, pgo bool) nodescan.BinaryInfo {
	return nodescan.BinaryInfo{
		PodUID:      podUID,
		ContainerID: containerID,
		GoVersion:   goVersion,
		MainModule:  module,
		PGO:         pgo,
	}
}

// withDeps attaches dependencies by path, at one version, for the tests whose
// subject is the join rather than the module list itself.
func withDeps(b nodescan.BinaryInfo, paths ...string) nodescan.BinaryInfo {
	mods := make([]nodescan.Module, 0, len(paths))
	for _, path := range paths {
		mods = append(mods, nodescan.Module{Path: path, Version: "v1.0.0"})
	}
	b.Dependencies = mods
	return b
}

func withModules(b nodescan.BinaryInfo, mods ...nodescan.Module) nodescan.BinaryInfo {
	b.Dependencies = mods
	return b
}

func TestIngestKeepsBuildFactsPerBuild(t *testing.T) {
	resolver := fakeResolver{
		"pod-web/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{
		withModules(binary("pod-web", "cid-web", "go1.26.1", "github.com/acme/web", true),
			nodescan.Module{Path: "golang.org/x/sync", Version: "v0.9.0"},
			nodescan.Module{Path: "github.com/cespare/xxhash/v2", Version: "v2.3.0"},
			nodescan.Module{Path: "golang.org/x/sync", Version: "v0.9.0"},
			// The same module at two versions is one build linking one of them
			// and recording the other as replaced; both rows survive, because
			// collapsing them would invent a version the build does not have.
			nodescan.Module{Path: "golang.org/x/sync", Version: "v0.8.0", Replaced: true},
		),
	}}, resolver)

	pending := s.PendingBuilds()
	if len(pending) != 1 {
		t.Fatalf("got %d build fact sets, want 1", len(pending))
	}
	got := pending[0]
	if got.ImageDigest != "sha256:web" || got.GoVersion != "go1.26.1" || got.MainModule != "github.com/acme/web" {
		t.Errorf("build identity = %+v, want the web build", got)
	}
	// Sorted by path then version and deduplicated, so the payload bytes are
	// deterministic.
	want := []nodescan.Module{
		{Path: "github.com/cespare/xxhash/v2", Version: "v2.3.0"},
		{Path: "golang.org/x/sync", Version: "v0.8.0", Replaced: true},
		{Path: "golang.org/x/sync", Version: "v0.9.0"},
	}
	if !slices.Equal(got.Modules, want) {
		t.Errorf("modules = %v, want %v", got.Modules, want)
	}
}

// The settings the node kept ride into the build facts unchanged: the allow-list
// runs on the node, before the channel (ADR 0019), so the store neither filters
// nor interprets them.
func TestIngestCarriesBuildSettings(t *testing.T) {
	resolver := fakeResolver{
		"pod-web/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	b := binary("pod-web", "cid-web", "go1.26.1", "github.com/acme/web", false)
	b.Settings = map[string]string{"GOARCH": "arm64", "vcs.revision": "a5edd4b", "vcs.modified": "true"}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{b}}, resolver)

	pending := s.PendingBuilds()
	if len(pending) != 1 {
		t.Fatalf("got %d build fact sets, want 1", len(pending))
	}
	if !maps.Equal(pending[0].Settings, b.Settings) {
		t.Errorf("settings = %v, want %v", pending[0].Settings, b.Settings)
	}

	// The returned map must not alias store state: a caller writing into what it
	// got back would corrupt the facts of a build that is immutable by contract.
	pending[0].Settings["GOARCH"] = "tampered"
	if again := s.PendingBuilds(); again[0].Settings["GOARCH"] != "arm64" {
		t.Errorf("GOARCH = %q after the caller mutated its copy, want arm64", again[0].Settings["GOARCH"])
	}
}

// The GODEBUG defaults ride the same way, and need their own aliasing check:
// they are a second map on a struct copied out of the store (ADR 0050).
func TestIngestCarriesGoDebugDefaults(t *testing.T) {
	resolver := fakeResolver{
		"pod-web/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	b := binary("pod-web", "cid-web", "go1.26.1", "github.com/acme/web", false)
	b.GoDebug = map[string]string{"containermaxprocs": "0", "updatemaxprocs": "0"}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{b}}, resolver)

	pending := s.PendingBuilds()
	if len(pending) != 1 {
		t.Fatalf("got %d build fact sets, want 1", len(pending))
	}
	if !maps.Equal(pending[0].GoDebug, b.GoDebug) {
		t.Errorf("godebug = %v, want %v", pending[0].GoDebug, b.GoDebug)
	}

	pending[0].GoDebug["containermaxprocs"] = "tampered"
	if again := s.PendingBuilds(); again[0].GoDebug["containermaxprocs"] != "0" {
		t.Errorf("containermaxprocs = %q after the caller mutated its copy, want 0", again[0].GoDebug["containermaxprocs"])
	}
}

func TestBuildFactsDedupAcrossReplicasAndSurviveUntilMarked(t *testing.T) {
	// Two replicas of the same build on two nodes, plus a second build: one
	// dependency set per digest, not per replica.
	resolver := fakeResolver{
		"pod-a/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
		"pod-b/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
		"pod-c/cid": {Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "app", ImageDigest: "sha256:index"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{
		withDeps(binary("pod-a", "cid", "go1.26.1", "github.com/acme/web", false), "golang.org/x/sync"),
	}}, resolver)
	s.Ingest(nodescan.Report{Node: "n2", Binaries: []nodescan.BinaryInfo{
		withDeps(binary("pod-b", "cid", "go1.26.1", "github.com/acme/web", false), "golang.org/x/sync"),
		withDeps(binary("pod-c", "cid", "go1.26.1", "github.com/acme/index", false), "golang.org/x/time"),
	}}, resolver)

	pending := s.PendingBuilds()
	if len(pending) != 2 {
		t.Fatalf("got %d build fact sets, want 2 (one per digest)", len(pending))
	}
	// Sorted by digest so the flush order — and any partial-failure retry — is
	// deterministic.
	if pending[0].ImageDigest != "sha256:index" || pending[1].ImageDigest != "sha256:web" {
		t.Errorf("pending order = %s, %s; want sorted by digest", pending[0].ImageDigest, pending[1].ImageDigest)
	}

	// A set that was not marked is still pending: an unacknowledged write must
	// be retried, not lost.
	s.MarkBuildWritten("sha256:index")
	pending = s.PendingBuilds()
	if len(pending) != 1 || pending[0].ImageDigest != "sha256:web" {
		t.Fatalf("after marking one, pending = %+v, want only sha256:web", pending)
	}

	// Marked sets are never offered again, not even after the same build is
	// re-reported by a later scan.
	s.MarkBuildWritten("sha256:web")
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{
		withDeps(binary("pod-a", "cid", "go1.26.1", "github.com/acme/web", false), "golang.org/x/sync"),
	}}, resolver)
	if pending := s.PendingBuilds(); len(pending) != 0 {
		t.Errorf("pending after marking both = %+v, want none", pending)
	}
}

func TestJoinedFactWithoutDigestIsCountedNotDropped(t *testing.T) {
	// The controller has the pod but no container status yet, so there is no
	// digest to key a build by. The record still lands; the dependency set
	// cannot, and that must show up as a number rather than as silence.
	resolver := fakeResolver{
		"pod-web/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{
		withDeps(binary("pod-web", "cid-web", "go1.26.1", "github.com/acme/web", false), "golang.org/x/sync"),
	}}, resolver)

	if recs := s.Snapshot(); len(recs) != 1 {
		t.Fatalf("got %d records, want 1 — the record does not depend on the digest", len(recs))
	}
	if pending := s.PendingBuilds(); len(pending) != 0 {
		t.Errorf("pending = %+v, want none: build facts with no digest cannot be keyed", pending)
	}
	if c := s.Coverage(); c.FactsUndigested != 1 || c.FactsJoined != 1 {
		t.Errorf("coverage = %+v, want joined 1 / undigested 1", c)
	}
}

func TestCoverageCountsReportingNodes(t *testing.T) {
	resolver := fakeResolver{
		"p1/c1": {Namespace: "a", WorkloadKind: "Deployment", WorkloadName: "one", Container: "app", ImageDigest: "sha256:one"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{binary("p1", "c1", "go1.26.1", "example.com/one", false)}}, resolver)
	// A node that scanned and found nothing still reported: it counts, which is
	// what separates "no Go workloads there" from "that node never checked in".
	s.Ingest(nodescan.Report{Node: "n2"}, resolver)
	// The same node reporting again is still one node.
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{binary("p1", "c1", "go1.26.1", "example.com/one", false)}}, resolver)
	// A report with no node name (a malformed or pre-ADR-0010 sender) counts no
	// node rather than an empty one.
	s.Ingest(nodescan.Report{}, resolver)

	c := s.Coverage()
	if !c.Since.Equal(testSince) {
		t.Errorf("since = %v, want %v — the base of the counters travels with them", c.Since, testSince)
	}
	if c.NodesReported != 2 {
		t.Errorf("nodes_reported = %d, want 2", c.NodesReported)
	}
	if c.FactsReceived != 2 || c.FactsJoined != 2 || c.FactsUnjoined != 0 {
		t.Errorf("coverage = %+v, want received 2 / joined 2 / unjoined 0", c)
	}
}

func TestRetainForgetsDepartedWorkloads(t *testing.T) {
	resolver := fakeResolver{
		"pod-web/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
		"pod-idx/cid": {Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "app", ImageDigest: "sha256:index"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{
		withDeps(binary("pod-web", "cid", "go1.26.1", "github.com/acme/web", false), "golang.org/x/sync"),
		withDeps(binary("pod-idx", "cid", "go1.26.1", "github.com/acme/index", false), "golang.org/x/time"),
	}}, resolver)

	web := Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"}

	// The index workload is deleted from the cluster; the web one stays, and
	// stays without any fresh node fact — a live workload must not need to be
	// re-reported to survive a flush.
	ev := s.Retain([]Key{web})
	if ev.Records != 1 || ev.Builds != 1 {
		t.Errorf("evicted = %+v, want 1 record / 1 build", ev)
	}
	recs := s.Snapshot()
	if len(recs) != 1 || recs[0].Key != web {
		t.Fatalf("records = %+v, want only %+v", recs, web)
	}

	// The departed workload's dependency set goes with it; the surviving one
	// stays pending because it was never marked written.
	pending := s.PendingBuilds()
	if len(pending) != 1 || pending[0].ImageDigest != "sha256:web" {
		t.Errorf("pending = %+v, want only the surviving build", pending)
	}
	if c := s.Counters(); c.Builds != 1 {
		t.Errorf("builds = %d, want 1", c.Builds)
	}
}

func TestRetainOnEmptyClusterEmptiesTheInventory(t *testing.T) {
	// A cluster where nothing passes the filters has an empty inventory. This
	// is the case an "if live is empty, skip" guard would break: it would pin
	// the last non-empty snapshot forever.
	resolver := fakeResolver{
		"pod-web/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{
		binary("pod-web", "cid", "go1.26.1", "github.com/acme/web", false),
	}}, resolver)

	if ev := s.Retain(nil); ev.Records != 1 {
		t.Errorf("evicted = %+v, want 1 record", ev)
	}
	if recs := s.Snapshot(); len(recs) != 0 {
		t.Errorf("records = %+v, want none", recs)
	}
}

// A workload that leaves and comes back must be shippable again. Dropping the
// dependency set without dropping its "already written" mark would leave the
// returning build permanently unsent — silently, with nothing failing.
func TestRetainClearsTheWrittenMarkWithTheBuildFacts(t *testing.T) {
	resolver := fakeResolver{
		"pod-web/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	s := NewStore(testSince)
	report := nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{
		withDeps(binary("pod-web", "cid", "go1.26.1", "github.com/acme/web", false), "golang.org/x/sync"),
	}}
	s.Ingest(report, resolver)
	s.MarkBuildWritten("sha256:web")
	if pending := s.PendingBuilds(); len(pending) != 0 {
		t.Fatalf("pending after marking = %+v, want none", pending)
	}

	s.Retain(nil)              // the workload is deleted
	s.Ingest(report, resolver) // and comes back on the next scan

	pending := s.PendingBuilds()
	if len(pending) != 1 || pending[0].ImageDigest != "sha256:web" {
		t.Fatalf("pending after the build returned = %+v, want it offered again", pending)
	}
}

// Only the records' own digests keep a dependency set alive: a build no record
// points at any more is gone even if another workload survives.
func TestRetainKeepsBuildFactsSharedByAnotherWorkload(t *testing.T) {
	// Two workloads running the same image, so one record's departure must not
	// take the shared dependency set with it.
	resolver := fakeResolver{
		"pod-a/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:shared"},
		"pod-b/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "api", Container: "app", ImageDigest: "sha256:shared"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{
		withDeps(binary("pod-a", "cid", "go1.26.1", "github.com/acme/web", false), "golang.org/x/sync"),
		withDeps(binary("pod-b", "cid", "go1.26.1", "github.com/acme/web", false), "golang.org/x/sync"),
	}}, resolver)

	api := Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "api", Container: "app"}
	ev := s.Retain([]Key{api})
	if ev.Records != 1 {
		t.Errorf("evicted records = %d, want 1", ev.Records)
	}
	if ev.Builds != 0 {
		t.Errorf("evicted builds = %d, want 0 — the surviving workload still runs that image", ev.Builds)
	}
	if pending := s.PendingBuilds(); len(pending) != 1 {
		t.Errorf("pending = %+v, want the shared build still held", pending)
	}
}

func TestRetainNodesForgetsDepartedNodes(t *testing.T) {
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "n1"}, fakeResolver{})
	s.Ingest(nodescan.Report{Node: "n2"}, fakeResolver{})
	s.Ingest(nodescan.Report{Node: "n3"}, fakeResolver{})

	// The cluster scaled down to one node. Reporting three would exceed the
	// fleet in node_metadata and hide a DaemonSet gap instead of showing it.
	s.RetainNodes([]string{"n2"})
	if c := s.Coverage(); c.NodesReported != 1 {
		t.Errorf("nodes_reported = %d, want 1", c.NodesReported)
	}
}

func TestLiveKeysCoversEveryContainerIncludingInit(t *testing.T) {
	pods := []collector.PodInfo{
		{
			Namespace: "shop",
			Workload:  collector.WorkloadRef{Kind: "Deployment", Name: "web"},
			Containers: []collector.Container{
				{Name: "app"},
				{Name: "istio-proxy"},
				// A native sidecar is declared as an init container and runs for
				// the pod's whole life; excluding it would evict its record on
				// every flush.
				{Name: "vault-agent", Init: true},
			},
		},
		{
			Namespace:  "search",
			Workload:   collector.WorkloadRef{Kind: "StatefulSet", Name: "index"},
			Containers: []collector.Container{{Name: "app"}},
		},
	}
	got := LiveKeys(pods)
	want := []Key{
		{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"},
		{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "istio-proxy"},
		{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "vault-agent"},
		{Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "app"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("live keys = %+v, want %+v", got, want)
	}
}

// Replicas of one workload produce the same key; a record must survive as long
// as any replica does.
func TestRetainSurvivesWhileAnyReplicaLives(t *testing.T) {
	resolver := fakeResolver{
		"pod-a/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{
		binary("pod-a", "cid", "go1.26.1", "github.com/acme/web", false),
	}}, resolver)

	// Two replicas of the same workload: the key repeats in the live set.
	live := LiveKeys([]collector.PodInfo{
		{Namespace: "shop", Workload: collector.WorkloadRef{Kind: "Deployment", Name: "web"}, Containers: []collector.Container{{Name: "app"}}},
		{Namespace: "shop", Workload: collector.WorkloadRef{Kind: "Deployment", Name: "web"}, Containers: []collector.Container{{Name: "app"}}},
	})
	if ev := s.Retain(live); ev.Records != 0 {
		t.Errorf("evicted = %+v, want nothing", ev)
	}
	if recs := s.Snapshot(); len(recs) != 1 {
		t.Errorf("records = %+v, want the workload kept", recs)
	}
}

func TestIngestJoinsFacts(t *testing.T) {
	resolver := fakeResolver{
		"pod-web/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{
		Node:     "node-1",
		Binaries: []nodescan.BinaryInfo{binary("pod-web", "cid-web", "go1.26.1", "github.com/acme/web", true)},
	}, resolver)

	recs := s.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	want := GoRecord{
		Key:         Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"},
		GoVersion:   "go1.26.1",
		ModulePath:  "github.com/acme/web",
		ImageDigest: "sha256:web",
		PGO:         true,
	}
	if r != want {
		t.Errorf("record = %+v, want %+v", r, want)
	}
}

func TestIngestCountsUnjoined(t *testing.T) {
	resolver := fakeResolver{} // resolves nothing
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{
		Binaries: []nodescan.BinaryInfo{
			binary("pod-x", "cid-x", "go1.26.1", "example.com/x", false),
			binary("pod-y", "cid-y", "go1.26.1", "example.com/y", false),
		},
	}, resolver)

	if recs := s.Snapshot(); len(recs) != 0 {
		t.Fatalf("got %d records for unjoinable facts, want 0", len(recs))
	}
	c := s.Counters()
	if c.FactsReceived != 2 || c.FactsJoined != 0 || c.FactsUnjoined != 2 {
		t.Errorf("counters = %+v, want received 2 / joined 0 / unjoined 2", c)
	}
}

func TestIngestDedupsAcrossReplicas(t *testing.T) {
	// Two pods (two nodes/replicas) of the same workload container resolve to
	// the same key; they collapse to one record.
	resolver := fakeResolver{
		"pod-a/cid-a": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
		"pod-b/cid-b": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{binary("pod-a", "cid-a", "go1.26.1", "github.com/acme/web", true)}}, resolver)
	s.Ingest(nodescan.Report{Node: "n2", Binaries: []nodescan.BinaryInfo{binary("pod-b", "cid-b", "go1.26.1", "github.com/acme/web", true)}}, resolver)

	if recs := s.Snapshot(); len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (replicas dedup)", len(recs))
	}
	c := s.Counters()
	if c.FactsJoined != 2 || c.Records != 1 {
		t.Errorf("counters = %+v, want joined 2 / records 1", c)
	}
}

func TestIngestLatestBuildWins(t *testing.T) {
	// A rollout: the same container reports a newer build. The record flips.
	resolver := fakeResolver{
		"pod-old/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:old"},
		"pod-new/cid": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:new"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Binaries: []nodescan.BinaryInfo{binary("pod-old", "cid", "go1.25.0", "github.com/acme/web", false)}}, resolver)
	s.Ingest(nodescan.Report{Binaries: []nodescan.BinaryInfo{binary("pod-new", "cid", "go1.26.1", "github.com/acme/web", true)}}, resolver)

	recs := s.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].GoVersion != "go1.26.1" || recs[0].ImageDigest != "sha256:new" || !recs[0].PGO {
		t.Errorf("record = %+v, want the newer build (go1.26.1, sha256:new, pgo)", recs[0])
	}
}

func TestCountersDistinctVersionsAndPGO(t *testing.T) {
	resolver := fakeResolver{
		"p1/c1": {Namespace: "a", WorkloadKind: "Deployment", WorkloadName: "one", Container: "app"},
		"p2/c2": {Namespace: "a", WorkloadKind: "Deployment", WorkloadName: "two", Container: "app"},
		"p3/c3": {Namespace: "a", WorkloadKind: "Deployment", WorkloadName: "three", Container: "app"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Binaries: []nodescan.BinaryInfo{
		binary("p1", "c1", "go1.26.1", "example.com/one", true),
		binary("p2", "c2", "go1.26.1", "example.com/two", false),
		binary("p3", "c3", "go1.25.0", "example.com/three", true),
	}}, resolver)

	c := s.Counters()
	if c.Records != 3 {
		t.Errorf("records = %d, want 3", c.Records)
	}
	if c.GoVersions != 2 {
		t.Errorf("distinct go versions = %d, want 2", c.GoVersions)
	}
	if c.PGOBuilds != 2 {
		t.Errorf("pgo builds = %d, want 2", c.PGOBuilds)
	}
}

func TestSnapshotSorted(t *testing.T) {
	resolver := fakeResolver{
		"p1/c1": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"},
		"p2/c2": {Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "app"},
		"p3/c3": {Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "sidecar"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Binaries: []nodescan.BinaryInfo{
		binary("p1", "c1", "go1.26.1", "example.com/web", false),
		binary("p2", "c2", "go1.26.1", "example.com/index", false),
		binary("p3", "c3", "go1.26.1", "example.com/index", false),
	}}, resolver)

	recs := s.Snapshot()
	// search/index/app, search/index/sidecar, shop/web/app
	want := []Key{
		{Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "app"},
		{Namespace: "search", WorkloadKind: "StatefulSet", WorkloadName: "index", Container: "sidecar"},
		{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app"},
	}
	for i, w := range want {
		if recs[i].Key != w {
			t.Errorf("record[%d].Key = %+v, want %+v", i, recs[i].Key, w)
		}
	}
}

// withPeak attaches the measured half of a node report to a binary.
func withPeak(b nodescan.BinaryInfo, peakBytes int64, cpus int) nodescan.BinaryInfo {
	b.PeakRSSBytes = peakBytes
	b.CPUsAllowed = cpus
	return b
}

func webResolver() fakeResolver {
	return fakeResolver{
		"pod-web-1/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
		"pod-web-2/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
}

// Replicas on two nodes merge into one record: the largest peak, the count of
// processes behind it, and the range of CPU counts they see.
func TestPeaksMergeAcrossReplicasAndNodes(t *testing.T) {
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{
		withPeak(binary("pod-web-1", "cid-web", "go1.26.1", "github.com/acme/web", false), 300<<20, 8),
	}}, webResolver())
	s.Ingest(nodescan.Report{Node: "node-2", Binaries: []nodescan.BinaryInfo{
		withPeak(binary("pod-web-2", "cid-web", "go1.26.1", "github.com/acme/web", false), 481<<20, 4),
	}}, webResolver())

	peaks := s.PeakSnapshot()
	if len(peaks) != 1 {
		t.Fatalf("peaks = %+v, want one record", peaks)
	}
	p := peaks[0]
	if p.PeakRSSBytes != 481<<20 || p.Processes != 2 {
		t.Errorf("peak/processes = %d/%d, want the larger of the two over two processes", p.PeakRSSBytes, p.Processes)
	}
	if p.CPUsAllowedMin != 4 || p.CPUsAllowedMax != 8 {
		t.Errorf("cpus allowed = %d..%d, want 4..8", p.CPUsAllowedMin, p.CPUsAllowedMax)
	}
	if p.ImageDigest != "sha256:web" {
		t.Errorf("image digest = %q; a peak belongs to the build that reached it", p.ImageDigest)
	}
}

// The failure this design exists to avoid: a deploy that fixes a leak must not
// inherit the old build's number. A node's contribution is replaced wholesale by
// its next report, and the digest keys the record (ADR 0052 §3).
func TestAPeakDoesNotOutliveTheProcessThatSetIt(t *testing.T) {
	leaky := fakeResolver{
		"pod-web-1/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:leaky"},
	}
	fixed := fakeResolver{
		"pod-web-1/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:fixed"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{
		withPeak(binary("pod-web-1", "cid-web", "go1.26.1", "github.com/acme/web", false), 900<<20, 8),
	}}, leaky)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{
		withPeak(binary("pod-web-1", "cid-web", "go1.26.1", "github.com/acme/web", false), 120<<20, 8),
	}}, fixed)

	peaks := s.PeakSnapshot()
	if len(peaks) != 1 {
		t.Fatalf("peaks = %+v, want only the build that is running", peaks)
	}
	if peaks[0].PeakRSSBytes != 120<<20 || peaks[0].ImageDigest != "sha256:fixed" {
		t.Errorf("peak = %d for %s; the previous build's number survived its pods",
			peaks[0].PeakRSSBytes, peaks[0].ImageDigest)
	}
	// And the count is processes, not scans.
	if peaks[0].Processes != 1 {
		t.Errorf("processes = %d after two reports of one process, want 1", peaks[0].Processes)
	}
}

// A node that leaves takes its contribution with it; the record goes when the
// last node stops standing behind it.
func TestPeaksLeaveWithTheirNode(t *testing.T) {
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{
		withPeak(binary("pod-web-1", "cid-web", "go1.26.1", "github.com/acme/web", false), 300<<20, 8),
	}}, webResolver())
	s.RetainNodes([]string{"node-2"})
	if peaks := s.PeakSnapshot(); len(peaks) != 0 {
		t.Errorf("peaks = %+v after the reporting node left, want none", peaks)
	}
}

// And with their workload, on the same signal the inventory records use.
func TestPeaksLeaveWithTheirWorkload(t *testing.T) {
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{
		withPeak(binary("pod-web-1", "cid-web", "go1.26.1", "github.com/acme/web", false), 300<<20, 8),
	}}, webResolver())
	ev := s.Retain(nil)
	if ev.Peaks != 1 {
		t.Errorf("evicted peaks = %d, want 1", ev.Peaks)
	}
	if peaks := s.PeakSnapshot(); len(peaks) != 0 {
		t.Errorf("peaks = %+v after the workload went, want none", peaks)
	}
}

// A binary whose status could not be read contributes nothing at all. Folding it
// in as a zero would claim a container reached no memory and would pull the CPU
// minimum to zero.
func TestABinaryWithoutAStatusReadContributesNothing(t *testing.T) {
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{
		binary("pod-web-1", "cid-web", "go1.26.1", "github.com/acme/web", false),
		withPeak(binary("pod-web-2", "cid-web", "go1.26.1", "github.com/acme/web", false), 300<<20, 8),
	}}, webResolver())

	peaks := s.PeakSnapshot()
	if len(peaks) != 1 {
		t.Fatalf("peaks = %+v, want one record", peaks)
	}
	if peaks[0].Processes != 1 || peaks[0].CPUsAllowedMin != 8 {
		t.Errorf("processes = %d, cpus min = %d; the unread process was counted",
			peaks[0].Processes, peaks[0].CPUsAllowedMin)
	}
}
