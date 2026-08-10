package inventory

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

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

func TestIngestJoinsFacts(t *testing.T) {
	resolver := fakeResolver{
		"pod-web/cid-web": {Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app", ImageDigest: "sha256:web"},
	}
	s := NewStore()
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
	s := NewStore()
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
	s := NewStore()
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
	s := NewStore()
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
	s := NewStore()
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
	s := NewStore()
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
