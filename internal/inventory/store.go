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

// PeakKey extends Key with the build, because a peak belongs to the code that
// reached it: a rollout that fixes a leak must not inherit the number the
// previous build set (ADR 0052).
type PeakKey struct {
	Key
	ImageDigest string `json:"image_digest,omitempty"`
}

// PeakRecord is what the processes of one workload container, of one build,
// currently reach — measured on the node and merged across every replica the
// agent can see.
//
// It is rebuilt from the latest report of each node rather than accumulated. A
// pod that died takes its peak with it, which is the honest reading: a number
// no running process still stands behind is not evidence about what is deployed
// now (ADR 0052 §3).
type PeakRecord struct {
	PeakKey
	// PeakRSSBytes is the largest high-water mark among those processes. It is
	// a floor under the container's peak, never the figure the OOM killer
	// compares against: the cgroup also holds page cache and every other
	// process in the container.
	PeakRSSBytes int64 `json:"peak_rss_bytes"`
	// Processes is how many processes the numbers were taken over. Zero
	// processes produce no record at all, so this is never zero — it is what
	// says whether one replica or forty stand behind the peak.
	Processes int `json:"processes"`
	// CPUsAllowedMin and CPUsAllowedMax bracket the affinity masks seen. Equal
	// values mean every replica sees the same number of CPUs; different ones
	// mean the replicas sit on different machines, or one of them is pinned.
	CPUsAllowedMin int `json:"cpus_allowed_min,omitempty"`
	CPUsAllowedMax int `json:"cpus_allowed_max,omitempty"`
}

// nodePeaks is one node's contribution to a PeakRecord: what its latest report
// said about the processes of that container on that node.
type nodePeaks struct {
	peakRSSBytes     int64
	processes        int
	cpusMin, cpusMax int
}

// PortKey extends Key with the build, for the reason PeakKey does: a rollout
// that moves an endpoint must not inherit the port the previous build served.
type PortKey struct {
	Key
	ImageDigest string `json:"image_digest,omitempty"`
}

// PortRecord is what the processes of one workload container, of one build,
// accept connections on — merged across every replica the agent can see.
//
// Rebuilt from the latest report of each node, like the peaks beside it: a port
// no live process still binds is not a port the workload serves, and a record
// that outlived its process would send a reader after an endpoint that is gone
// (ADR 0056 §3).
type PortRecord struct {
	PortKey
	// Ports is the union of what the replicas listen on, in port order.
	Ports []nodescan.ListeningPort `json:"ports"`
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
	// HasPprof is true when net/http/pprof is linked into the build. A property
	// of the image like everything else here, which is why it is filed per
	// digest and asked once rather than once per replica (ADR 0056 §1).
	HasPprof bool `json:"has_pprof,omitempty"`
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

	// peaks holds, per (record key, build) and per reporting node, what that
	// node's latest report said. Keeping the node in the key is what makes the
	// aggregate decay: a node's contribution is replaced wholesale on its next
	// report, so a peak survives only while a process on some node still holds
	// it (ADR 0052 §3).
	peaks map[PeakKey]map[string]nodePeaks

	// ports holds, per (record key, build) and per reporting node, the listening
	// ports that node's latest report saw. Node-keyed for the reason peaks are:
	// a contribution is replaced wholesale on the node's next report, so a port
	// survives only while some node still serves it (ADR 0056 §3).
	ports map[PortKey]map[string][]nodescan.ListeningPort

	// scans holds the latest scan counters each reporting node sent. Latest
	// rather than summed: they describe one pass over a node's process table,
	// and adding successive passes would count the same long-lived process once
	// per scan interval (ADR 0054 §4).
	scans map[string]nodescan.Counters

	// profiling holds the latest eBPF-profiling coverage each reporting node
	// sent. Latest rather than summed for the same reason as scans, and
	// cumulative on the node rather than per pass: a node states what its
	// profiler has done since it started, and the fleet answer is the sum of
	// those statements (ADR 0060 §3).
	profiling map[string]nodescan.ProfilingCoverage

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
		peaks:         make(map[PeakKey]map[string]nodePeaks),
		ports:         make(map[PortKey]map[string][]nodescan.ListeningPort),
		nodesReported: make(map[string]struct{}),
		scans:         make(map[string]nodescan.Counters),
		profiling:     make(map[string]nodescan.ProfilingCoverage),
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
		s.scans[report.Node] = report.Counters
		if report.Profiling != nil {
			s.profiling[report.Node] = *report.Profiling
		}
	}
	seenPeaks := make(map[PeakKey]bool, len(report.Binaries))
	seenPorts := make(map[PortKey]bool, len(report.Binaries))
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
		s.ingestPeaks(b, key, res.ImageDigest, report.Node, seenPeaks)
		s.ingestPorts(b, key, res.ImageDigest, report.Node, seenPorts)
	}
	s.forgetUnreported(report.Node, seenPeaks)
	s.forgetUnreportedPorts(report.Node, seenPorts)
}

// ingestPeaks folds one binary's measured facts into this node's contribution
// for its key. seen collects the keys this report touched, so the contribution
// of a key the node no longer runs can be dropped afterwards.
//
// A process whose status could not be read adds nothing — not a zero, which
// would drag a minimum down and claim a container reached nothing.
//
// Callers hold s.mu.
func (s *Store) ingestPeaks(b nodescan.BinaryInfo, key Key, digest, node string, seen map[PeakKey]bool) {
	if b.PeakRSSBytes == 0 && b.CPUsAllowed == 0 {
		return
	}
	pk := PeakKey{Key: key, ImageDigest: digest}
	byNode, ok := s.peaks[pk]
	if !ok {
		byNode = make(map[string]nodePeaks, 1)
		s.peaks[pk] = byNode
	}
	// The first binary of this key in this report starts the node's contribution
	// from zero, discarding what the node said last time; the rest merge into
	// it. Accumulating across reports instead would make `processes` count
	// scans rather than processes, and would keep a peak no live process holds.
	var n nodePeaks
	if seen[pk] {
		n = byNode[node]
	}
	seen[pk] = true
	n.processes++
	n.peakRSSBytes = max(n.peakRSSBytes, b.PeakRSSBytes)
	if c := b.CPUsAllowed; c > 0 {
		if n.cpusMin == 0 || c < n.cpusMin {
			n.cpusMin = c
		}
		n.cpusMax = max(n.cpusMax, c)
	}
	byNode[node] = n
}

// forgetUnreported drops this node's contribution to every key its latest report
// did not mention, and the key itself once no node contributes to it. Without
// it a peak would outlive the pod that set it, which is the whole failure mode
// ADR 0052 §3 exists to avoid.
//
// Callers hold s.mu.
func (s *Store) forgetUnreported(node string, seen map[PeakKey]bool) {
	for pk, byNode := range s.peaks {
		if seen[pk] {
			continue
		}
		delete(byNode, node)
		if len(byNode) == 0 {
			delete(s.peaks, pk)
		}
	}
}

// ingestPorts folds one binary's listening ports into this node's contribution
// for its key, as ingestPeaks folds its measured facts. seen carries the same
// meaning: the first binary of a key in a report starts the node's contribution
// over, the rest merge into it.
//
// Callers hold s.mu.
func (s *Store) ingestPorts(b nodescan.BinaryInfo, key Key, digest, node string, seen map[PortKey]bool) {
	if len(b.ListeningPorts) == 0 {
		return
	}
	pk := PortKey{Key: key, ImageDigest: digest}
	byNode, ok := s.ports[pk]
	if !ok {
		byNode = make(map[string][]nodescan.ListeningPort, 1)
		s.ports[pk] = byNode
	}
	var acc []nodescan.ListeningPort
	if seen[pk] {
		acc = byNode[node]
	}
	seen[pk] = true
	byNode[node] = nodescan.MergeListeningPorts(acc, b.ListeningPorts)
}

// forgetUnreportedPorts drops this node's contribution to every port key its
// latest report did not mention, and the key itself once no node contributes.
//
// Callers hold s.mu.
func (s *Store) forgetUnreportedPorts(node string, seen map[PortKey]bool) {
	for pk, byNode := range s.ports {
		if seen[pk] {
			continue
		}
		delete(byNode, node)
		if len(byNode) == 0 {
			delete(s.ports, pk)
		}
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
		HasPprof:    b.HasPprof,
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
	// Peaks is peak records whose workload container is gone.
	Peaks int
	// Ports is port records whose workload container is gone.
	Ports int
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
	for pk := range s.peaks {
		if _, ok := set[pk.Key]; !ok {
			delete(s.peaks, pk)
			ev.Peaks++
		}
	}
	for pk := range s.ports {
		if _, ok := set[pk.Key]; !ok {
			delete(s.ports, pk)
			ev.Ports++
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
			delete(s.scans, n)
			delete(s.profiling, n)
		}
	}
	// A departed node's peaks and ports go with it. Its pods are gone, so what
	// it contributed stands behind nothing (ADR 0052 §3).
	for pk, byNode := range s.peaks {
		for node := range byNode {
			if _, ok := set[node]; !ok {
				delete(byNode, node)
			}
		}
		if len(byNode) == 0 {
			delete(s.peaks, pk)
		}
	}
	for pk, byNode := range s.ports {
		for node := range byNode {
			if _, ok := set[node]; !ok {
				delete(byNode, node)
			}
		}
		if len(byNode) == 0 {
			delete(s.ports, pk)
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

// PeakSnapshot merges every node's contribution into one record per (key,
// build), sorted so the payload bytes are deterministic. The slice is a copy and
// is non-nil even when empty.
func (s *Store) PeakSnapshot() []PeakRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PeakRecord, 0, len(s.peaks))
	for pk, byNode := range s.peaks {
		rec := PeakRecord{PeakKey: pk}
		for _, n := range byNode {
			rec.PeakRSSBytes = max(rec.PeakRSSBytes, n.peakRSSBytes)
			rec.Processes += n.processes
			if n.cpusMin > 0 && (rec.CPUsAllowedMin == 0 || n.cpusMin < rec.CPUsAllowedMin) {
				rec.CPUsAllowedMin = n.cpusMin
			}
			rec.CPUsAllowedMax = max(rec.CPUsAllowedMax, n.cpusMax)
		}
		if rec.Processes == 0 {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return lessKey(out[i].Key, out[j].Key)
		}
		return out[i].ImageDigest < out[j].ImageDigest
	})
	return out
}

// PortSnapshot returns one record per (workload container, build) that some node
// still reports a listening port for, sorted deterministically.
func (s *Store) PortSnapshot() []PortRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PortRecord, 0, len(s.ports))
	for pk, byNode := range s.ports {
		lists := make([][]nodescan.ListeningPort, 0, len(byNode))
		for _, ports := range byNode {
			lists = append(lists, ports)
		}
		merged := nodescan.MergeListeningPorts(lists...)
		if len(merged) == 0 {
			continue
		}
		out = append(out, PortRecord{PortKey: pk, Ports: merged})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return lessKey(out[i].Key, out[j].Key)
		}
		return out[i].ImageDigest < out[j].ImageDigest
	})
	return out
}

// PprofBuilds returns, per image digest whose build links net/http/pprof, the
// module paths that build compiles from source (ADR 0056 §1). It is the second
// stage of the endpoint funnel, and the one that answers for a whole image at
// once.
//
// The modules travel with the digest because they are what says which frames of
// a profile of that build are the customer's own code — read from the binary,
// where no configured list can be wrong about it (ADR 0058 §5).
func (s *Store) PprofBuilds() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]string)
	for digest, b := range s.builds {
		if b.HasPprof {
			out[digest] = nodescan.OwnModules(b.MainModule, b.Modules)
		}
	}
	return out
}

// ScanCoverage is what the node scanners did, summed over the nodes that
// reported: how many processes they walked and how many they dropped, by
// reason.
//
// Aggregate, never per node. The question "how many processes did the fleet
// skip" is a cluster question, and a per-node breakdown would grow the payload
// with the fleet for an answer nobody asks that way (ADR 0054 §4).
type ScanCoverage struct {
	// Nodes is how many nodes contributed the counters below.
	Nodes int `json:"nodes"`
	// ProcessesScanned is every PID the passes attempted; GoFound is what they
	// kept. The three drops in between are the whole of what the node did not
	// report, and none of them names anything (CLAUDE.md invariant 6).
	ProcessesScanned int `json:"processes_scanned"`
	GoFound          int `json:"go_found"`
	// FilteredScope is processes whose pod the controller's filters excluded —
	// the customer's own choice, counted on the node before an executable was
	// opened. FilteredInfra is cluster infrastructure and this agent itself.
	// Unreadable is a real executable carrying no Go build information.
	FilteredScope int `json:"filtered_scope"`
	FilteredInfra int `json:"filtered_infra"`
	Unreadable    int `json:"unreadable"`
}

// ScanCoverage sums the latest scan counters of every reporting node.
func (s *Store) ScanCoverage() ScanCoverage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ScanCoverage{Nodes: len(s.scans)}
	for _, c := range s.scans {
		out.ProcessesScanned += c.ProcessesScanned
		out.GoFound += c.GoFound
		out.FilteredScope += c.FilteredScope
		out.FilteredInfra += c.FilteredInfra
		out.Unreadable += c.Unreadable
	}
	return out
}

// ProfileCoverage is what the fleet's eBPF profilers did, summed over the nodes
// that reported, plus the two numbers only the controller can count. It is the
// answer to "this workload has no profile — is that the cluster or the agent?",
// which the log lines it replaces could only answer node by node (ADR 0060).
//
// Aggregate, never per node, for the reason ScanCoverage is: the fleet is the
// unit the question is asked in, and a per-node breakdown would grow the
// payload with the cluster (ADR 0054 §4).
type ProfileCoverage struct {
	// Nodes is how many nodes reported profiling coverage at all. States is how
	// many of them are in each state — "supported", "disabled", or a refusal —
	// so a fleet that profiles nothing says why in one field.
	Nodes  int            `json:"nodes"`
	States map[string]int `json:"states,omitempty"`
	// Capture windows and the ones that produced nothing, by reason.
	Windows          int `json:"windows"`
	WindowsNoScope   int `json:"windows_no_scope"`
	WindowsNoTargets int `json:"windows_no_targets"`
	WindowsNoSamples int `json:"windows_no_samples"`
	// What the nodes built and what became of it.
	ProfilesShipped   int `json:"profiles_shipped"`
	ProfilesInvalid   int `json:"profiles_invalid"`
	ProfilesUnshipped int `json:"profiles_unshipped"`
	SamplesOutOfScope int `json:"samples_out_of_scope"`
	// What the symbol filter redacted on the nodes, in aggregate and with no
	// identity of anything redacted (CLAUDE.md invariant 6).
	ThirdPartyDropped   uint64 `json:"third_party_dropped"`
	UnsymbolizedDropped uint64 `json:"unsymbolized_dropped"`
	SamplesFiltered     uint64 `json:"samples_filtered"`
	// ProfilesReceived and ProfilesUnjoined are the controller's own count, not
	// the nodes': a profile whose pod the controller cannot find is dropped
	// here, after a node did the work of capturing it (ADR 0010 §5).
	ProfilesReceived uint64 `json:"profiles_received"`
	ProfilesUnjoined uint64 `json:"profiles_unjoined"`
}

// ProfileCoverage sums the latest profiling coverage of every reporting node.
// The two controller-side counts are the caller's to fill: the store never sees
// a profile, only the reports that say one was made.
func (s *Store) ProfileCoverage() ProfileCoverage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ProfileCoverage{Nodes: len(s.profiling), States: make(map[string]int, len(s.profiling))}
	for _, c := range s.profiling {
		out.States[c.State]++
		out.Windows += c.Windows
		out.WindowsNoScope += c.WindowsNoScope
		out.WindowsNoTargets += c.WindowsNoTargets
		out.WindowsNoSamples += c.WindowsNoSamples
		out.ProfilesShipped += c.ProfilesShipped
		out.ProfilesInvalid += c.ProfilesInvalid
		out.ProfilesUnshipped += c.ProfilesUnshipped
		out.SamplesOutOfScope += c.SamplesOutOfScope
		out.ThirdPartyDropped += c.ThirdPartyDropped
		out.UnsymbolizedDropped += c.UnsymbolizedDropped
		out.SamplesFiltered += c.SamplesFiltered
	}
	return out
}

// Counters is the aggregate inventory state for the coverage report.
type Counters struct {
	// Records is the number of distinct (namespace, workload, container)
	// records currently held.
	Records int `json:"records"`
	// GoVersions is the number of distinct Go versions across those records.
	GoVersions int `json:"go_versions"`
	// PGOBuilds is how many records were built with PGO.
	PGOBuilds int `json:"pgo_builds"`
	// Builds is the number of distinct image digests whose facts are held.
	Builds int `json:"builds"`
	// FactsReceived / FactsJoined / FactsUnjoined / FactsUndigested are
	// cumulative since start.
	FactsReceived   int64 `json:"facts_received"`
	FactsJoined     int64 `json:"facts_joined"`
	FactsUnjoined   int64 `json:"facts_unjoined"`
	FactsUndigested int64 `json:"facts_undigested"`
	// NodesReported is how many distinct nodes have delivered a report.
	NodesReported int `json:"nodes_reported"`
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
