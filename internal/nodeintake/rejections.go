package nodeintake

import (
	"sync/atomic"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

// Rejections counts what the receiver refused, shared by the handlers that
// refuse and read by the coverage report (ADR 0067). A nil *Rejections counts
// nothing, so a handler built without one still works.
type Rejections struct {
	unauthorized atomic.Uint64
	tooLarge     atomic.Uint64
	malformed    atomic.Uint64
}

func (r *Rejections) unauthorizedAdd() {
	if r != nil {
		r.unauthorized.Add(1)
	}
}

func (r *Rejections) tooLargeAdd() {
	if r != nil {
		r.tooLarge.Add(1)
	}
}

func (r *Rejections) malformedAdd() {
	if r != nil {
		r.malformed.Add(1)
	}
}

// Snapshot returns the counts so far. A nil *Rejections snapshots to zeroes.
func (r *Rejections) Snapshot() model.IntakeRejections {
	if r == nil {
		return model.IntakeRejections{}
	}
	return model.IntakeRejections{
		Unauthorized: r.unauthorized.Load(),
		TooLarge:     r.tooLarge.Load(),
		Malformed:    r.malformed.Load(),
	}
}
