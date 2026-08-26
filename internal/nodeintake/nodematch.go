package nodeintake

import (
	"log/slog"
	"net/http"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
)

// authorizeNode checks that the node a request speaks for is the node the
// caller's token says it runs on, and writes the refusal itself when it is not.
// It returns true when the request may proceed.
//
// Why this exists (ADR 0040). Every pod of the DaemonSet presents the same
// ServiceAccount, so the token established which *role* was calling and nothing
// about which *node*. The node name arrived in the request body, where the
// caller chose it. One node's token was therefore enough to post inventory and
// profiles attributed to workloads on other nodes, and to ask the scope and
// targets endpoints about any node name at all — turning a single compromised
// node into a reader of the controller's cluster-wide admitted-pod index.
//
// The token has carried the answer the whole time: a projected token is bound
// to the pod it was mounted into, and the kubelet writes that pod's node into
// the `kubernetes.io` claim block. nodeauth reads it; this compares it.
//
// The body field is kept rather than removed, and that is deliberate. Removing
// it would change the wire format between two components that upgrade
// independently — the DaemonSet and the Deployment roll separately — so during
// an upgrade one side would be speaking a shape the other does not know.
// Requiring the two to agree is pure tightening: an old node sends its own name
// and matches, a new node does the same, and only a caller speaking for a node
// it is not on is refused.
//
// The status is 403 rather than 401: the token was valid and the caller is who
// it says it is. What it may not do is speak for somebody else.
func authorizeNode(w http.ResponseWriter, logger *slog.Logger, what string, id nodeauth.Identity, claimed string) bool {
	if claimed == "" {
		http.Error(w, "node required", http.StatusBadRequest)
		return false
	}
	if claimed != id.Node {
		// The rejection is logged with both names because it is the one event
		// here that distinguishes a misconfiguration from an attempt: a node
		// speaking for itself can never reach this line.
		logger.Warn(what+" rejected: node mismatch",
			"claimed_node", claimed, "token_node", id.Node, "subject", id.Subject)
		http.Error(w, "node does not match the token", http.StatusForbidden)
		return false
	}
	return true
}
