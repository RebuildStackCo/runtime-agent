package model

import (
	"reflect"
	"time"
)

// NodeInfo is the collected view of one node: how big it is, what kind of
// machine it is, where it sits, what software runs it, what state it is in and
// what it refuses to run. Nothing else is read from node objects
// (security.md §4).
//
// Zone and region are join keys the cluster already publishes; the agent copies
// them and draws no conclusion from them. The fields past Architecture arrived
// together in ADR 0064.
type NodeInfo struct {
	Name          string `json:"name"`
	InstanceType  string `json:"instance_type,omitempty"`
	CapacityType  string `json:"capacity_type,omitempty"` // "spot", "on-demand", or "" when undeterminable
	Zone          string `json:"zone,omitempty"`
	Region        string `json:"region,omitempty"`
	KernelVersion string `json:"kernel_version,omitempty"`
	// Architecture is the node's CPU architecture as the kubelet reports it
	// ("amd64", "arm64"). It is what makes a build's GOARCH and microarchitecture
	// level answerable rather than merely recorded: a question about a binary's
	// target needs the machine it landed on (ADR 0019).
	Architecture string `json:"architecture,omitempty"`
	// CreatedAt is the node object's own creation timestamp. A fleet's age
	// distribution is what separates a cluster the autoscaler churns hourly from
	// one whose machines have been up since the last upgrade.
	CreatedAt time.Time `json:"created_at,omitzero"`
	// The rest of `status.nodeInfo` the agent reads. Kernel version above
	// already decides whether the eBPF profiler can run; these say what would
	// have to be upgraded to change that, and which runtime's behavior a
	// container is subject to.
	KubeletVersion   string `json:"kubelet_version,omitempty"`
	OSImage          string `json:"os_image,omitempty"`
	OperatingSystem  string `json:"operating_system,omitempty"`
	ContainerRuntime string `json:"container_runtime,omitempty"`

	AllocatableCPUMilli    int64 `json:"allocatable_cpu_milli"`
	AllocatableMemoryBytes int64 `json:"allocatable_memory_bytes"`
	CapacityCPUMilli       int64 `json:"capacity_cpu_milli"`
	CapacityMemoryBytes    int64 `json:"capacity_memory_bytes"`
	// The two core resources that also bound scheduling and that CPU and memory
	// do not imply: image and log space, and the kubelet's own pod ceiling. A
	// node idle in both CPU and memory can still be full (ADR 0064 §1).
	AllocatableEphemeralBytes int64 `json:"allocatable_ephemeral_storage_bytes,omitempty"`
	CapacityEphemeralBytes    int64 `json:"capacity_ephemeral_storage_bytes,omitempty"`
	AllocatablePods           int64 `json:"allocatable_pods,omitempty"`
	CapacityPods              int64 `json:"capacity_pods,omitempty"`

	// The three reduced lists (nodedetail.go). Each is absent when the node has
	// none, which for Conditions cannot happen on a live node — a node reporting
	// no allow-listed condition is one the kubelet has never contacted.
	Devices    []NodeDevice    `json:"devices,omitempty"`
	Conditions []NodeCondition `json:"conditions,omitempty"`
	Taints     []NodeTaint     `json:"taints,omitempty"`
}

// NodeLifecycle is one node arriving in or leaving the cluster (ADR 0064 §3).
// It carries the node's collected view because a departure is the last moment
// that view exists: once the object is gone, `node_metadata` no longer holds the
// row, and the size of what left would be unknowable.
type NodeLifecycle struct {
	Node NodeInfo
	// Joined distinguishes the two events. A join is only reported when it can
	// be proved — see reportJoin.
	Joined bool
	// At is the node object's creation timestamp for a join, and the instant the
	// agent noticed for a departure. Observed says which.
	At       time.Time
	Observed bool
}

// NodeCondition is one condition of an allow-listed type. `message` is never
// read: it is free text into which the kubelet and node-problem-detector write
// paths, device names and command output (ADR 0064 §2).
type NodeCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	// Reason is the kubelet's own CamelCase token. It is safe to carry only
	// because Type is allow-listed: a custom condition's reason is written by
	// whoever installed the agent that sets it, and none of those types survive
	// the allow-list.
	Reason string `json:"reason,omitempty"`
	// Since is the last transition, not the last heartbeat. The heartbeat moves
	// every few seconds and would make every flush report a changed node; the
	// transition is what says how long the node has been in this state.
	Since time.Time `json:"since,omitzero"`
}

// NodeDevice is one extended resource the node advertises, with what the
// scheduler may still hand out. It is the accelerator inventory: the resource
// most likely to be the reason a node exists and the one CPU and memory say
// nothing about.
type NodeDevice struct {
	Name        string `json:"name"`
	Capacity    int64  `json:"capacity"`
	Allocatable int64  `json:"allocatable"`
}

// NodeTaint is one taint, field for field — the mirror of Toleration, which is
// already collected from the pod side (placement.go). A toleration without the
// taint it answers is half a fact: it says what the pod would put up with, not
// what the fleet actually fences off.
type NodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// SameAs reports whether two collected views are the same fact, replacing the
// `==` this type carried while every field was scalar. Reflection rather than a
// field list, so a field added above joins the test by existing; affordable
// because node objects change on the order of minutes — the kubelet heartbeats
// to a Lease, and the one sub-second field it does write, the condition
// heartbeat, is deliberately not collected. The lists are built sorted, so
// element-wise equality is exact rather than merely sufficient.
func (n NodeInfo) SameAs(other NodeInfo) bool { return reflect.DeepEqual(n, other) }
