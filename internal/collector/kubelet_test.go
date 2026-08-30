package collector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// hangingKubelets serves the named nodes by blocking until the test ends, and
// every other node by answering an empty summary and exposition at once. It
// returns the number of requests that reached the handler.
func hangingKubelets(t *testing.T, hung ...string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	block := make(chan struct{})
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		for _, node := range hung {
			if strings.Contains(r.URL.Path, "/nodes/"+node+"/") {
				<-block
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "metrics/cadvisor") {
			return
		}
		_, _ = io.WriteString(w, `{"pods":[]}`)
	}))
	// Closing the server waits for the blocked handlers, so release them first.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(block) })
	return server, &served
}

func pollerAgainst(t *testing.T, server *httptest.Server, timeout time.Duration, nodes ...string) *UsagePoller {
	t.Helper()
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: -1})
	if err != nil {
		t.Fatal(err)
	}
	p := NewUsagePoller(clientset, func() []string { return nodes }, webResolver(), nil, nil, nil, func(string, error) {})
	p.requestTimeout = timeout
	return p
}

// A kubelet that accepts the connection and then says nothing holds the agent
// for as long as it likes: the API server proxies the silence, and client-go
// bounds the dial and the handshake but not the response (ADR 0045).
func TestASilentKubeletDoesNotHoldThePollForever(t *testing.T) {
	server, _ := hangingKubelets(t, "node-1")
	p := pollerAgainst(t, server, 200*time.Millisecond, "node-1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.pollOnce(context.Background(), usageTestStart)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pollOnce is still waiting on a silent kubelet; the read has no deadline")
	}

	obs := p.Observation()
	if obs.PollsFailed != 2 {
		t.Errorf("polls failed = %d, want 2 — a read that timed out is a failed read, not a quiet node", obs.PollsFailed)
	}
}

// The property the deadline alone does not buy: with the nodes read one after
// another, every hung kubelet spends the whole cluster's poll budget (ADR 0045).
func TestOneUnresponsiveNodeCostsOneNodeNotTheCluster(t *testing.T) {
	const timeout = 300 * time.Millisecond
	hung := []string{"hung-1", "hung-2", "hung-3", "hung-4"}
	server, served := hangingKubelets(t, hung...)
	p := pollerAgainst(t, server, timeout, append(hung, "healthy-1", "healthy-2")...)

	start := time.Now()
	p.pollOnce(context.Background(), usageTestStart)
	elapsed := time.Since(start)

	// Serially this is eight timeouts; concurrently the four hung nodes overlap
	// and the healthy two answer immediately.
	if want := 4 * timeout; elapsed > want {
		t.Errorf("the poll took %v, over %v: the hung nodes were read one after another", elapsed, want)
	}
	if got := served.Load(); got != 12 {
		t.Errorf("%d requests reached the kubelets, want 12 — every node is read in every cycle", got)
	}
	obs := p.Observation()
	if obs.PollsAttempted != 12 || obs.PollsFailed != 8 {
		t.Errorf("attempted %d / failed %d, want 12 / 8", obs.PollsAttempted, obs.PollsFailed)
	}
}

// A response cut off at the limit must fail. Truncation is silent by nature:
// a shorter exposition is a valid exposition, and describes a node with fewer
// containers running (ADR 0045).
func TestATruncatedExpositionIsAnErrorAndNotASmallerOne(t *testing.T) {
	at := usageTestStart.Add(30 * time.Second)
	body := fixture(40, 100, at, usageTestStart)

	// Cut where a metric family ends, which is where a decoder is happiest to
	// call it a clean end of input.
	limit := strings.Index(body, "# HELP container_cpu_cfs_throttled_periods_total")
	if limit <= 0 {
		t.Fatal("fixture no longer has a family boundary to cut at")
	}

	samples, err := parseCadvisor(&cappedReader{r: strings.NewReader(body), limit: int64(limit)})
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("parsed %d samples with err %v, want the read to be refused", len(samples), err)
	}
}

func TestTheReadLimitIsAnUpperBoundAndNotAnOffByOne(t *testing.T) {
	for _, c := range []struct {
		name     string
		body     string
		refuseIt bool
	}{
		{name: "under the limit", body: "abcd"},
		{name: "exactly at the limit", body: "abcdefgh"},
		{name: "one byte over", body: "abcdefghi", refuseIt: true},
	} {
		body, err := readCapped(strings.NewReader(c.body), 8)
		if c.refuseIt {
			if !errors.Is(err, errResponseTooLarge) {
				t.Errorf("%s: err = %v, want the read refused", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: err = %v, want the body through", c.name, err)
		} else if string(body) != c.body {
			t.Errorf("%s: read %q, want %q", c.name, body, c.body)
		}
	}
}
