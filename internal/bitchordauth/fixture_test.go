package bitchordauth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureHost = "addon.example.com"

type fixture struct {
	t       *testing.T
	kid     string
	edPub   ed25519.PublicKey
	edPriv  ed25519.PrivateKey
	device  *ecdsa.PrivateKey
	jwks    *httptest.Server
	hits    atomic.Int32
	status  atomic.Int32
	gate    chan struct{}
	mu      sync.Mutex
	now     time.Time
	jwksDoc func() any
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	device, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, kid: "test-key-1", edPub: edPub, edPriv: edPriv, device: device, now: time.Unix(1_800_000_000, 0)}
	f.status.Store(http.StatusOK)
	f.jwksDoc = func() any {
		return map[string]any{"keys": []map[string]any{{"kty": "OKP", "crv": "Ed25519", "kid": f.kid, "x": base64.RawURLEncoding.EncodeToString(f.edPub)}}}
	}
	f.jwks = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.gate != nil {
			<-f.gate
		}
		if status := int(f.status.Load()); status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.jwksDoc())
	}))
	t.Cleanup(f.jwks.Close)
	return f
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fixture) auth(mode string, hosts ...string) *Auth {
	if len(hosts) == 0 {
		hosts = []string{fixtureHost}
	}
	return New(Options{Mode: mode, Hosts: hosts, JWKSURL: f.jwks.URL, Now: f.clock})
}

type claims map[string]any

func (f *fixture) claims() claims {
	now := f.clock().Unix()
	x := f.device.PublicKey.X.FillBytes(make([]byte, 32))
	y := f.device.PublicKey.Y.FillBytes(make([]byte, 32))
	return claims{
		"iss": DefaultIssuer, "aud": DefaultAudience, "sub": "device-sub-123",
		"iat": now - 10, "exp": now + 3600, "ver": 39, "sl": "strongbox",
		"cnf": map[string]any{"jwk": map[string]any{"kty": "EC", "crv": "P-256",
			"x": base64.RawURLEncoding.EncodeToString(x), "y": base64.RawURLEncoding.EncodeToString(y)}},
	}
}

func (f *fixture) sign(header map[string]any, body claims, key ed25519.PrivateKey) string {
	f.t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		f.t.Fatal(err)
	}
	p, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	signed := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(signed)))
}

func (f *fixture) token(body claims) string {
	return f.sign(map[string]any{"alg": "EdDSA", "kid": f.kid, "typ": "JWT"}, body, f.edPriv)
}

func (f *fixture) proofAt(token, host string, at time.Time) string {
	f.t.Helper()
	ath := sha256.Sum256([]byte(token))
	ts := at.UnixMilli()
	msg := fmt.Sprintf("%s\n%s\n%d\n%s", ProofContext, host, ts, base64.RawURLEncoding.EncodeToString(ath[:]))
	digest := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, f.device, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	raw := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return fmt.Sprintf("v1.%d.%s", ts, base64.RawURLEncoding.EncodeToString(raw))
}

func (f *fixture) proof(token string) string {
	return f.proofAt(token, fixtureHost, f.clock())
}
