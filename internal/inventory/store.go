// Package inventory joins the node role's Go build-info facts (ADR 0009,
// delivered over the channel in ADR 0010) against the controller's workload
// inventory into a per-(namespace, workload, container) Go-inventory record. It
// holds the join state in memory only: it is reconstructed from the next node
// scan and the live cluster, so it adds no persistent state and preserves the
// loss-harmless property (ADR 0003).
package inventory

import (
	"maps"
	"slices"
	"sort"
	"strings"
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

// BuildFacts is everything immutable the agent knows about one build, keyed by
// the image digest that identifies it. Deliberately not a field on GoRecord: a
// record merges across every replica and node, and one image usually backs
// several workloads, so hanging hundreds of module paths off each record would
// duplicate them on every flush (ADR 0017, ADR 0019).
//
// Each module carries the version the toolchain recorded, at the price
// ADR 0048 §3 weighs.
type BuildFacts struct {
	ImageDigest string `json:"image_digest"`
	GoVersion   string `json:"go_version"`
	MainModule  string `json:"main_module"`
	// Modules is sorted by path and then version, so the payload bytes are
	// deterministic.
	Modules []nodescan.Module `json:"modules"`
	// Settings is the allow-listed build settings the node kept. It is what the
	// scanner sent: the filtering happened there, before the channel, so nothing
	// outside the allow-list ever reached this struct (ADR 0019).
	Settings map[string]string `json:"settings,omitempty"`
	// GoDebug is the allow-listed GODEBUG defaults compiled into the build. It
	// is read beside GoVersion, never alone: absence is the toolchain's own
	// default, not a missing value (ADR 0050 §2).
	GoDebug map[string]string `json:"godebug,omitempty"`
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

// Store accumulates joined Go-inventory records, the facts of each observed
// build, and the aggregate join counters. It is safe for concurrent
// use: reports arrive on the receiver's request goroutines while the
// flush/coverage goroutine reads snapshots.
type Store struct {
	mu      sync.Mutex
	records map[Key]GoRecord

	// builds holds one set of build facts per observed image digest, and written
	// records which of those have already been handed to the sink. Both grow
	// with the number of distinct builds observed since start, not with the
	// flush cadence: a build's facts are immutable, so a digest already present
	// is skipped on ingest rather than recomputed.
	builds  map[string]BuildFacts
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
		builds:        make(map[string]BuildFacts),
		written:       make(map[string]struct{}),
		nodesReported: make(map[string]struct{}),
		since:         since,
	}
}

// Ingest joins one node report against the resolver, upserting a record for
// every attributable binary and counting the rest as unjoined. A later report
// for the same container overwrites the record, so a rollout flips it as nodes
// re-scan and multiple replicas of one build collapse to one record (ADR 0010).
//
// It also files each binary's build facts under the image digest it was joined
// to, and remembers which nodes have reported at all (ADR 0017).
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
		s.ingestBuild(b, res.ImageDigest)
	}
}

// ingestBuild files one binary's build facts under its image digest. A joined
// fact with no digest — the controller has the pod but not yet a running
// container status — cannot identify a build, so its facts are dropped and
// counted. Counting rather than silently discarding is the point: an
// unattributable fact must be visible as a number, never as absence.
//
// Callers hold s.mu.
func (s *Store) ingestBuild(b nodescan.BinaryInfo, digest string) {
	if digest == "" {
		s.factsUndigested++
		return
	}
	if _, ok := s.builds[digest]; ok {
		return // same digest, same build, same facts — nothing to recompute
	}
	modules := slices.Clone(b.Dependencies)
	slices.SortFunc(modules, compareModules)
	modules = slices.Compact(modules)
	s.builds[digest] = BuildFacts{
		ImageDigest: digest,
		GoVersion:   b.GoVersion,
		MainModule:  b.MainModule,
		Modules:     modules,
		Settings:    maps.Clone(b.Settings),
		GoDebug:     maps.Clone(b.GoDebug),
	}
}

// PendingBuilds returns the build facts not yet acknowledged as written, sorted
// by digest. It does not mark them: the caller marks each build only once its
// write succeeded, so a failed write is retried on the next flush.
func (s *Store) PendingBuilds() []BuildFacts {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BuildFacts, 0, len(s.builds))
	for digest, d := range s.builds {
		if _, ok := s.written[digest]; !ok {
			// no aliasing of store state past the lock
			d.Modules = slices.Clone(d.Modules)
			d.Settings = maps.Clone(d.Settings)
			d.GoDebug = maps.Clone(d.GoDebug)
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ImageDigest < out[j].ImageDigest })
	return out
}

// LiveKeys is the set of record keys the controller's filtered pod index
// currently supports: one per container of every admitted pod. It is the same
// index workload metadata is derived from, so the two payloads agree by
// construction rather than by convention (ADR 0018).
//
// Init containers are included: a native sidecar is declared as one and runs for
// the pod's whole life, so excluding them would evict its record every flush.
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
	// Builds is build facts no surviving record references any more.
	Builds int
}

// Retain drops every record whose key is absent from live, and then the build
// facts no surviving record still references.
//
// Without it the store only grows, and the record that matters is the one for a
// pod the customer opted out of: once it leaves the scan scope no new fact
// arrives to remove it (ADR 0015, ADR 0018). An empty live cannot mean
// "unsynced" — the pod index is only ever mutated by informer events, never
// wiped — so it is not guarded against.
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
	for digest := range s.builds {
		if _, ok := referenced[digest]; ok {
			continue
		}
		// builds and written must be dropped together. Dropping only builds would
		// leave the digest marked as already written, so if that image ever came
		// back its facts would be re-ingested and never sent again — silently,
		// with nothing failing.
		delete(s.builds, digest)
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

// MarkBuildWritten records that the facts for digest reached the sink, so they
// are not written again. The bookkeeping is in memory only, and losing it is
// harmless: after a restart the facts are written once more, and since the
// payload is immutable given its digest, a duplicate delivery is a
// byte-identical upsert (ADR 0003, ADR 0017).
func (s *Store) MarkBuildWritten(digest string) {
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
	// Builds is the number of distinct image digests whose facts are held.
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
		Builds:          len(s.builds),
		FactsReceived:   s.factsReceived,
		FactsJoined:     s.factsJoined,
		FactsUnjoined:   s.factsUnjoined,
		FactsUndigested: s.factsUndigested,
		NodesReported:   len(s.nodesReported),
	}
}

// Coverage is what the Go-inventory payload says about its own completeness. The
// inventory is assembled from facts the nodes push, so a record can be missing
// for reasons the record set cannot express: a DaemonSet that never started
// reports nothing, an unresolvable fact is dropped. Both are counts, never
// identities (CLAUDE.md invariant 6).
//
// The fact counters are cumulative from Since, carried alongside them so the
// base is in the bytes rather than in documentation.
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
	// build facts could not be keyed to a build.
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

// compareModules orders a build's dependencies by path and then version, so the
// payload bytes do not depend on the order the toolchain recorded them in.
func compareModules(a, b nodescan.Module) int {
	if a.Path != b.Path {
		return strings.Compare(a.Path, b.Path)
	}
	return strings.Compare(a.Version, b.Version)
}
