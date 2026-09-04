package nodeauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseJWKSRejectsEmptySet(t *testing.T) {
	if _, err := ParseJWKS([]byte(`{"keys":[]}`)); err == nil {
		t.Fatal("empty JWKS was accepted; it verifies nothing")
	}
}

func TestParseJWKSSkipsUnknownKeyType(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	// One usable RSA key alongside an oct (symmetric) entry we cannot verify
	// with: the oct is skipped, the RSA key survives.
	var rsaDoc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(rsaJWKS("rsa-1", &key.PublicKey), &rsaDoc); err != nil {
		t.Fatal(err)
	}
	mixed := fmt.Sprintf(`{"keys":[%s,{"kty":"oct","kid":"sym","k":"AAAA"}]}`, string(rsaDoc.Keys[0]))

	set, err := ParseJWKS([]byte(mixed))
	if err != nil {
		t.Fatalf("mixed JWKS rejected: %v", err)
	}
	if _, err := set.Key("rsa-1"); err != nil {
		t.Errorf("usable RSA key missing after skipping oct: %v", err)
	}
	if _, err := set.Key("sym"); err == nil {
		t.Error("symmetric key should not have been added to the set")
	}
}

func TestKeySetKeySingularNoKid(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseJWKS(rsaJWKS("rsa-1", &key.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Key(""); err != nil {
		t.Errorf("singular key set should resolve an empty kid: %v", err)
	}
}

// jwksTestServer serves OIDC discovery and JWKS, counting JWKS hits so tests
// can assert on refresh behavior. The JWKS body is swappable to model a
// signing-key rotation.
type jwksTestServer struct {
	*httptest.Server
	jwksHits atomic.Int32
	body     atomic.Pointer[[]byte]
}

func newJWKSTestServer(t *testing.T, initial []byte) *jwksTestServer {
	t.Helper()
	s := &jwksTestServer{}
	s.body.Store(&initial)
	mux := http.NewServeMux()
	s.Server = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": s.URL, "jwks_uri": s.URL + "/openid/v1/jwks"})
	})
	mux.HandleFunc("/openid/v1/jwks", func(w http.ResponseWriter, _ *http.Request) {
		s.jwksHits.Add(1)
		_, _ = w.Write(*s.body.Load())
	})
	t.Cleanup(s.Close)
	return s
}

func TestHTTPKeySourceFollowsDiscovery(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := newJWKSTestServer(t, rsaJWKS("rsa-1", &key.PublicKey))

	src := &HTTPKeySource{IssuerBaseURL: srv.URL, Client: srv.Client()}
	set, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := set.Key("rsa-1"); err != nil {
		t.Errorf("fetched set missing rsa-1: %v", err)
	}
}

func TestCachingKeySourceCachesThenRefreshesOnUnknownKid(t *testing.T) {
	key1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := newJWKSTestServer(t, rsaJWKS("rsa-1", &key1.PublicKey))
	at := time.Unix(1_700_000_000, 0)
	c := &CachingKeySource{
		Source: &HTTPKeySource{IssuerBaseURL: srv.URL, Client: srv.Client()},
		now:    func() time.Time { return at },
	}
	ctx := context.Background()

	if _, err := c.Key(ctx, "rsa-1"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if _, err := c.Key(ctx, "rsa-1"); err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if got := srv.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetched %d times for a cached kid, want 1", got)
	}

	// Rotate the cluster's signing key: a new kid appears. Once the refresh
	// floor has passed, the lookup for the unknown kid must trigger exactly one
	// refetch and then resolve.
	key2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rotated := rsaJWKS("rsa-2", &key2.PublicKey)
	srv.body.Store(&rotated)
	at = at.Add(defaultRefetchInterval)

	if _, err := c.Key(ctx, "rsa-2"); err != nil {
		t.Fatalf("lookup after rotation: %v", err)
	}
	if got := srv.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetched %d times total, want 2 (one refresh on unknown kid)", got)
	}
}

func TestCachingKeySourceUnknownKidAfterRefreshErrors(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := newJWKSTestServer(t, rsaJWKS("rsa-1", &key.PublicKey))
	c := &CachingKeySource{Source: &HTTPKeySource{IssuerBaseURL: srv.URL, Client: srv.Client()}}

	if _, err := c.Key(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("a kid absent even after refresh should error")
	}
	if got := srv.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetched %d times, want 1 (a single refresh attempt)", got)
	}
}

// blockingSource stands in for an API server that accepted the connection and
// then stopped answering — the shape a rolling control plane produces. It never
// returns on its own; only the context ends it.
type blockingSource struct {
	entered chan struct{}
	once    sync.Once
}

func (s *blockingSource) Fetch(ctx context.Context) (*KeySet, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestARefreshIsBoundedByItsOwnDeadlineNotTheCallers is the whole of this fix.
//
// A signing-key rotation tends to coincide with a rolling API server: every
// node's token carries the new kid at once and every verification waits behind
// one refresh. That refresh had no deadline of its own — the only bound was the
// caller's context, 30s for the node client and unbounded for anything slower.
// Here the caller never gives up (context.Background()); the refresh must still
// end.
func TestARefreshIsBoundedByItsOwnDeadlineNotTheCallers(t *testing.T) {
	src := &blockingSource{entered: make(chan struct{})}
	c := &CachingKeySource{Source: src, FetchTimeout: 50 * time.Millisecond}

	done := make(chan error, 1)
	go func() { _, err := c.Key(context.Background(), "any-kid"); done <- err }()

	select {
	case <-src.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh never started")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a refresh that never answered returned no error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want a deadline; the fetch must end on its own timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh outlived its deadline; a hung API server would freeze the channel")
	}
}

// TestAFailedRefreshKeepsTheKeysAlreadyInHand: an API server that cannot be
// reached says nothing about the keys already fetched. Clearing them would turn
// a transient outage into an authentication outage that outlives it.
func TestAFailedRefreshKeepsTheKeysAlreadyInHand(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := newJWKSTestServer(t, rsaJWKS("rsa-1", &key.PublicKey))
	at := time.Unix(1_700_000_000, 0)
	// Attempts are counted on the source itself: after the change of ADR 0066,
	// "the refresh failed" and "no refresh was made" both leave the server's own
	// hit count where it was, and only the first is what this test is about.
	var attempts atomic.Int64
	source := &HTTPKeySource{IssuerBaseURL: srv.URL, Client: srv.Client()}
	c := &CachingKeySource{
		Source: keySourceFunc(func(ctx context.Context) (*KeySet, error) {
			attempts.Add(1)
			return source.Fetch(ctx)
		}),
		now: func() time.Time { return at },
	}
	ctx := context.Background()

	if _, err := c.Key(ctx, "rsa-1"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}

	// The source goes away, and a lookup for an unknown kid fails against it.
	// The clock moves past the refresh floor first, so this is a refresh that
	// was attempted and failed rather than one the floor declined to make —
	// which is the case this test is about.
	srv.Close()
	at = at.Add(defaultRefetchInterval)
	if _, err := c.Key(ctx, "rsa-2"); err == nil {
		t.Fatal("a refresh against a dead source should error")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("the source was asked %d times, want 2: the failing refresh must have been attempted", got)
	}

	// The key that was already held still verifies.
	if _, err := c.Key(ctx, "rsa-1"); err != nil {
		t.Errorf("the cached key was lost when a refresh failed: %v", err)
	}
}

// The defense of ADR 0066: the kid comes from an unverified token, so a caller
// that can reach the port picks it. Without the floor, each invented kid is two
// GETs to the API server.
func TestUnknownKidsWithinTheFloorCostNoFetch(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := newJWKSTestServer(t, rsaJWKS("rsa-1", &key.PublicKey))
	at := time.Unix(1_700_000_000, 0)
	c := &CachingKeySource{
		Source: &HTTPKeySource{IssuerBaseURL: srv.URL, Client: srv.Client()},
		now:    func() time.Time { return at },
	}

	for i := range 50 {
		if _, err := c.Key(context.Background(), fmt.Sprintf("forged-%d", i)); err == nil {
			t.Fatal("a kid the cluster does not advertise should never resolve")
		}
	}
	if got := srv.jwksHits.Load(); got != 1 {
		t.Fatalf("JWKS fetched %d times for 50 invented kids, want 1", got)
	}

	// The floor is a delay, not a latch: the next rotation is still picked up.
	at = at.Add(defaultRefetchInterval)
	if _, err := c.Key(context.Background(), "forged-51"); err == nil {
		t.Fatal("a kid the cluster does not advertise should never resolve")
	}
	if got := srv.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetched %d times, want 2 (one more once the floor passed)", got)
	}
}

// A refresh that failed is rate-limited on the same floor. An API server that is
// down is where retrying once per request costs the most and helps least.
func TestAFailedRefreshIsRateLimitedToo(t *testing.T) {
	var attempts atomic.Int64
	at := time.Unix(1_700_000_000, 0)
	c := &CachingKeySource{
		Source: keySourceFunc(func(context.Context) (*KeySet, error) {
			attempts.Add(1)
			return nil, errors.New("api server unreachable")
		}),
		now: func() time.Time { return at },
	}

	for range 20 {
		if _, err := c.Key(context.Background(), "rsa-1"); err == nil {
			t.Fatal("a lookup with no key set should error")
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("fetch attempted %d times behind an unreachable server, want 1", got)
	}
}

// keySourceFunc adapts a function to KeySource.
type keySourceFunc func(context.Context) (*KeySet, error)

func (f keySourceFunc) Fetch(ctx context.Context) (*KeySet, error) { return f(ctx) }
