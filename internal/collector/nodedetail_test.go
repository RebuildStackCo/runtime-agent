package collector

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The promise of ADR 0064 §2 that costs the most to break: a condition's message
// is free text, and the two things that write it put paths and command output
// there. The struct has nowhere for it to go, and this asserts that no field
// acquires one — the same shape as the process name's test in nodescan.
func TestAConditionMessageHasNowhereToGo(t *testing.T) {
	const message = "/var/lib/kubelet/pods/acme-payments-7f8/volumes is 94% full"
	var drops nodeDrops
	kept := reduceConditions([]corev1.NodeCondition{{
		Type:    corev1.NodeDiskPressure,
		Status:  corev1.ConditionTrue,
		Reason:  "KubeletHasDiskPressure",
		Message: message,
	}}, &drops)

	if len(kept) != 1 {
		t.Fatalf("kept %d conditions, want 1", len(kept))
	}
	for _, field := range []string{kept[0].Type, kept[0].Status, kept[0].Reason} {
		if strings.Contains(field, "acme-payments") || strings.Contains(field, "/var/lib") {
			t.Errorf("a condition field carries the message: %q", field)
		}
	}
}

// Custom condition types are what node-problem-detector installs, and their
// reasons are written by whoever installed it. The type allow-list is what makes
// `reason` safe to carry at all (ADR 0064 §2).
func TestACustomConditionIsCountedNotCarried(t *testing.T) {
	var drops nodeDrops
	kept := reduceConditions([]corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		{Type: "KernelDeadlock", Status: corev1.ConditionFalse, Reason: "acme-internal-probe"},
		{Type: "ReadonlyFilesystem", Status: corev1.ConditionFalse},
	}, &drops)

	if len(kept) != 1 || kept[0].Type != "Ready" {
		t.Fatalf("kept %+v, want only Ready", kept)
	}
	if drops.Conditions != 2 {
		t.Errorf("counted %d dropped conditions, want 2", drops.Conditions)
	}
}

// The heartbeat is deliberately not collected: it moves every few seconds on
// every node, and carrying it would make every node differ from its previous
// view on every flush (ADR 0064 §2).
func TestTheConditionKeepsTheTransitionNotTheHeartbeat(t *testing.T) {
	transition := metav1.Date(2026, 8, 3, 10, 0, 0, 0, metav1.Now().Location())
	beat := metav1.Date(2026, 8, 6, 10, 0, 40, 0, metav1.Now().Location())
	var drops nodeDrops
	kept := reduceConditions([]corev1.NodeCondition{{
		Type:               corev1.NodeReady,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: transition,
		LastHeartbeatTime:  beat,
	}}, &drops)

	if got := kept[0].Since; !got.Equal(transition.Time) {
		t.Errorf("since = %v, want the transition %v", got, transition.Time)
	}
}

func nodeStatusWith(capacity, allocatable corev1.ResourceList) corev1.NodeStatus {
	return corev1.NodeStatus{Capacity: capacity, Allocatable: allocatable}
}

// A vendor's line is open — a GPU and a MIG slice of it are the same fact under
// different names — so the allow-list is by prefix (ADR 0064 §2).
func TestAMIGSliceIsTheSameVendor(t *testing.T) {
	var drops nodeDrops
	devices := reduceDevices(nodeStatusWith(
		corev1.ResourceList{"nvidia.com/mig-1g.5gb": resource.MustParse("7")},
		corev1.ResourceList{"nvidia.com/mig-1g.5gb": resource.MustParse("7")},
	), &drops)

	if len(devices) != 1 || devices[0].Name != "nvidia.com/mig-1g.5gb" || devices[0].Capacity != 7 {
		t.Fatalf("devices = %+v, want the MIG slice kept", devices)
	}
	if drops.Devices != 0 {
		t.Errorf("counted %d drops for a known vendor, want 0", drops.Devices)
	}
}

// The rest of the extended-resource namespace belongs to whoever runs the
// cluster, where a resource named for an internal team is an identity that must
// not travel. It is counted, not carried (ADR 0064 §2, §4).
func TestAnOperatorsOwnResourceIsCountedNotNamed(t *testing.T) {
	var drops nodeDrops
	devices := reduceDevices(nodeStatusWith(
		corev1.ResourceList{
			"acme-internal.example.com/licence-seat": resource.MustParse("4"),
			"nvidia.com/gpu":                         resource.MustParse("2"),
		},
		corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")},
	), &drops)

	if len(devices) != 1 || devices[0].Name != "nvidia.com/gpu" {
		t.Fatalf("devices = %+v, want only the GPU", devices)
	}
	if drops.Devices != 1 {
		t.Errorf("counted %d drops, want 1", drops.Devices)
	}
}

// Core resources are already named fields; they must not also arrive as devices.
func TestCoreResourcesAreNotDevices(t *testing.T) {
	var drops nodeDrops
	devices := reduceDevices(nodeStatusWith(
		corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("4"),
			corev1.ResourceMemory:           resource.MustParse("16Gi"),
			corev1.ResourcePods:             resource.MustParse("110"),
			corev1.ResourceEphemeralStorage: resource.MustParse("50Gi"),
			"hugepages-2Mi":                 resource.MustParse("0"),
		},
		corev1.ResourceList{},
	), &drops)

	if len(devices) != 0 {
		t.Fatalf("devices = %+v, want none", devices)
	}
	if drops.Devices != 0 {
		t.Errorf("counted %d drops for core resources, want 0 — they are not extended resources at all", drops.Devices)
	}
}

// Hardware present that the scheduler will not hand out is the true state of a
// node whose device plugin has died, and reports as zero allocatable rather than
// as no device (ADR 0064 §2).
func TestHardwarePresentButUnschedulableReportsZero(t *testing.T) {
	var drops nodeDrops
	devices := reduceDevices(nodeStatusWith(
		corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
		corev1.ResourceList{},
	), &drops)

	if len(devices) != 1 || devices[0].Capacity != 8 || devices[0].Allocatable != 0 {
		t.Fatalf("devices = %+v, want 8 present and 0 allocatable", devices)
	}
}

// The taint bound is placement's: dropped whole rather than truncated, because a
// prefix of an unexpected string is still an unexpected string (ADR 0031).
func TestAnOversizedTaintIsDroppedWhole(t *testing.T) {
	var drops nodeDrops
	taints := reduceTaints([]corev1.Taint{
		{Key: strings.Repeat("k", maxPlacementValue+1), Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule},
	}, &drops)

	if len(taints) != 1 || taints[0].Key != "dedicated" {
		t.Fatalf("taints = %+v, want only the one that fits", taints)
	}
	if drops.Taints != 1 {
		t.Errorf("counted %d dropped taints, want 1", drops.Taints)
	}
}

// The two taints the node controller adds are kept, unlike the tolerations that
// answer them: on a node they appear only when it is actually broken, so there
// they are the signal (ADR 0064 §2).
func TestTheNodeControllersOwnTaintsAreKept(t *testing.T) {
	var drops nodeDrops
	taints := reduceTaints([]corev1.Taint{
		{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoExecute},
	}, &drops)

	if len(taints) != 1 {
		t.Fatalf("taints = %+v, want the not-ready taint kept", taints)
	}
}
