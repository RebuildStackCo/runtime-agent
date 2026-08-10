package nodescan

// Report is the node role's scan result as delivered to the controller
// (ADR 0010): the kept (non-infrastructure) binaries and the aggregate counters
// for one pass, tagged with the node that produced them. It is the wire
// contract of the node→controller channel — the node marshals it, the
// controller unmarshals it — and carries only already-filtered facts (the
// on-node module filter, ADR 0009, ran before this shape exists). No identity
// of a filtered-out or unreadable binary appears here; those are only the
// counters (CLAUDE.md invariant 6).
type Report struct {
	Node     string       `json:"node"`
	Binaries []BinaryInfo `json:"binaries"`
	Counters Counters     `json:"counters"`
}
