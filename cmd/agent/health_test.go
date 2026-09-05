package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/RebuildStackCo/runtime-agent/internal/config"
	"github.com/RebuildStackCo/runtime-agent/internal/health"
)

// freeAddress returns a loopback address nothing is listening on.
func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	return addr
}

// ask returns the status of one GET, or 0 if the listener is not up yet.
func ask(t *testing.T, addr, path string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// waitFor polls until want is returned or the deadline passes.
func waitFor(t *testing.T, addr, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		if last = ask(t, addr, path); last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned %d; last was %d", path, want, last)
}

// The controller answers both questions, and readiness is the one that waits:
// it turns 200 only once the caches that gate collection have filled.
func TestTheControllerAnswersBothQuestions(t *testing.T) {
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
	if code := ask(t, addr, health.LivePath); code != http.StatusOK {
		t.Errorf("GET %s = %d, want 200", health.LivePath, code)
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

// An agent with no spool collects into its log and is not ready, because there
// is nothing for a reader to collect from. It is alive throughout: liveness is
// the process, not what the process has (ADR 0069).
func TestALogOnlyControllerIsAliveAndNeverReady(t *testing.T) {
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
	if code := ask(t, addr, health.ReadyPath); code != http.StatusServiceUnavailable {
		t.Errorf("GET %s = %d, want 503 without a spool", health.ReadyPath, code)
	}
	cancel()
	<-done
}

// No address, no listener. The chart always renders one; an agent run by hand
// has no kubelet asking and must not open a port because of it.
func TestNoConfiguredAddressOpensNoListener(t *testing.T) {
	addr := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)),
			fake.NewClientset(), &rest.Config{}, config.Config{}, config.Shape{}, time.Now())
	}()
	time.Sleep(200 * time.Millisecond)
	if code := ask(t, addr, health.LivePath); code != 0 {
		t.Errorf("something answered on %s: %d", addr, code)
	}
	cancel()
	<-done
}

// The node's readiness is its first completed pass, and its liveness is the scan
// loop's own stamp — neither asks the controller anything. This node's endpoints
// are all unreachable, which is the state a controller rollout puts every node
// in (ADR 0069 §3).
func TestTheNodeIsReadyAfterItsFirstPassWithNoControllerAtAll(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	addr := freeAddress(t)
	unreachable := "http://127.0.0.1:1/v1/node-scope"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runNode(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), []string{
			"-proc", t.TempDir(),
			"-interval", "1s",
			"-health-address", addr,
			"-scope-endpoint", unreachable,
		})
	}()

	waitFor(t, addr, health.ReadyPath, http.StatusOK)
	if code := ask(t, addr, health.LivePath); code != http.StatusOK {
		t.Errorf("GET %s = %d, want 200: a node whose controller is down is late, never dead", health.LivePath, code)
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

func TestTheNodeDeadlineCoversThreePassesAndTheirWaitOnTheController(t *testing.T) {
	if got, want := nodeLivenessDeadline(time.Minute), 4*time.Minute; got != want {
		t.Errorf("deadline at a 1m interval = %v, want %v", got, want)
	}
	// A single-pass run has no interval to multiply.
	if got := nodeLivenessDeadline(0); got != nodePassCeiling {
		t.Errorf("deadline at interval 0 = %v, want %v", got, nodePassCeiling)
	}
}

// A slow pass is not a dead agent: the deadline has to be several intervals, or
// one flush that ran long restarts a controller that was working.
func TestTheControllerDeadlineIsSeveralPassesAndNotOne(t *testing.T) {
	if controllerLivenessDeadline() <= coverageInterval {
		t.Fatalf("liveness deadline %v is not longer than one pass (%v)",
			controllerLivenessDeadline(), coverageInterval)
	}
}

// The heartbeat is the loop, and the loop is what keeps the agent alive. Both
// tests below run past the deadline: a role whose loop stopped stamping would
// have gone stale by then (ADR 0069 §2).
//
// Both compress the periods, the way internal/collector compresses watchLimits,
// and keep the ratio the real ones have — a pass far shorter than its interval.
// Compressed further, one slow pass overruns its own deadline and the test fails
// on a busy machine for the reason the agent is designed to report.
func TestAControllerThatKeepsPassingStaysAlivePastItsDeadline(t *testing.T) {
	restore := coverageInterval
	t.Cleanup(func() { coverageInterval = restore })
	coverageInterval = 250 * time.Millisecond // deadline: 750ms, for a pass of a few small writes

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

	waitFor(t, addr, health.LivePath, http.StatusOK)
	deadline := time.Now().Add(3 * controllerLivenessDeadline())
	for time.Now().Before(deadline) {
		if code := ask(t, addr, health.LivePath); code != http.StatusOK {
			t.Fatalf("GET %s = %d while the flush loop was running", health.LivePath, code)
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done
}

// And the other direction: a pass that does not return makes the node report
// itself dead, which is the whole reason liveness exists. The pass is wedged by
// the one thing that can hold it — a controller that accepts the scope query and
// never answers it.
func TestANodeWhosePassNeverReturnsGoesStale(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	restore := nodePassCeiling
	t.Cleanup(func() { nodePassCeiling = restore })
	nodePassCeiling = 100 * time.Millisecond // deadline: 250ms at the interval below

	// A controller that accepts the query and never answers. The cleanups run
	// last-registered first, so the handler is released before the server is
	// closed — closing a server with a request still in it blocks.
	stop := make(chan struct{})
	wedge := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(wedge.Close)
	t.Cleanup(func() { close(stop) })

	addr := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runNode(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), []string{
			"-proc", t.TempDir(),
			"-interval", "50ms",
			"-health-address", addr,
			"-scope-endpoint", wedge.URL,
			// The scope query is only made once there is a token to make it
			// with; without this the pass fails before it can hang.
			"-token-path", tokenFile(t),
		})
	}()

	waitFor(t, addr, health.LivePath, http.StatusServiceUnavailable)
	// Readiness never latched either: the first pass never finished.
	if code := ask(t, addr, health.ReadyPath); code != http.StatusServiceUnavailable {
		t.Errorf("GET %s = %d, want 503: no pass has completed", health.ReadyPath, code)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runNode did not stop")
	}
}

// A node that keeps scanning stays alive across many passes, including the ones
// that scan nothing because no controller answered.
func TestANodeThatKeepsScanningStaysAlivePastItsDeadline(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	restore := nodePassCeiling
	t.Cleanup(func() { nodePassCeiling = restore })
	const interval = 100 * time.Millisecond
	nodePassCeiling = 300 * time.Millisecond // deadline: 600ms

	addr := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runNode(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), []string{
			"-proc", t.TempDir(),
			"-interval", interval.String(),
			"-health-address", addr,
		})
	}()

	waitFor(t, addr, health.LivePath, http.StatusOK)
	deadline := time.Now().Add(3 * nodeLivenessDeadline(interval))
	for time.Now().Before(deadline) {
		if code := ask(t, addr, health.LivePath); code != http.StatusOK {
			t.Fatalf("GET %s = %d while the scan loop was running", health.LivePath, code)
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done
}

// tokenFile writes a stand-in for the projected token the DaemonSet mounts. Its
// contents are never verified here: the wedged server above reads no header.
func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("not-a-real-token"), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}
	return path
}

// The split this whole slice rests on: while the caches are filling the agent is
// alive and not ready. If liveness answered the readiness question, the kubelet
// would restart the controller in the middle of its first sync — which on a
// large cluster is where it spends its first minute (ADR 0069 §2, §6).
func TestWhileTheCachesFillTheAgentIsAliveAndNotReady(t *testing.T) {
	clientset := fake.NewClientset()
	listing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		once.Do(func() { close(listing) })
		<-release
		return false, nil, nil // fall through to the fake's own listing
	})

	addr := freeAddress(t)
	cfg := config.Config{Health: config.Health{ListenAddress: addr}}
	cfg.Spool.Dir = t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)),
			clientset, &rest.Config{}, cfg, config.Shape{}, time.Now())
	}()

	<-listing
	waitFor(t, addr, health.LivePath, http.StatusOK)
	if code := ask(t, addr, health.ReadyPath); code != http.StatusServiceUnavailable {
		t.Errorf("GET %s = %d while the pod cache was still listing, want 503", health.ReadyPath, code)
	}

	close(release)
	waitFor(t, addr, health.ReadyPath, http.StatusOK)

	cancel()
	<-done
}
