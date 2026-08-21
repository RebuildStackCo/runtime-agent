package inventory

import (
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

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

func withDeps(b nodescan.BinaryInfo, deps ...string) nodescan.BinaryInfo {
	b.Dependencies = deps
	return b
}

func TestIngestKeepsDependenciesPerBuild(t *testing.T) {
	resolver := fakeResolver{
		"pod-web/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	s := NewStore(testSince)
	s.Ingest(nodescan.Report{Node: "node-1", Binaries: []nodescan.BinaryInfo{
		withDeps(binary("pod-web", "cid-web", "go1.26.1", "github.com/acme/web", true),
			"golang.org/x/sync", "github.com/cespare/xxhash/v2", "golang.org/x/sync"),
	}}, resolver)

	pending := s.PendingDependencies()
	if len(pending) != 1 {
		t.Fatalf("got %d dependency sets, want 1", len(pending))
	}
	got := pending[0]
	if got.ImageDigest != "sha256:web" || got.GoVersion != "go1.26.1" || got.MainModule != "github.com/acme/web" {
		t.Errorf("build identity = %+v, want the web build", got)
	}
	// Sorted and deduplicated, so the payload bytes are deterministic.
	want := []string{"github.com/cespare/xxhash/v2", "golang.org/x/sync"}
	if !slices.Equal(got.Modules, want) {
		t.Errorf("modules = %v, want %v", got.Modules, want)
	}
}

func TestDependenciesDedupAcrossReplicasAndSurviveUntilMarked(t *testing.T) {
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

	pending := s.PendingDependencies()
	if len(pending) != 2 {
		t.Fatalf("got %d dependency sets, want 2 (one per digest)", len(pending))
	}
	// Sorted by digest so the flush order — and any partial-failure retry — is
	// deterministic.
	if pending[0].ImageDigest != "sha256:index" || pending[1].ImageDigest != "sha256:web" {
		t.Errorf("pending order = %s, %s; want sorted by digest", pending[0].ImageDigest, pending[1].ImageDigest)
	}

	// A set that was not marked is still pending: an unacknowledged write must
	// be retried, not lost.
	s.MarkDependenciesWritten("sha256:index")
	pending = s.PendingDependencies()
	if len(pending) != 1 || pending[0].ImageDigest != "sha256:web" {
		t.Fatalf("after marking one, pending = %+v, want only sha256:web", pending)
	}

	// Marked sets are never offered again, not even after the same build is
	// re-reported by a later scan.
	s.MarkDependenciesWritten("sha256:web")
	s.Ingest(nodescan.Report{Node: "n1", Binaries: []nodescan.BinaryInfo{
		withDeps(binary("pod-a", "cid", "go1.26.1", "github.com/acme/web", false), "golang.org/x/sync"),
	}}, resolver)
	if pending := s.PendingDependencies(); len(pending) != 0 {
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
	if pending := s.PendingDependencies(); len(pending) != 0 {
		t.Errorf("pending = %+v, want none: a dependency set with no digest cannot be keyed", pending)
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
