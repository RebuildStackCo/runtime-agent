package nodeprofile

import (
	"context"
	"testing"
)

// A capture that ends on its own is a different fact from one the agent shut
// down, and only the first has to reach the report: a node whose tracer died
// cuts empty windows forever and reads as idle (ADR 0060 §5).
func TestACaptureThatEndsOnItsOwnSaysSo(t *testing.T) {
	var s Session
	s.watch(context.Background(), func() {})
	if !s.Stopped() {
		t.Error("the capture returned with the context still live and the session denies it stopped")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var shutdown Session
	shutdown.watch(ctx, func() {})
	if shutdown.Stopped() {
		t.Error("a cancelled context is the agent stopping, not the tracer dying")
	}
}
