// Package nodeauth verifies the projected ServiceAccount tokens the node role
// presents to the controller (ADR 0010). Verification is local: the controller
// fetches the cluster's JWKS from the OIDC discovery endpoints and checks each
// token's signature, expiry, audience, and subject itself — no TokenReview, so
// no new RBAC verb. The package holds no Kubernetes client; callers pass an
// *http.Client (in-cluster, the controller supplies the API server's REST
// transport) and the issuer base URL.
package nodeauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// KeySet is an immutable set of signing keys indexed by key ID, parsed from a
// JWKS document. A token's "kid" header selects the key that must verify it.
type KeySet struct {
	keys map[string]crypto.PublicKey
}

// Key returns the public key for kid. When the token carries no kid and the
// set holds exactly one key, that key is returned — the unambiguous case some
// issuers produce. Otherwise a missing kid is an error, never a silent guess.
func (ks *KeySet) Key(kid string) (crypto.PublicKey, error) {
	if kid == "" {
		if len(ks.keys) == 1 {
			for _, k := range ks.keys {
				return k, nil
			}
		}
		return nil, fmt.Errorf("nodeauth: token has no kid and the key set is not singular")
	}
	k, ok := ks.keys[kid]
	if !ok {
		return nil, fmt.Errorf("nodeauth: no signing key for kid %q", kid)
	}
	return k, nil
}

// jwk is one key in a JWKS document; only the fields we verify with are read.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// ParseJWKS decodes a JWKS document into a KeySet. It understands the two key
// types the Kubernetes service-account issuer uses: RSA (RS256/384/512) and
// EC (ES256/384/512). Unknown key types are skipped rather than failing the
// whole set — a set may legitimately carry a type we do not verify with — but
// an empty result is an error, since it would verify nothing.
func ParseJWKS(data []byte) (*KeySet, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("nodeauth: decoding JWKS: %w", err)
	}
	keys := make(map[string]crypto.PublicKey)
	for i, k := range doc.Keys {
		pub, err := k.publicKey()
		if err != nil {
			return nil, fmt.Errorf("nodeauth: key %d (kid %q): %w", i, k.Kid, err)
		}
		if pub == nil {
			continue // a key type we do not verify with; skip it
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("nodeauth: JWKS contained no usable RSA or EC keys")
	}
	return &KeySet{keys: keys}, nil
}

// publicKey converts one JWK to a crypto public key, or (nil, nil) for a key
// type this package does not verify with.
func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := decodeBigInt(k.N)
		if err != nil {
			return nil, fmt.Errorf("modulus: %w", err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("exponent: %w", err)
		}
		if len(eBytes) == 0 {
			return nil, fmt.Errorf("empty exponent")
		}
		return &rsa.PublicKey{N: n, E: int(new(big.Int).SetBytes(eBytes).Int64())}, nil
	case "EC":
		curve, err := curveFor(k.Crv)
		if err != nil {
			return nil, err
		}
		x, err := decodeBigInt(k.X)
		if err != nil {
			return nil, fmt.Errorf("x coordinate: %w", err)
		}
		y, err := decodeBigInt(k.Y)
		if err != nil {
			return nil, fmt.Errorf("y coordinate: %w", err)
		}
		// An off-curve point cannot verify any signature (ECDSA verification
		// rejects it), so no explicit on-curve check is needed here — that
		// check matters for ECDH key agreement, which this package never does.
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	default:
		return nil, nil
	}
}

func curveFor(crv string) (elliptic.Curve, error) {
	switch crv {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", crv)
	}
}

func decodeBigInt(b64 string) (*big.Int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty value")
	}
	return new(big.Int).SetBytes(raw), nil
}

// KeySource fetches the current JWKS. The controller's implementation reads it
// from the API server over the in-cluster REST transport; tests serve it from
// an httptest server.
type KeySource interface {
	Fetch(ctx context.Context) (*KeySet, error)
}

// HTTPKeySource resolves the cluster's OIDC discovery document to its jwks_uri
// and fetches the key set, using the caller-supplied HTTP client. Passing the
// API server's REST transport is what carries the in-cluster CA and bearer
// credential; this package adds no transport policy of its own.
type HTTPKeySource struct {
	// IssuerBaseURL is where discovery lives: <base>/.well-known/openid-configuration.
	// For in-cluster validation this is the API server host.
	IssuerBaseURL string
	Client        *http.Client
}

// Fetch performs OIDC discovery and returns the parsed key set. It follows the
// jwks_uri the discovery document advertises — the OIDC-correct source of the
// signing keys — rather than assuming a fixed path.
func (s *HTTPKeySource) Fetch(ctx context.Context) (*KeySet, error) {
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := getJSON(ctx, client, s.IssuerBaseURL+"/.well-known/openid-configuration", &discovery); err != nil {
		return nil, fmt.Errorf("nodeauth: OIDC discovery: %w", err)
	}
	if discovery.JWKSURI == "" {
		return nil, fmt.Errorf("nodeauth: discovery document has no jwks_uri")
	}
	raw, err := getBytes(ctx, client, discovery.JWKSURI)
	if err != nil {
		return nil, fmt.Errorf("nodeauth: fetching JWKS: %w", err)
	}
	return ParseJWKS(raw)
}

func getBytes(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return body, nil
}

func getJSON(ctx context.Context, client *http.Client, url string, out any) error {
	body, err := getBytes(ctx, client, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// KeyProvider supplies the verifying key for a token's kid, refreshing from the
// source when a kid is unknown — a signing-key rotation on the cluster.
type KeyProvider interface {
	Key(ctx context.Context, kid string) (crypto.PublicKey, error)
}

// StaticKeys is a KeyProvider backed by a fixed KeySet; it never refreshes.
// Useful in tests and where the key set is supplied out of band.
type StaticKeys struct{ Set *KeySet }

// Key returns the key for kid from the fixed set, ignoring the context.
func (s StaticKeys) Key(_ context.Context, kid string) (crypto.PublicKey, error) {
	return s.Set.Key(kid)
}

// defaultFetchTimeout bounds one refresh of the key set. The fetch is two GETs
// to the API server on the cluster network, so a healthy one takes
// milliseconds; this is a ceiling on a hang, not a budget.
const defaultFetchTimeout = 10 * time.Second

// CachingKeySource is a KeyProvider that caches the fetched KeySet and refetches
// once when asked for a kid it does not hold — the signal that the cluster
// rotated its signing keys. Concurrent verifications share one refresh.
type CachingKeySource struct {
	Source KeySource
	// FetchTimeout bounds a single refresh; zero selects defaultFetchTimeout.
	FetchTimeout time.Duration

	mu     sync.Mutex
	cached *KeySet
}

// Key returns the key for kid, fetching the set on first use and refetching once
// if the cached set lacks it. Still absent after that is an error: the token was
// signed by a key the cluster does not advertise.
//
// The refresh runs under the mutex, which serializes it — a key rotation makes
// every verification miss the cache at once, and one attempt at a time keeps
// that off a control plane that is likely rolling. The cost is that a slow
// refresh stalls verification, which is why the fetch carries its own deadline.
func (c *CachingKeySource) Key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != nil {
		if key, err := c.cached.Key(kid); err == nil {
			return key, nil
		}
	}

	timeout := c.FetchTimeout
	if timeout <= 0 {
		timeout = defaultFetchTimeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	set, err := c.Source.Fetch(fetchCtx)
	if err != nil {
		// The previous set is kept rather than cleared: a refresh that failed
		// says nothing about the keys already in hand, and dropping them would
		// turn one unreachable API server into a cluster-wide authentication
		// outage that outlives it.
		return nil, err
	}
	c.cached = set
	return set.Key(kid)
}
