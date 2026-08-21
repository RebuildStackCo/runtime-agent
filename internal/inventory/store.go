// Package inventory joins the node role's Go build-info facts (ADR 0009,
// delivered over the channel in ADR 0010) against the controller's workload
// inventory into a per-(namespace, workload, container) Go-inventory record. It
// holds the join state in memory only: it is reconstructed from the next node
// scan and the live cluster, so it adds no persistent state and preserves the
// loss-harmless property (ADR 0003).
package inventory

import (
	"slices"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/RebuildStackCo/runtime-agent/internal/collector"
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

// BuildDependencies is the dependency module set of one build, keyed by the
// image digest that identifies that build. It is deliberately not a field on
// GoRecord: a record merges across every replica and node running the build, and
// the same image usually backs several workloads, so hanging hundreds of module
// paths off each record would duplicate them across the inventory snapshot on
// every flush. Keyed by digest instead, the set is written once and joined back
// through GoRecord.ImageDigest (ADR 0017).
//
// Module paths only, never versions: a path says what a build is made of, a
// path with a version is a vulnerability-scanning input, which this agent does
// not collect (docs/security.md §8).
type BuildDependencies struct {
	ImageDigest string `json:"image_digest"`
	GoVersion   string `json:"go_version"`
	MainModule  string `json:"main_module"`
	// Modules is sorted, so the payload bytes are deterministic.
	Modules []string `json:"modules"`
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

// Store accumulates joined Go-inventory records, the dependency set of each
// observed build, and the aggregate join counters. It is safe for concurrent
// use: reports arrive on the receiver's request goroutines while the
// flush/coverage goroutine reads snapshots.
type Store struct {
	mu      sync.Mutex
	records map[Key]GoRecord

	// deps holds one dependency set per observed image digest, and written
	// records which of those have already been handed to the sink. Both grow
	// with the number of distinct builds observed since start, not with the
	// flush cadence: a build's dependency set is immutable, so a digest already
	// present is skipped on ingest rather than recomputed.
	deps    map[string]BuildDependencies
	written map[string]struct{}

	// nodesReported is the set of nodes still in the cluster that have delivered
	// at least one report. Compared against the node count in the node-metadata
	// payload it is what makes a DaemonSet that never started visible
	// downstream — which is why departed nodes are pruned from it (ADR 0018):
	// a count that outlives the nodes would exceed the fleet and hide the gap
	// it exists to show.
	nodesReported map[string]struct{}

	// since is when this store started counting: the base of every cumulative
	// counter below, carried in the payload so the base is in the bytes rather
	// than in prose.
	since time.Time

	// Cumulative join counters since start, for the coverage report. They
	// describe what was joined and what could not be, never the identity of an
	// unjoined fact (CLAUDE.md invariant 6).
	factsReceived   int64
	factsJoined     int64
	factsUnjoined   int64
	factsUndigested int64
}

// NewStore returns an empty store counting from since.
func NewStore(since time.Time) *Store {
	return &Store{
		records:       make(map[Key]GoRecord),
		deps:          make(map[string]BuildDependencies),
		written:       make(map[string]struct{}),
		nodesReported: make(map[string]struct{}),
		since:         since,
	}
}

// Ingest joins one node report against the resolver, upserting a record for
// every attributable binary and counting the rest as unjoined. A later report
// for the same container overwrites the record — the inventory reflects the
// current build, and snapshots supersede downstream (ADR 0010), so a rollout
// flips the record as nodes re-scan. Multiple replicas reporting the same build
// collapse to one record (idempotent upsert).
//
// It also files each binary's dependency set under the image digest of the
// container it was joined to, and remembers which nodes have reported at all —
// both for the payloads of ADR 0017.
func (s *Store) Ingest(report nodescan.Report, resolver ContainerResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if report.Node != "" {
		s.nodesReported[report.Node] = struct{}{}
	}
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
		s.ingestDependencies(b, res.ImageDigest)
	}
}

// ingestDependencies files one binary's dependency set under its image digest.
// A joined fact with no digest — the controller has the pod but not yet a
// running container status — cannot identify a build, so its dependency set is
// dropped and counted. Counting rather than silently discarding is the point:
// an unattributable fact must be visible as a number, never as absence.
//
// Callers hold s.mu.
func (s *Store) ingestDependencies(b nodescan.BinaryInfo, digest string) {
	if digest == "" {
		s.factsUndigested++
		return
	}
	if _, ok := s.deps[digest]; ok {
		return // same digest, same build, same modules — nothing to recompute
	}
	modules := slices.Clone(b.Dependencies)
	slices.Sort(modules)
	modules = slices.Compact(modules)
	s.deps[digest] = BuildDependencies{
		ImageDigest: digest,
		GoVersion:   b.GoVersion,
		MainModule:  b.MainModule,
		Modules:     modules,
	}
}

// PendingDependencies returns the dependency sets not yet acknowledged as
// written, sorted by digest. It does not mark them: the caller marks each set
// only once its write succeeded, so a failed write is retried on the next
// flush.
func (s *Store) PendingDependencies() []BuildDependencies {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BuildDependencies, 0, len(s.deps))
	for digest, d := range s.deps {
		if _, ok := s.written[digest]; !ok {
			d.Modules = slices.Clone(d.Modules) // no aliasing of store state past the lock
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ImageDigest < out[j].ImageDigest })
	return out
}

// LiveKeys is the set of record keys the controller's own filtered pod index
// currently supports: one per container of every pod that passed the customer's
// filters. It is the same index workload metadata is derived from, which is the
// point — the two payloads then agree by construction rather than by
// convention (ADR 0018).
//
// Init containers are included. A native sidecar is declared as one and runs
// for the pod's whole life, so excluding them would evict its record on every
// flush and re-add it on the next node scan.
func LiveKeys(pods []collector.PodInfo) []Key {
	out := make([]Key, 0, len(pods))
	for _, p := range pods {
		for _, c := range p.Containers {
			out = append(out, Key{
				Namespace:    p.Namespace,
				WorkloadKind: p.Workload.Kind,
				WorkloadName: p.Workload.Name,
				Container:    c.Name,
			})
		}
	}
	return out
}

// Evicted is what one Retain pass removed, for the coverage log.
type Evicted struct {
	// Records is records whose workload container is gone from the cluster.
	Records int
	// Builds is dependency sets no surviving record references any more.
	Builds int
}

// Retain drops every record whose key is absent from live, and then every
// dependency set no surviving record still references.
//
// The store accumulates records from facts the nodes push, so without this it
// only ever grows: a deleted workload's record would keep shipping in every
// snapshot until the controller restarted, and — the reason this is not merely
// stale data — so would the record of a pod the customer opted out of. Once the
// pod leaves the scan scope (ADR 0015) no new fact arrives to correct or remove
// it, and the opt-out that was supposed to apply at collection would apply only
// to facts not yet collected. Retaining against the live index is what makes
// "the exclusion applies at the collection stage" true of this payload too.
//
// live may legitimately be empty — a cluster where nothing passes the filters
// has an empty inventory — so this does not guard against it. That is safe
// because the pod index is only ever mutated by informer events, never wiped:
// PodWatcher registers its handlers after the caches sync, and a relist replays
// as adds and updates. An empty index therefore means an empty cluster, not an
// unsynced one.
//
// A record for a pod whose node scanned it before the controller's informer saw
// it survives at most one flush cycle: it is evicted here, and re-added by the
// next scan once the informer has caught up.
func (s *Store) Retain(live []Key) Evicted {
	s.mu.Lock()
	defer s.mu.Unlock()

	set := make(map[Key]struct{}, len(live))
	for _, k := range live {
		set[k] = struct{}{}
	}
	var ev Evicted
	for k := range s.records {
		if _, ok := set[k]; !ok {
			delete(s.records, k)
			ev.Records++
		}
	}

	referenced := make(map[string]struct{}, len(s.records))
	for _, r := range s.records {
		if r.ImageDigest != "" {
			referenced[r.ImageDigest] = struct{}{}
		}
	}
	for digest := range s.deps {
		if _, ok := referenced[digest]; ok {
			continue
		}
		// deps and written must be dropped together. Dropping only deps would
		// leave the digest marked as already written, so if that image ever came
		// back its dependency set would be re-ingested and never sent again —
		// silently, with nothing failing.
		delete(s.deps, digest)
		delete(s.written, digest)
		ev.Builds++
	}
	return ev
}

// RetainNodes drops reporting nodes that are no longer in the cluster, so
// nodes_reported stays comparable with the node count in the node-metadata
// payload. Without it a cluster that scaled down would report more nodes than
// it has, and a scaled-down fleet with a broken DaemonSet would look complete.
func (s *Store) RetainNodes(live []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make(map[string]struct{}, len(live))
	for _, n := range live {
		set[n] = struct{}{}
	}
	for n := range s.nodesReported {
		if _, ok := set[n]; !ok {
			delete(s.nodesReported, n)
		}
	}
}

// MarkDependenciesWritten records that the set for digest reached the sink, so
// it is not written again. The bookkeeping is in memory only, and losing it is
// harmless: after a restart the set is written once more, and since the payload
// is immutable given its digest, a duplicate delivery is a byte-identical
// upsert (ADR 0003, ADR 0017).
func (s *Store) MarkDependenciesWritten(digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written[digest] = struct{}{}
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
	// Builds is the number of distinct image digests whose dependency set is
	// held.
	Builds int
	// FactsReceived / FactsJoined / FactsUnjoined / FactsUndigested are
	// cumulative since start.
	FactsReceived   int64
	FactsJoined     int64
	FactsUnjoined   int64
	FactsUndigested int64
	// NodesReported is how many distinct nodes have delivered a report.
	NodesReported int
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
		Records:         len(s.records),
		GoVersions:      len(versions),
		PGOBuilds:       pgo,
		Builds:          len(s.deps),
		FactsReceived:   s.factsReceived,
		FactsJoined:     s.factsJoined,
		FactsUnjoined:   s.factsUnjoined,
		FactsUndigested: s.factsUndigested,
		NodesReported:   len(s.nodesReported),
	}
}

// Coverage is what the Go-inventory payload says about its own completeness.
// The inventory is assembled from facts the nodes push, so a record can be
// missing for reasons the record set cannot express: a node whose DaemonSet
// never started reports nothing, and a fact whose pod the controller cannot
// resolve is dropped. Both are counts here, never identities (CLAUDE.md
// invariant 6).
//
// The fact counters are cumulative from Since, which is carried alongside them
// so a consumer never has to know the base from documentation (ADR 0013's
// principle, applied to a structural payload).
type Coverage struct {
	Since time.Time `json:"since"`
	// NodesReported is how many nodes *currently in the cluster* have reported
	// since Since. It is not a running total: a node that has left is dropped,
	// so this stays directly comparable with the node count in node_metadata
	// (ADR 0018).
	NodesReported int `json:"nodes_reported"`
	// FactsReceived is every binary in every report; Joined is those attributed
	// to a workload container; Unjoined is those that could not be. Undigested
	// counts joined facts whose container had no image digest yet, so their
	// dependency set could not be keyed to a build.
	FactsReceived   int64 `json:"facts_received"`
	FactsJoined     int64 `json:"facts_joined"`
	FactsUnjoined   int64 `json:"facts_unjoined"`
	FactsUndigested int64 `json:"facts_undigested"`
}

// Coverage returns the completeness block for the Go-inventory payload.
func (s *Store) Coverage() Coverage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Coverage{
		// UTC so the payload's two timestamps are formatted alike; the agent's
		// local zone is not a fact about the cluster.
		Since:           s.since.UTC(),
		NodesReported:   len(s.nodesReported),
		FactsReceived:   s.factsReceived,
		FactsJoined:     s.factsJoined,
		FactsUnjoined:   s.factsUnjoined,
		FactsUndigested: s.factsUndigested,
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
