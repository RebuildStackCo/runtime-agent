package main

import (
	"github.com/RebuildStackCo/runtime-agent/internal/journal"
	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

// agentStates is the pass's own states in the shape the window accumulates,
// built from the values the coverage snapshot was just written from so the two
// cannot disagree (ADR 0088 §2).
//
// Every watched source contributes, failing or not: a source with no record is
// one nobody watched, and that is the distinction the window exists to keep.
func agentStates(sources []model.SourceHealth, shipping *model.Shipping) []journal.AgentState {
	out := make([]journal.AgentState, 0, len(sources)+1)
	for _, src := range sources {
		out = append(out, journal.AgentState{
			State:   journal.StateFailing,
			Subject: src.Name,
			Since:   src.FailingSince,
		})
	}
	// Absent when the agent has no backend: an agent told to ship nowhere has
	// not stopped shipping, and an hour of "not halted" would claim it had a
	// backend to be halted from.
	if shipping != nil {
		out = append(out, journal.AgentState{
			State:   journal.StateHalted,
			Subject: journal.SubjectShip,
			Since:   shipping.HaltedSince,
		})
	}
	return out
}
