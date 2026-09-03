// Package model is the vocabulary of collected cluster facts: what a workload,
// a node, a container restart and a policy look like once the agent has reduced
// them.
//
// Every type here is plain data with no Kubernetes dependency, and that is the
// package's purpose rather than a property it happens to have. The packages
// that window, aggregate and serialize these facts never talk to a cluster;
// holding the vocabulary here is what keeps client-go out of them (ADR 0065).
package model
