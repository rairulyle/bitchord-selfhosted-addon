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
	"testing"
	"time"
)

func TestAuthMissingHeaders(t *testing.T) {
	auth := New(Options{Hosts: []string{"addon.example.com"}})
	res := auth.Verify("", "")
	if res.OK || res.Status != http.StatusUnauthorized || res.Error != "missing" {
		t.Fatalf("expected missing rejection, got %+v", res)
	}

	rec := httptest.NewRecorder()
	res.WriteRejection(rec)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected code 401, got %d", rec.Code)
	}
	wwwAuth := rec.Header().Get("WWW-Authenticate")
	if !stringsContains(wwwAuth, `error="missing"`) {
		t.Fatalf("unexpected WWW-Authenticate: %s", wwwAuth)
	}
}

func TestAuthEndToEnd(t *testing.T) {
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-key-1"

	jwksHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "OKP",
					"crv": "Ed25519",
					"kid": kid,
					"x":   base64.RawURLEncoding.EncodeToString(edPub),
				},
			},
		})
	})
	jwksServer := httptest.NewServer(jwksHandler)
	defer jwksServer.Close()

	now := time.Now()
	auth := New(Options{
		Hosts:       []string{"addon.example.com"},
		JWKSURL:     jwksServer.URL,
		Now:         func() time.Time { return now },
		ClockSkew:   60 * time.Second,
		MaxProofAge: 120 * time.Second,
	})

	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecPub := &ecPriv.PublicKey
	xStr := base64.RawURLEncoding.EncodeToString(ecPub.X.Bytes())
	yStr := base64.RawURLEncoding.EncodeToString(ecPub.Y.Bytes())

	headerJSON := fmt.Sprintf(`{"alg":"EdDSA","kid":"%s","typ":"JWT"}`, kid)
	payloadJSON := fmt.Sprintf(`{
		"iss":"%s",
		"aud":"%s",
		"sub":"device-sub-123",
		"iat":%d,
		"exp":%d,
		"ver":39,
		"sl":"strongbox",
		"cnf":{"jwk":{"kty":"EC","crv":"P-256","x":"%s","y":"%s"}}
	}`, DefaultIssuer, DefaultAudience, now.Unix()-10, now.Unix()+3600, xStr, yStr)

	hB64 := base64.RawURLEncoding.EncodeToString([]byte(headerJSON))
	pB64 := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	tokenSig := ed25519.Sign(edPriv, []byte(hB64+"."+pB64))
	token := fmt.Sprintf("%s.%s.%s", hB64, pB64, base64.RawURLEncoding.EncodeToString(tokenSig))

	tokenSha := sha256.Sum256([]byte(token))
	ath := base64.RawURLEncoding.EncodeToString(tokenSha[:])
	tsMs := now.UnixMilli()
	proofMsg := fmt.Sprintf("%s\n%s\n%d\n%s", ProofContext, "addon.example.com", tsMs, ath)
	digest := sha256.Sum256([]byte(proofMsg))
	r, s, err := ecdsa.Sign(rand.Reader, ecPriv, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	rawSig := make([]byte, 64)
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	copy(rawSig[32-len(rBytes):32], rBytes)
	copy(rawSig[64-len(sBytes):64], sBytes)
	proof := fmt.Sprintf("v1.%d.%s", tsMs, base64.RawURLEncoding.EncodeToString(rawSig))

	res := auth.Verify(token, proof)
	if !res.OK {
		t.Fatalf("expected valid verification, got error: %s (%s)", res.Error, res.Message)
	}
	if res.Claims.Sub != "device-sub-123" || res.Claims.Ver != 39 || res.Claims.Sl != "strongbox" {
		t.Fatalf("unexpected claims: %+v", res.Claims)
	}

	// Test stale proof
	authStale := New(Options{
		Hosts:       []string{"addon.example.com"},
		JWKSURL:     jwksServer.URL,
		Now:         func() time.Time { return now.Add(5 * time.Minute) },
		ClockSkew:   60 * time.Second,
		MaxProofAge: 120 * time.Second,
	})
	resStale := authStale.Verify(token, proof)
	if resStale.OK || resStale.Error != "stale_proof" {
		t.Fatalf("expected stale_proof, got %+v", resStale)
	}

	// Test wrong host
	authWrongHost := New(Options{
		Hosts:       []string{"other.domain.com"},
		JWKSURL:     jwksServer.URL,
		Now:         func() time.Time { return now },
		ClockSkew:   60 * time.Second,
		MaxProofAge: 120 * time.Second,
	})
	resWrong := authWrongHost.Verify(token, proof)
	if resWrong.OK || resWrong.Error != "invalid_proof" {
		t.Fatalf("expected invalid_proof for host mismatch, got %+v", resWrong)
	}
}

func stringsContains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && len(sub) > 0 && indexOf(s, sub) != -1))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
