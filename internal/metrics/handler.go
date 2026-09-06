package metrics

import (
	"log/slog"
	"net/http"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// Gatherer builds the families for one scrape. It is called per request, from
// the accessors the coverage payload reads, so there is nothing to keep in sync.
type Gatherer func() ([]*dto.MetricFamily, error)

// Handler serves the text exposition. Like the probes beside it, it reads
// nothing about the request but its method: no body, no query, nothing a caller
// sends changes what the agent does (CLAUDE.md invariant 1).
//
// The encoder is prometheus/common's own — the same package the cAdvisor reader
// decodes with — so the format is the reference implementation's rather than
// this repository's (ADR 0070 §1).
func Handler(gather Gatherer, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only endpoint", http.StatusMethodNotAllowed)
			return
		}
		families, err := gather()
		if err != nil {
			// A label name outside the permitted set is the only way here, and
			// a test makes it unreachable. Serving nothing is the safe half of
			// that: the reason is in the agent's log, not in the reply.
			logger.Error("gathering agent metrics", "error", err)
			http.Error(w, "metrics unavailable", http.StatusInternalServerError)
			return
		}
		format := expfmt.NewFormat(expfmt.TypeTextPlain)
		w.Header().Set("Content-Type", string(format))
		enc := expfmt.NewEncoder(w, format)
		for _, fam := range families {
			if err := enc.Encode(fam); err != nil {
				// The response is already partly written; there is nobody left
				// to tell but the log.
				logger.Error("encoding agent metrics", "error", err, "metric", fam.GetName())
				return
			}
		}
	})
}
