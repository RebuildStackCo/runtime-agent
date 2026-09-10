package main

import (
	"context"
	"slices"
	"testing"
)

func TestRunFlushersRunsEveryWriterInOrder(t *testing.T) {
	var ran []string
	record := func(name string) func() { return func() { ran = append(ran, name) } }
	flushers := []flusher{
		{run: record("first"), onShutdown: true},
		{run: record("second")},
		{run: record("third"), onShutdown: true},
	}

	runFlushers(flushers, false)

	want := []string{"first", "second", "third"}
	if !slices.Equal(ran, want) {
		t.Fatalf("periodic pass ran %v, want %v", ran, want)
	}
}

func TestRunFlushersSkipsPeriodicOnlyWritersOnShutdown(t *testing.T) {
	var ran []string
	record := func(name string) func() { return func() { ran = append(ran, name) } }
	flushers := []flusher{
		{run: record("first"), onShutdown: true},
		{run: record("second")},
		{run: record("third"), onShutdown: true},
	}

	runFlushers(flushers, true)

	want := []string{"first", "third"}
	if !slices.Equal(ran, want) {
		t.Fatalf("shutdown pass ran %v, want %v", ran, want)
	}
}

// recordingShipper notes when the last round happened relative to the writers.
type recordingShipper struct {
	ran   *[]string
	calls int
}

func (r *recordingShipper) FinalRound(context.Context) {
	r.calls++
	*r.ran = append(*r.ran, "ship")
}

// The order is the whole fix. Shipping used to stop with the watchers, so the
// payloads the shutdown pass wrote were written after the only thing that could
// deliver them had already gone — onto a volume that goes with the pod
// (ADR 0079).
func TestTheShutdownPassShipsAfterItHasWritten(t *testing.T) {
	var ran []string
	record := func(name string) func() { return func() { ran = append(ran, name) } }
	flushers := []flusher{
		{run: record("journals"), onShutdown: true},
		{run: record("metadata")},
		{run: record("inventory"), onShutdown: true},
	}

	runShutdown(flushers, &recordingShipper{ran: &ran})

	want := []string{"journals", "inventory", "ship"}
	if !slices.Equal(ran, want) {
		t.Fatalf("the shutdown pass ran %v, want %v", ran, want)
	}
}

// An installation with no backend configured — which is every installation
// today — still writes its last pass and simply has nowhere to send it.
func TestTheShutdownPassWritesWithNoBackendConfigured(t *testing.T) {
	var ran []string
	flushers := []flusher{
		{run: func() { ran = append(ran, "journals") }, onShutdown: true},
	}

	runShutdown(flushers, nil)

	if !slices.Equal(ran, []string{"journals"}) {
		t.Fatalf("the shutdown pass ran %v, want the writers to run regardless", ran)
	}
}
