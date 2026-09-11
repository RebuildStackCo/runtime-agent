package pprofprobe

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/pprof"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/RebuildStackCo/runtime-agent/internal/model"
)

// theReplica is the pod every candidate resolves to here.
const theReplica = "web-0"

// fixedReplica answers every candidate with one address and that pod.
func fixedReplica(addr string) Replica {
	return func(Candidate) (string, string, bool) { return addr, theReplica, true }
}

func testCounter(t *testing.T, r Replica, sink CountSink) *Counter {
	t.Helper()
	return NewCounter(r, sink, slog.New(slog.DiscardHandler))
}

// collect returns a sink that records what it was handed.
func collect(got *[]model.GoroutineCount) CountSink {
	var mu sync.Mutex
	return func(c model.GoroutineCount) {
		mu.Lock()
		defer mu.Unlock()
		*got = append(*got, c)
	}
}

// The count is read off the page Go itself renders, not off a fixture written
// here: the whole risk of this parser is that the page is not what we assumed,
// and a fixture would only test our assumption against itself.
//
// The claim is made by moving the number. A band around this process's own
// goroutine count would be satisfied by a parser that picked up any of the
// other rows; a reading that rises by exactly the goroutines that were parked
// between two readings can only be the goroutine row.
func TestTheCountComesOffThePageGoRenders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(pprof.Index))
	defer srv.Close()

	var got []model.GoroutineCount
	c := testCounter(t, fixedReplica(hostOf(t, srv.URL)), collect(&got))
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})

	const parked = 2000
	release := make(chan struct{})
	defer close(release)
	var up sync.WaitGroup
	up.Add(parked)
	for range parked {
		go func() {
			up.Done()
			<-release
		}()
	}
	up.Wait()

	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})

	if len(got) != 2 {
		t.Fatalf("took %d readings, want two: %+v", len(got), got)
	}
	// Slack in both directions for the goroutines the test server and the HTTP
	// client run and retire around each reading. It is far narrower than the
	// gap to any other row on the page.
	const slack = 100
	rose := got[1].Goroutines - got[0].Goroutines
	if rose < parked-slack || rose > parked+slack {
		t.Errorf("the count rose by %d when %d goroutines were parked (%d then %d)",
			rose, parked, got[0].Goroutines, got[1].Goroutines)
	}
	if got[0].Pod != theReplica {
		t.Errorf("pod = %q, want the replica that was read", got[0].Pod)
	}
	if got[0].Workload != (model.WorkloadRef{Kind: "Deployment", Name: "web"}) {
		t.Errorf("workload = %+v, want the candidate's", got[0].Workload)
	}
	if got := c.Snapshot(); got != (CountCoverage{Sampled: 2}) {
		t.Errorf("coverage = %+v, want two sampled", got)
	}
}

// Nothing but the index page. The same mux serves `/debug/pprof/cmdline`, and
// this reader has no more business there than the prober does.
func TestTheCounterRequestsTheIndexPathAndNothingElse(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		pprof.Index(w, r)
	}))
	defer srv.Close()

	var got []model.GoroutineCount
	c := testCounter(t, fixedReplica(hostOf(t, srv.URL)), collect(&got))
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"/debug/pprof/", "/debug/pprof/"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("requested %v, want only the index path", paths)
	}
}

// A 200 from something that is not the pprof index must not yield a number.
// The prober's answer is about a build; what answers on that address today is
// a separate question, and scraping digits out of a stranger's page would
// invent a series.
func TestAPageThatIsNotTheIndexYieldsNoCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><title>metrics</title><tr><td>4096</td><td>goroutine</td></tr></html>")
	}))
	defer srv.Close()

	var got []model.GoroutineCount
	c := testCounter(t, fixedReplica(hostOf(t, srv.URL)), collect(&got))
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})

	if len(got) != 0 {
		t.Errorf("took %d readings from a page that is not the index: %+v", len(got), got)
	}
	if got := c.Snapshot(); got != (CountCoverage{Unreadable: 1}) {
		t.Errorf("coverage = %+v, want one unreadable", got)
	}
}

// A connection that fails says nothing about the endpoint, and is counted apart
// from an answer that arrived and could not be used.
func TestAnUnreachableReplicaIsCountedApartFromAnUnreadableAnswer(t *testing.T) {
	var got []model.GoroutineCount
	c := testCounter(t, func(Candidate) (string, string, bool) { return "", "", false }, collect(&got))
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})

	if got := c.Snapshot(); got != (CountCoverage{Unreachable: 1}) {
		t.Errorf("coverage = %+v, want one unreachable", got)
	}
}

// One process, one reading. A container binding three ports is one Go runtime
// with one goroutine count; asking each port would report the same number three
// times and put three samples in the window where one belongs.
func TestOneProcessIsReadOncePerRoundHoweverManyPortsItBinds(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		pprof.Index(w, r)
	}))
	defer srv.Close()

	var got []model.GoroutineCount
	c := testCounter(t, fixedReplica(hostOf(t, srv.URL)), collect(&got))
	c.round(t.Context(), []Candidate{
		candidate("sha256:a", 8080),
		candidate("sha256:a", 6060),
		candidate("sha256:a", 9090),
	})

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("asked %d times, want one reading for one process", hits)
	}
	if len(got) != 1 {
		t.Fatalf("took %d readings, want one: %+v", len(got), got)
	}
	// The lowest port, chosen so the same endpoint is read every round.
	if got[0].ImageDigest != "sha256:a" {
		t.Errorf("digest = %q, want the candidate's", got[0].ImageDigest)
	}
}

// Two containers of one pod are two processes, and each has its own count.
func TestTwoContainersOfOnePodAreTwoReadings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(pprof.Index))
	defer srv.Close()

	sidecar := candidate("sha256:a", 6060)
	sidecar.Container = "proxy"

	var got []model.GoroutineCount
	c := testCounter(t, fixedReplica(hostOf(t, srv.URL)), collect(&got))
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060), sidecar})

	if len(got) != 2 {
		t.Fatalf("took %d readings, want one per container: %+v", len(got), got)
	}
}

// The parser's own edges, against rows the index page really produces beside
// the one this reads: the "full goroutine stack dump" link and the description
// list both carry the word and neither is a count.
func TestOnlyTheProfileRowNamedGoroutineIsACount(t *testing.T) {
	page := "<html>\n<title>/debug/pprof/</title>\n" +
		"<tr><td>17</td><td><a href='allocs?debug=1'>allocs</a></td></tr>\n" +
		"<tr><td>412</td><td><a href='goroutine?debug=1'>goroutine</a></td></tr>\n" +
		"<a href=\"goroutine?debug=2\">full goroutine stack dump</a>\n" +
		"<li><div class=profile-name>goroutine: </div> Stack traces of all current goroutines.</li>\n" +
		"</html>"
	got, ok := goroutineCount([]byte(page))
	if !ok || got != 412 {
		t.Errorf("count = %d, %v; want 412, true", got, ok)
	}
}

func TestAPageWithNoGoroutineRowIsUnreadable(t *testing.T) {
	page := "<html>\n<title>/debug/pprof/</title>\n" +
		"<tr><td>17</td><td><a href='allocs?debug=1'>allocs</a></td></tr>\n</html>"
	if got, ok := goroutineCount([]byte(page)); ok {
		t.Errorf("count = %d, true; want no count at all", got)
	}
}

// A target that fails is rested, and the reason is the round's budget rather
// than politeness: a handful of dead endpoints at the front of a sorted list
// would otherwise spend every round and the tail would never be read.
func TestAFailedReadingRestsItsTarget(t *testing.T) {
	// A listener that is closed at once gives an address nothing answers on.
	srv := httptest.NewServer(http.HandlerFunc(pprof.Index))
	addr := hostOf(t, srv.URL)
	srv.Close()

	var got []model.GoroutineCount
	c := testCounter(t, fixedReplica(addr), collect(&got))
	now := time.Now()
	c.now = func() time.Time { return now }

	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})
	if got := c.Snapshot(); got.Unreachable != 1 {
		t.Errorf("attempted %d times inside the hold, want the second round to skip", got.Unreachable)
	}

	now = now.Add(heldAfterFailure + time.Second)
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})
	if got := c.Snapshot(); got.Unreachable != 2 {
		t.Errorf("attempted %d times once the hold expired, want a second attempt", got.Unreachable)
	}
}

// A reading that worked clears the rest, so one failure does not cost five
// readings after the endpoint has come back.
func TestASuccessfulReadingClearsTheRest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(pprof.Index))
	defer srv.Close()

	var got []model.GoroutineCount
	c := testCounter(t, fixedReplica(hostOf(t, srv.URL)), collect(&got))
	c.hold(Target{ImageDigest: "sha256:a", Port: 6060}, heldAfterFailure)

	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})
	if len(got) != 0 {
		t.Fatalf("read a held target: %+v", got)
	}
	c.hold(Target{ImageDigest: "sha256:a", Port: 6060}, 0)
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})
	c.round(t.Context(), []Candidate{candidate("sha256:a", 6060)})
	if len(got) != 2 {
		t.Errorf("took %d readings after the rest was cleared, want two", len(got))
	}
}
