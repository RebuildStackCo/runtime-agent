package nodeauth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// asymmetricMethods is the closed set of signing algorithms the verifier
// accepts. Restricting to asymmetric algorithms is the defense against the
// algorithm-confusion attack: without it, a token forged with alg=HS256 would
// be verified using the RSA/EC *public* key as an HMAC secret — public data an
// attacker also holds. "none" is likewise never accepted.
var asymmetricMethods = []string{
	"RS256", "RS384", "RS512",
	"ES256", "ES384", "ES512",
	"PS256", "PS384", "PS512",
}

// Identity is what a verified token establishes about its bearer.
type Identity struct {
	// Subject is the raw "sub" claim, e.g.
	// "system:serviceaccount:runtime-agent:runtime-agent-node".
	Subject string
	// Namespace and ServiceAccount are parsed from Subject when it has the
	// canonical service-account form; empty otherwise.
	Namespace      string
	ServiceAccount string
	// Node is the name of the node the bearer's pod is running on, taken from
	// the token rather than from anything the caller said. It is what separates
	// one node from another: every node in the DaemonSet presents the same
	// Subject, so Subject establishes the *role* and only this establishes the
	// *node* (ADR 0040). Never empty on a verified identity — a token without
	// the claim does not verify.
	Node string
	// NodeUID is the node object's UID from the same claim. It distinguishes a
	// node that was deleted and recreated under its old name.
	NodeUID string
	// Audience is the token's audience list, already checked to contain the
	// verifier's expected audience.
	Audience []string
	// ExpiresAt is the token's expiry.
	ExpiresAt time.Time
}

// nodeBoundClaims is the subset of a projected ServiceAccount token this
// verifier reads: the registered claims plus the node half of Kubernetes'
// private claim block. Measured on 1.36.1, that block is keyed "kubernetes.io"
// and carries namespace, node{name,uid}, pod{name,uid}, serviceaccount{name,uid}.
//
// Only "node" is decoded. The pod identity would pin harder, but no decision
// here turns on it, and a claim that is read has to keep being true.
type nodeBoundClaims struct {
	jwt.RegisteredClaims
	Kubernetes struct {
		Node struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"node"`
	} `json:"kubernetes.io"`
}

// Verifier validates a projected ServiceAccount token against the cluster
// signing keys and the controller's expectations. It is safe for concurrent
// use; the key provider owns any refresh.
type Verifier struct {
	keys            KeyProvider
	audience        string
	expectedSubject string
	leeway          time.Duration
}

// Option configures a Verifier.
type Option func(*Verifier)

// WithLeeway tolerates a clock skew of d when checking exp/nbf. Kubernetes
// keeps node and control-plane clocks close; a small leeway absorbs the rest.
func WithLeeway(d time.Duration) Option {
	return func(v *Verifier) { v.leeway = d }
}

// WithExpectedSubject requires the token's subject to equal sub — the node
// role's ServiceAccount (e.g.
// "system:serviceaccount:runtime-agent:runtime-agent-node"). With it, only the
// node DaemonSet's identity is accepted, not any token the cluster happens to
// have minted for the controller's audience. An empty value disables the check.
func WithExpectedSubject(sub string) Option {
	return func(v *Verifier) { v.expectedSubject = sub }
}

// NewVerifier builds a verifier that accepts tokens whose audience contains
// audience and whose signature validates against a key from keys.
func NewVerifier(keys KeyProvider, audience string, opts ...Option) *Verifier {
	v := &Verifier{keys: keys, audience: audience, leeway: time.Minute}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Verify validates tokenString and returns the established identity. It checks,
// in one pass: the signature (against the kid-selected key, asymmetric methods
// only), that expiry is present and unexpired, that the audience contains the
// configured value, that the token names the node its bearer runs on, and —
// when configured — that the subject is the expected ServiceAccount. Any failure
// returns an error and a zero Identity; nothing is trusted from an unverified
// token.
func (v *Verifier) Verify(ctx context.Context, tokenString string) (Identity, error) {
	keyfunc := func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.keys.Key(ctx, kid)
	}

	var claims nodeBoundClaims
	_, err := jwt.ParseWithClaims(tokenString, &claims, keyfunc,
		jwt.WithValidMethods(asymmetricMethods),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(v.audience),
		jwt.WithLeeway(v.leeway),
	)
	if err != nil {
		return Identity{}, fmt.Errorf("nodeauth: verifying token: %w", err)
	}

	if v.expectedSubject != "" && claims.Subject != v.expectedSubject {
		return Identity{}, fmt.Errorf("nodeauth: token subject %q is not the expected %q", claims.Subject, v.expectedSubject)
	}

	// A token with no node claim is not a token the kubelet projected into a
	// running pod — it is one something minted directly, e.g. through the
	// TokenRequest API with no bound object. Such a token carries no node
	// identity at all, so the caller could claim any node; rejecting it is the
	// only answer that keeps the rest of this package's promise (ADR 0040).
	if claims.Kubernetes.Node.Name == "" {
		return Identity{}, fmt.Errorf("nodeauth: token for subject %q carries no kubernetes.io node claim; "+
			"only a token projected into a running pod is accepted", claims.Subject)
	}

	id := Identity{
		Subject:  claims.Subject,
		Node:     claims.Kubernetes.Node.Name,
		NodeUID:  claims.Kubernetes.Node.UID,
		Audience: []string(claims.Audience),
	}
	if claims.ExpiresAt != nil {
		id.ExpiresAt = claims.ExpiresAt.Time
	}
	id.Namespace, id.ServiceAccount = splitServiceAccountSubject(claims.Subject)
	return id, nil
}

// splitServiceAccountSubject parses "system:serviceaccount:<ns>:<name>" into
// its namespace and name. A subject not in that form yields two empty strings —
// the caller keeps the raw Subject regardless.
func splitServiceAccountSubject(sub string) (namespace, name string) {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(sub, prefix) {
		return "", ""
	}
	rest := strings.TrimPrefix(sub, prefix)
	ns, sa, ok := strings.Cut(rest, ":")
	if !ok {
		return "", ""
	}
	return ns, sa
}
