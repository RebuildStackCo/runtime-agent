package main

import (
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
