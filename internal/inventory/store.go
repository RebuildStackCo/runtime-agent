// Package inventory joins the node role's Go build-info facts (ADR 0009,
// delivered over the channel in ADR 0010) against the controller's workload
// inventory into a per-(namespace, workload, container) Go-inventory record. It
// holds the join state in memory only: it is reconstructed from the next node
// scan and the live cluster, so it adds no persistent state and preserves the
// loss-harmless property (ADR 0003).
package inventory

import (
	"sort"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/RebuildStackCo/runtime-agent/internal/nodescan"
)

// Key identifies one Go-inventory record: one container of one workload, merged
// across every replica and node that runs the same build (ADR 0010). Identities
// of filtered-out binaries never reach a Key — the node dropped them (ADR 0009).
type Key struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workload_kind"`
	WorkloadName string `json:"workload_name"`
	Container    string `json:"container"`
}

// GoRecord is the joined Go inventory for one workload container: the Go version
// and main module read on the node, plus the image digest the controller holds
// for that container. It is resource-unit-free metadata (ADR 0004) of the
// "workload metadata" sensitivity class (docs/security.md §8).
type GoRecord struct {
	Key
	GoVersion   string `json:"go_version"`
	ModulePath  string `json:"module_path"`
	ImageDigest string `json:"image_digest,omitempty"`
	// PGO is true when the binary was built with profile-guided optimization.
	PGO bool `json:"pgo"`
}

// Resolved is what a ContainerResolver returns for a (pod UID, container ID)
// pair: the workload and container the node fact belongs to, plus the image
// digest the controller already collected for it.
type Resolved struct {
	Namespace    string
	WorkloadKind string
	WorkloadName string
	Container    string
	ImageDigest  string
}

// ContainerResolver maps a node fact's pod UID and container ID to the workload
// container it belongs to. PodWatcher satisfies the underlying lookup; the
// controller adapts it to this interface. A false result means the fact cannot
// be attributed (informer lag, or a filtered/unknown pod) and is dropped.
type ContainerResolver interface {
	LookupContainer(podUID types.UID, containerID string) (Resolved, bool)
}

// Store accumulates joined Go-inventory records and the aggregate join
// counters. It is safe for concurrent use: reports arrive on the receiver's
// request goroutines while the flush/coverage goroutine reads snapshots.
type Store struct {
	mu      sync.Mutex
	records map[Key]GoRecord

	// Cumulative join counters since start, for the coverage report. They
	// describe what was joined and what could not be, never the identity of an
	// unjoined fact (CLAUDE.md invariant 6).
	factsReceived int64
	factsJoined   int64
	factsUnjoined int64
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{records: make(map[Key]GoRecord)}
}

// Ingest joins one node report against the resolver, upserting a record for
// every attributable binary and counting the rest as unjoined. A later report
// for the same container overwrites the record — the inventory reflects the
// current build, and snapshots supersede downstream (ADR 0010), so a rollout
// flips the record as nodes re-scan. Multiple replicas reporting the same build
// collapse to one record (idempotent upsert).
func (s *Store) Ingest(report nodescan.Report, resolver ContainerResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range report.Binaries {
		s.factsReceived++
		res, ok := resolver.LookupContainer(types.UID(b.PodUID), b.ContainerID)
		if !ok {
			s.factsUnjoined++
			continue
		}
		s.factsJoined++
		key := Key{
			Namespace:    res.Namespace,
			WorkloadKind: res.WorkloadKind,
			WorkloadName: res.WorkloadName,
			Container:    res.Container,
		}
		s.records[key] = GoRecord{
			Key:         key,
			GoVersion:   b.GoVersion,
			ModulePath:  b.MainModule,
			ImageDigest: res.ImageDigest,
			PGO:         b.PGO,
		}
	}
}

// Snapshot returns the current records, sorted by their key so the payload
// bytes are deterministic (the golden contract, docs/development.md). The slice
// is a copy; callers may retain it. It is non-nil even when empty.
func (s *Store) Snapshot() []GoRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]GoRecord, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Key, out[j].Key) })
	return out
}

// Counters is the aggregate inventory state for the coverage report.
type Counters struct {
	// Records is the number of distinct (namespace, workload, container)
	// records currently held.
	Records int
	// GoVersions is the number of distinct Go versions across those records.
	GoVersions int
	// PGOBuilds is how many records were built with PGO.
	PGOBuilds int
	// FactsReceived / FactsJoined / FactsUnjoined are cumulative since start.
	FactsReceived int64
	FactsJoined   int64
	FactsUnjoined int64
}

// Counters returns a snapshot of the aggregate inventory state.
func (s *Store) Counters() Counters {
	s.mu.Lock()
	defer s.mu.Unlock()
	versions := make(map[string]struct{})
	pgo := 0
	for _, r := range s.records {
		versions[r.GoVersion] = struct{}{}
		if r.PGO {
			pgo++
		}
	}
	return Counters{
		Records:       len(s.records),
		GoVersions:    len(versions),
		PGOBuilds:     pgo,
		FactsReceived: s.factsReceived,
		FactsJoined:   s.factsJoined,
		FactsUnjoined: s.factsUnjoined,
	}
}

func lessKey(a, b Key) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.WorkloadKind != b.WorkloadKind {
		return a.WorkloadKind < b.WorkloadKind
	}
	if a.WorkloadName != b.WorkloadName {
		return a.WorkloadName < b.WorkloadName
	}
	return a.Container < b.Container
}
