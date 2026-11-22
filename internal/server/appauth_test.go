package server

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/bitchordauth"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

type app struct {
	t      *testing.T
	issuer ed25519.PrivateKey
	device *ecdsa.PrivateKey
	jwks   string
}

func newApp(t *testing.T) *app {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	device, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{"kty": "OKP", "crv": "Ed25519", "kid": "k1", "x": base64.RawURLEncoding.EncodeToString(pub)}}})
	}))
	t.Cleanup(jwks.Close)
	return &app{t: t, issuer: priv, device: device, jwks: jwks.URL}
}

func (a *app) headers(host string) http.Header {
	now := time.Now()
	b64 := base64.RawURLEncoding.EncodeToString
	jose, _ := json.Marshal(map[string]any{"alg": "EdDSA", "kid": "k1"})
	claims, _ := json.Marshal(map[string]any{
		"iss": bitchordauth.DefaultIssuer, "aud": bitchordauth.DefaultAudience, "sub": "device",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"cnf": map[string]any{"jwk": map[string]any{"kty": "EC", "crv": "P-256",
			"x": b64(a.device.PublicKey.X.FillBytes(make([]byte, 32))), "y": b64(a.device.PublicKey.Y.FillBytes(make([]byte, 32)))}},
	})
	signed := b64(jose) + "." + b64(claims)
	token := signed + "." + b64(ed25519.Sign(a.issuer, []byte(signed)))
	ath := sha256.Sum256([]byte(token))
	ts := now.UnixMilli()
	digest := sha256.Sum256(fmt.Appendf(nil, "%s\n%s\n%d\n%s", bitchordauth.ProofContext, host, ts, b64(ath[:])))
	r, s, err := ecdsa.Sign(rand.Reader, a.device, digest[:])
	if err != nil {
		a.t.Fatal(err)
	}
	proof := fmt.Sprintf("v1.%d.%s", ts, b64(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)))
	header := http.Header{}
	header.Set(bitchordauth.TokenHeader, token)
	header.Set(bitchordauth.ProofHeader, proof)
	return header
}

func TestEnforceAcceptsTheAppOnThePublicHostAndExtraHosts(t *testing.T) {
	client := newApp(t)
	fake := fakes.NewPlex(t)
	log := slog.New(slog.DiscardHandler)
	entry := newEntry(t, "plex-1", store.Plex, "Home", plex.New(plex.Options{BaseURL: fake.URL, Token: fakes.PlexToken, Log: log}), log, true)
	box := &siteBox{site: Site{PublicURL: testPublic, Secret: testSecret, LinkKey: testLinkKey, AppAuth: store.AppAuth{Mode: store.AppAuthEnforce}}}
	handler := newServer(Options{
		Site: box.get, Registry: &stubRegistry{entries: map[string]*registry.Entry{"plex-1": entry}}, Version: "1.2.3", Log: log,
		Auth: bitchordauth.New(bitchordauth.Options{JWKSURL: client.jwks}),
	}).handler()
	search := "/plex-1/" + testSecret + "/search?q=new+religion"

	if rec := do(handler, http.MethodGet, search, client.headers("music.example.com")); rec.Code != http.StatusOK {
		t.Fatalf("the public URL's host = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(handler, http.MethodGet, search, client.headers("alt.example.org")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a host not yet allowed = %d", rec.Code)
	}
	box.mu.Lock()
	box.site.AppAuth.Hosts = []string{"alt.example.org"}
	box.mu.Unlock()
	if rec := do(handler, http.MethodGet, search, client.headers("alt.example.org")); rec.Code != http.StatusOK {
		t.Fatalf("an extra host after the change = %d", rec.Code)
	}
	box.mu.Lock()
	box.site.AppAuth.Hosts = nil
	box.mu.Unlock()
	if rec := do(handler, http.MethodGet, search, client.headers("alt.example.org")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a removed host stayed allowed = %d", rec.Code)
	}
	stream := do(handler, http.MethodGet, "/plex-1/"+testSecret+"/stream/101", client.headers("music.example.com"))
	if stream.Code != http.StatusOK {
		t.Fatalf("stream = %d", stream.Code)
	}
}
