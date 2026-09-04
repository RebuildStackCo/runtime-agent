package nodeintake

import (
	"log/slog"
	"net/http"

	"github.com/RebuildStackCo/runtime-agent/internal/nodeauth"
)

// authorizeNode checks that the node a request speaks for is the node the
// caller's token says it runs on, writes the refusal itself when it is not, and
// returns true when the request may proceed.
//
// The whole DaemonSet shares one ServiceAccount, so the token proves the role
// and not the node; the token's `kubernetes.io` claim block is what proves the
// node (ADR 0040). 403 rather than 401: the caller is who it says it is, and
// may not speak for somebody else.
func authorizeNode(w http.ResponseWriter, logger *slog.Logger, rej *Rejections, what string, id nodeauth.Identity, claimed string) bool {
	if claimed == "" {
		rej.malformedAdd()
		http.Error(w, "node required", http.StatusBadRequest)
		return false
	}
	if claimed != id.Node {
		// Both names, because a node speaking for itself never reaches this
		// line: the pair is what separates misconfiguration from an attempt.
		logger.Warn(what+" rejected: node mismatch",
			"claimed_node", claimed, "token_node", id.Node, "subject", id.Subject)
		rej.unauthorizedAdd()
		http.Error(w, "node does not match the token", http.StatusForbidden)
		return false
	}
	return true
}
