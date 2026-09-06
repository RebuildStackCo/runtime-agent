package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/health"
	"github.com/RebuildStackCo/runtime-agent/internal/metrics"
	"github.com/RebuildStackCo/runtime-agent/internal/sink"
)

// scrape fetches and parses the exposition, with the same package's decoder the
// cAdvisor reader uses — so what this asserts is that a Prometheus can read it,
// not that some bytes were produced.
func scrape(t *testing.T, addr string) map[string]*dto.MetricFamily {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+addr+health.MetricsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scraping: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", health.MetricsPath, resp.StatusCode)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		t.Fatalf("parsing the exposition: %v", err)
	}
	return families
}

// TestTheControllerExposesItsOwnWork is the whole slice end to end: a running
// controller, scraped over the health listener's third path, parsed back.
func TestTheControllerExposesItsOwnWork(t *testing.T) {
	addr := freeAddress(t)
	cfg := config.Config{Health: config.Health{ListenAddress: addr}}
	cfg.Spool.Dir = t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)),
			fake.NewClientset(), &rest.Config{}, cfg, config.Shape{}, time.Now())
	}()
	waitFor(t, addr, health.ReadyPath, http.StatusOK)

	families := scrape(t, addr)

	// Every payload kind has a series before anything has shipped, because the
	// spool's counters are seeded from the registry (ADR 0022, ADR 0070 §3).
	kinds := map[string]bool{}
	for _, m := range families[metrics.Prefix+"spool_payloads_written_total"].GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "kind" {
				kinds[l.GetValue()] = true
			}
		}
	}
	for _, row := range sink.Registry() {
		if !kinds[row.Kind] {
			t.Errorf("no series for payload kind %q", row.Kind)
		}
	}

	for _, required := range []string{
		"build_info", "pass_start_timestamp_seconds", "pass_deadline_seconds",
		"source_synced", "kubelet_requests_total", "pods_observed_total",
		"spool_bytes", "spool_files", "spool_evicted_total",
	} {
		if families[metrics.Prefix+required] == nil {
			t.Errorf("%s%s is not exposed", metrics.Prefix, required)
		}
	}

	// And nothing it serves carries a label name outside the permitted set —
	// asserted against what actually crossed the wire, not against the builder.
	for name, fam := range families {
		if !strings.HasPrefix(name, metrics.Prefix) {
			t.Errorf("metric %q does not carry the agent's prefix", name)
		}
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				if !metrics.Allowed[metrics.LabelName(l.GetName())] {
					t.Errorf("metric %q carries label %q", name, l.GetName())
				}
			}
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
}

// TestALogOnlyControllerStillExposesItself: no spool means nothing to collect
// from, and the metrics still say so — a scrape is how an operator finds out
// the agent is in that mode, so it must not be the mode with no scrape.
func TestALogOnlyControllerStillExposesItself(t *testing.T) {
	addr := freeAddress(t)
	cfg := config.Config{Health: config.Health{ListenAddress: addr}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)),
			fake.NewClientset(), &rest.Config{}, cfg, config.Shape{}, time.Now())
	}()
	waitFor(t, addr, health.LivePath, http.StatusOK)

	families := scrape(t, addr)
	if families[metrics.Prefix+"spool_files"] == nil {
		t.Error("a log-only controller exposes no spool metrics at all")
	}
	if fam := families[metrics.Prefix+"spool_payloads_written_total"]; fam == nil {
		t.Error("a log-only controller names no payload kinds")
	} else if len(fam.GetMetric()) != len(sink.Registry()) {
		t.Errorf("%d kind series, want %d", len(fam.GetMetric()), len(sink.Registry()))
	}
	cancel()
	<-done
}

// TestTheScrapePathChangesNothing: it is a read, like the two probes beside it
// (CLAUDE.md invariant 1).
func TestTheScrapePathChangesNothing(t *testing.T) {
	addr := freeAddress(t)
	cfg := config.Config{Health: config.Health{ListenAddress: addr}}
	cfg.Spool.Dir = t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)),
			fake.NewClientset(), &rest.Config{}, cfg, config.Shape{}, time.Now())
	}()
	waitFor(t, addr, health.ReadyPath, http.StatusOK)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequestWithContext(context.Background(), method,
			"http://"+addr+health.MetricsPath, strings.NewReader("scrape_me=1"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, health.MetricsPath, resp.StatusCode)
		}
	}
	cancel()
	<-done
}

// TestTheNodeExposesItsOwnPass. The coverage payload is aggregate over the
// fleet by design (ADR 0054 §4), so which node went quiet is a question only a
// per-node scrape answers — and the node names itself nowhere in the reply: the
// asking is done by the customer's own service discovery (ADR 0070 §4).
func TestTheNodeExposesItsOwnPass(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	addr := freeAddress(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runNode(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), []string{
			"-proc", t.TempDir(),
			"-interval", "1s",
			"-health-address", addr,
		})
	}()
	waitFor(t, addr, health.ReadyPath, http.StatusOK)

	families := scrape(t, addr)
	for _, required := range []string{
		"build_info", "pass_start_timestamp_seconds", "scan_processes",
		"scan_pods_in_scope", "ebpf_state", "node_reports_shipped_total",
	} {
		if families[metrics.Prefix+required] == nil {
			t.Errorf("%s%s is not exposed", metrics.Prefix, required)
		}
	}
	// The node role is not the controller, and says so in the one label that
	// distinguishes them.
	for _, m := range families[metrics.Prefix+"build_info"].GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "role" && l.GetValue() != metrics.RoleNode {
				t.Errorf("role is %q, want %q", l.GetValue(), metrics.RoleNode)
			}
		}
	}
	// Nothing here names the node, the cluster, or anything in it.
	for name, fam := range families {
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				if !metrics.Allowed[metrics.LabelName(l.GetName())] {
					t.Errorf("metric %q carries label %q", name, l.GetName())
				}
				if l.GetValue() == "node-1" {
					t.Errorf("metric %q names the node it runs on", name)
				}
			}
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runNode returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runNode did not stop")
	}
}

// TestTheReceiverIsAskedAboutPerScrapeNotAtStartup. The node-intake receiver is
// wired after the health listener, so a presence check made when the listener
// was built would leave `node_reports_rejected_total` permanently absent on
// every installation that has a DaemonSet — the one signal that says whether
// the controller is the reason a node went quiet (ADR 0067).
func TestTheReceiverIsAskedAboutPerScrapeNotAtStartup(t *testing.T) {
	addr := freeAddress(t)
	cfg := config.Config{Health: config.Health{ListenAddress: addr}}
	cfg.Spool.Dir = t.TempDir()
	cfg.NodeIntake.Enabled = true
	cfg.NodeIntake.ListenAddress = freeAddress(t)
	cfg.NodeIntake.ExpectedSubject = "system:serviceaccount:runtime-agent:runtime-agent-node"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)),
			fake.NewClientset(), &rest.Config{}, cfg, config.Shape{}, time.Now())
	}()
	select {
	case err := <-done:
		t.Fatalf("run stopped early: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	waitFor(t, addr, health.ReadyPath, http.StatusOK)

	families := scrape(t, addr)
	fam := families[metrics.Prefix+"node_reports_rejected_total"]
	if fam == nil {
		t.Fatalf("%snode_reports_rejected_total is absent although the receiver is enabled",
			metrics.Prefix)
	}
	if n := len(fam.GetMetric()); n != 3 {
		t.Errorf("%d rejection reasons, want 3 (unauthorized, too_large, malformed)", n)
	}
	cancel()
	<-done
}
