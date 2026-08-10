package nodeauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testAudience = "rebuildstack-controller"
	nodeSubject  = "system:serviceaccount:runtime-agent:runtime-agent-node"
)

// signer bundles a signing key with the kid and method used to mint tokens for
// it, plus the KeySet a verifier would build from its public half.
type signer struct {
	kid    string
	method jwt.SigningMethod
	priv   any
	set    *KeySet
}

func newRSASigner(t *testing.T, kid string) signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, method: jwt.SigningMethodRS256, priv: key, set: keySetFromJWKS(t, rsaJWKS(kid, &key.PublicKey))}
}

func newECSigner(t *testing.T, kid string) signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, method: jwt.SigningMethodES256, priv: key, set: keySetFromJWKS(t, ecJWKS(kid, &key.PublicKey))}
}

// mint signs a token with the signer's key, allowing the claims to be tweaked
// per test. The kid header is set so the verifier selects the matching key.
func (s signer) mint(t *testing.T, claims jwt.RegisteredClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(s.method, claims)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.priv)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signed
}

// validClaims is a well-formed token body: node subject, controller audience,
// unexpired.
func validClaims() jwt.RegisteredClaims {
	now := time.Now()
	return jwt.RegisteredClaims{
		Subject:   nodeSubject,
		Audience:  jwt.ClaimStrings{testAudience},
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
	}
}

func newTestVerifier(s signer) *Verifier {
	return NewVerifier(StaticKeys{Set: s.set}, testAudience, WithExpectedSubject(nodeSubject))
}

func TestVerifyValidRSAToken(t *testing.T) {
	s := newRSASigner(t, "rsa-1")
	v := newTestVerifier(s)

	id, err := v.Verify(context.Background(), s.mint(t, validClaims()))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if id.Subject != nodeSubject {
		t.Errorf("subject = %q, want %q", id.Subject, nodeSubject)
	}
	if id.Namespace != "runtime-agent" || id.ServiceAccount != "runtime-agent-node" {
		t.Errorf("parsed identity = %q/%q, want runtime-agent/runtime-agent-node", id.Namespace, id.ServiceAccount)
	}
	if len(id.Audience) != 1 || id.Audience[0] != testAudience {
		t.Errorf("audience = %v, want [%s]", id.Audience, testAudience)
	}
}

func TestVerifyValidECToken(t *testing.T) {
	s := newECSigner(t, "ec-1")
	v := newTestVerifier(s)

	if _, err := v.Verify(context.Background(), s.mint(t, validClaims())); err != nil {
		t.Fatalf("valid EC token rejected: %v", err)
	}
}

func TestVerifyRejectsWrongAudience(t *testing.T) {
	s := newRSASigner(t, "rsa-1")
	v := newTestVerifier(s)

	claims := validClaims()
	claims.Audience = jwt.ClaimStrings{"some-other-service"}
	if _, err := v.Verify(context.Background(), s.mint(t, claims)); err == nil {
		t.Fatal("token for a different audience was accepted")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := newRSASigner(t, "rsa-1")
	v := newTestVerifier(s)

	claims := validClaims()
	now := time.Now()
	claims.IssuedAt = jwt.NewNumericDate(now.Add(-2 * time.Hour))
	claims.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Hour)) // well past any leeway
	if _, err := v.Verify(context.Background(), s.mint(t, claims)); err == nil {
		t.Fatal("expired token was accepted")
	}
}

func TestVerifyRejectsMissingExpiry(t *testing.T) {
	s := newRSASigner(t, "rsa-1")
	v := newTestVerifier(s)

	claims := validClaims()
	claims.ExpiresAt = nil
	if _, err := v.Verify(context.Background(), s.mint(t, claims)); err == nil {
		t.Fatal("token without an expiry was accepted")
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	s := newRSASigner(t, "rsa-1")
	v := newTestVerifier(s)

	token := s.mint(t, validClaims())
	// Flip the first character of the signature segment. The last base64url
	// character of an RSA-2048 signature encodes only padding bits, so mutate
	// the first one, whose high-order bits are always significant.
	lastDot := strings.LastIndexByte(token, '.')
	b := []byte(token)
	i := lastDot + 1
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	if _, err := v.Verify(context.Background(), string(b)); err == nil {
		t.Fatal("token with a tampered signature was accepted")
	}
}

func TestVerifyRejectsUnknownKid(t *testing.T) {
	signed := newRSASigner(t, "rsa-1")
	other := newRSASigner(t, "rsa-2")
	// Verifier only knows `other`'s keys, but the token is signed by `signed`
	// and carries kid "rsa-1", which the key set does not hold.
	v := newTestVerifier(other)
	if _, err := v.Verify(context.Background(), signed.mint(t, validClaims())); err == nil {
		t.Fatal("token signed by an unknown key was accepted")
	}
}

func TestVerifyRejectsWrongSubject(t *testing.T) {
	s := newRSASigner(t, "rsa-1")
	v := newTestVerifier(s)

	claims := validClaims()
	claims.Subject = "system:serviceaccount:runtime-agent:some-other-sa"
	if _, err := v.Verify(context.Background(), s.mint(t, claims)); err == nil {
		t.Fatal("token from an unexpected subject was accepted")
	}
}

// TestVerifyRejectsAlgorithmConfusion is the critical negative: a token forged
// with HS256, using the RSA public key as the HMAC secret, must be rejected by
// the method allow-list before the public key is ever used as a symmetric key.
func TestVerifyRejectsAlgorithmConfusion(t *testing.T) {
	s := newRSASigner(t, "rsa-1")
	v := newTestVerifier(s)

	pubDER, err := x509.MarshalPKIXPublicKey(s.priv.(*rsa.PrivateKey).Public())
	if err != nil {
		t.Fatal(err)
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	forged.Header["kid"] = s.kid
	signed, err := forged.SignedString(pubDER)
	if err != nil {
		t.Fatalf("signing HS256 forgery: %v", err)
	}
	if _, err := v.Verify(context.Background(), signed); err == nil {
		t.Fatal("HS256 algorithm-confusion forgery was accepted")
	}
}

func TestVerifyRejectsNoneAlgorithm(t *testing.T) {
	s := newRSASigner(t, "rsa-1")
	v := newTestVerifier(s)

	tok := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims())
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("signing none-alg token: %v", err)
	}
	if _, err := v.Verify(context.Background(), signed); err == nil {
		t.Fatal("none-algorithm token was accepted")
	}
}

func TestVerifyWithoutSubjectCheckAcceptsAnySubject(t *testing.T) {
	s := newRSASigner(t, "rsa-1")
	// No WithExpectedSubject: audience is the only identity gate.
	v := NewVerifier(StaticKeys{Set: s.set}, testAudience)

	claims := validClaims()
	claims.Subject = "system:serviceaccount:other-ns:other-sa"
	id, err := v.Verify(context.Background(), s.mint(t, claims))
	if err != nil {
		t.Fatalf("token rejected with subject check disabled: %v", err)
	}
	if id.Namespace != "other-ns" || id.ServiceAccount != "other-sa" {
		t.Errorf("parsed identity = %q/%q, want other-ns/other-sa", id.Namespace, id.ServiceAccount)
	}
}

// --- JWKS building helpers -------------------------------------------------

func rsaJWKS(kid string, pub *rsa.PublicKey) []byte {
	e := big.NewInt(int64(pub.E)).Bytes()
	return jwksBytes(map[string]string{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(e),
	})
}

func ecJWKS(kid string, pub *ecdsa.PublicKey) []byte {
	// Derive the affine coordinates via the uncompressed ECDH encoding
	// (0x04 || x || y) instead of the deprecated raw big.Int fields.
	ecdhPub, err := pub.ECDH()
	if err != nil {
		panic(err)
	}
	raw := ecdhPub.Bytes()
	coord := (len(raw) - 1) / 2 // strip the 0x04 prefix, split the two halves
	x, y := raw[1:1+coord], raw[1+coord:]
	return jwksBytes(map[string]string{
		"kty": "EC",
		"kid": kid,
		"use": "sig",
		"alg": "ES256",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(x),
		"y":   base64.RawURLEncoding.EncodeToString(y),
	})
}

func jwksBytes(keys ...map[string]string) []byte {
	doc := map[string]any{"keys": keys}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return raw
}

func keySetFromJWKS(t *testing.T, raw []byte) *KeySet {
	t.Helper()
	set, err := ParseJWKS(raw)
	if err != nil {
		t.Fatalf("parsing test JWKS: %v", err)
	}
	return set
}
