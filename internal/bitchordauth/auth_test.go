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
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuthMissingHeaders(t *testing.T) {
	auth := New(Options{Mode: "enforce", Hosts: []string{"addon.example.com"}})
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
		Mode:        "enforce",
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
		Mode:        "enforce",
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
		Mode:        "enforce",
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

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"addon.example.com":               "addon.example.com",
		"Addon.Example.com.":              "addon.example.com",
		"addon.example.com:8443":          "addon.example.com",
		"https://addon.example.com/x?y=1": "addon.example.com",
		"https://addon.example.com:443":   "addon.example.com",
		"[::1]:8080":                      "::1",
		"[::1]":                           "::1",
		"::1":                             "::1",
		"http://[2001:db8::7]:8080/path":  "2001:db8::7",
		"2001:db8::7":                     "2001:db8::7",
		"192.168.1.5:8080":                "192.168.1.5",
		" ":                               "",
	}
	for in, want := range cases {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
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

func TestKeysAreFetchedOnceForConcurrentRequests(t *testing.T) {
	f := newFixture(t)
	f.gate = make(chan struct{})
	auth := f.auth("enforce")
	token := f.token(f.claims())
	var wg sync.WaitGroup
	results := make([]Result, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = auth.Verify(token, f.proof(token))
		}()
	}
	for f.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(f.gate)
	wg.Wait()
	if hits := f.hits.Load(); hits != 1 {
		t.Fatalf("JWKS fetched %d times, want 1", hits)
	}
	for i, res := range results {
		if !res.OK {
			t.Errorf("request %d: %+v", i, res)
		}
	}
}

func TestStaleKeysAreServedWhileARefreshRuns(t *testing.T) {
	f := newFixture(t)
	auth := f.auth("enforce")
	token := f.token(f.claims())
	if res := auth.Verify(token, f.proof(token)); !res.OK {
		t.Fatalf("first verify: %+v", res)
	}
	f.gate = make(chan struct{})
	defer close(f.gate)
	f.advance(keysMaxAge + time.Minute)
	token = f.token(f.claims())
	if res := auth.Verify(token, f.proof(token)); !res.OK {
		t.Fatalf("verify with stale keys: %+v", res)
	}
	for f.hits.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
}

func TestAFailedFetchBacksOff(t *testing.T) {
	f := newFixture(t)
	f.status.Store(http.StatusInternalServerError)
	auth := f.auth("enforce")
	token := f.token(f.claims())
	for range 3 {
		if res := auth.Verify(token, f.proof(token)); res.OK || res.Status != http.StatusServiceUnavailable {
			t.Fatalf("verify while the issuer fails: %+v", res)
		}
	}
	if hits := f.hits.Load(); hits != 1 {
		t.Fatalf("JWKS fetched %d times during the backoff, want 1", hits)
	}
	f.status.Store(http.StatusOK)
	f.advance(fetchBackoff + time.Second)
	token = f.token(f.claims())
	if res := auth.Verify(token, f.proof(token)); !res.OK {
		t.Fatalf("verify after the backoff: %+v", res)
	}
}

func TestAnOversizedKeySetIsRefused(t *testing.T) {
	f := newFixture(t)
	f.jwksDoc = func() any { return map[string]any{"pad": strings.Repeat("x", maxJWKSBytes)} }
	auth := f.auth("enforce")
	token := f.token(f.claims())
	if res := auth.Verify(token, f.proof(token)); res.OK || res.Status != http.StatusServiceUnavailable {
		t.Fatalf("verify against an oversized key set: %+v", res)
	}
}

func TestAnUnknownKidRefetchesAtMostEveryFewMinutes(t *testing.T) {
	f := newFixture(t)
	auth := f.auth("enforce")
	token := f.token(f.claims())
	if res := auth.Verify(token, f.proof(token)); !res.OK {
		t.Fatalf("first verify: %+v", res)
	}
	unknown := f.sign(map[string]any{"alg": "EdDSA", "kid": "unknown"}, f.claims(), f.edPriv)
	for range 3 {
		if res := auth.Verify(unknown, f.proof(unknown)); res.OK || res.Error != "invalid_token" {
			t.Fatalf("verify with an unknown kid: %+v", res)
		}
	}
	if hits := f.hits.Load(); hits != 2 {
		t.Fatalf("JWKS fetched %d times, want 2", hits)
	}
	f.advance(unknownKidRetry + time.Second)
	auth.Verify(unknown, f.proof(unknown))
	if hits := f.hits.Load(); hits != 3 {
		t.Fatalf("JWKS fetched %d times after the wait, want 3", hits)
	}
}

func TestProofTiming(t *testing.T) {
	f := newFixture(t)
	auth := f.auth("enforce")
	cases := map[string]struct {
		offset time.Duration
		want   string
	}{
		"now":                     {0, ""},
		"within the skew ahead":   {50 * time.Second, ""},
		"beyond the skew ahead":   {90 * time.Second, "invalid_proof"},
		"far in the future":       {time.Hour, "invalid_proof"},
		"just inside the max age": {-110 * time.Second, ""},
		"older than the max age":  {-130 * time.Second, "stale_proof"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			token := f.token(f.claims())
			res := auth.Verify(token, f.proofAt(token, fixtureHost, f.clock().Add(tc.offset)))
			if tc.want == "" && !res.OK || tc.want != "" && (res.OK || res.Error != tc.want) {
				t.Fatalf("got %+v, want %q", res, tc.want)
			}
		})
	}
}

func TestAReplayedProofIsRefused(t *testing.T) {
	f := newFixture(t)
	auth := f.auth("enforce")
	token := f.token(f.claims())
	proof := f.proof(token)
	if res := auth.Verify(token, proof); !res.OK {
		t.Fatalf("first use: %+v", res)
	}
	if res := auth.Verify(token, proof); res.OK || res.Error != "replayed_proof" {
		t.Fatalf("replay: %+v", res)
	}
	if res := auth.Verify(token, f.proof(token)); !res.OK {
		t.Fatalf("a fresh proof for the same token: %+v", res)
	}
}

func TestReplaysForgetsExpiredAndOldestEntries(t *testing.T) {
	c := newReplays(2)
	now := time.Unix(1000, 0)
	if !c.remember("a", now, now.Add(time.Minute)) || c.remember("a", now, now.Add(time.Minute)) {
		t.Fatal("a second sighting inside the window must be refused")
	}
	if !c.remember("a", now.Add(2*time.Minute), now.Add(3*time.Minute)) {
		t.Fatal("an expired entry must be forgotten")
	}
	c.remember("b", now.Add(2*time.Minute), now.Add(3*time.Minute))
	c.remember("c", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if len(c.seen) != 2 || c.order.Len() != 2 {
		t.Fatalf("held %d keys and %d entries, want 2", len(c.seen), c.order.Len())
	}
	if _, ok := c.seen["a"]; ok {
		t.Fatal("the oldest entry must go first")
	}
}

func TestADeviceKeyOffTheCurveIsRefused(t *testing.T) {
	f := newFixture(t)
	auth := f.auth("enforce")
	one := base64.RawURLEncoding.EncodeToString(append(make([]byte, 31), 1))
	short := base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3})
	for name, xy := range map[string][2]string{"off the curve": {one, one}, "short coordinates": {short, short}} {
		t.Run(name, func(t *testing.T) {
			body := f.claims()
			body["cnf"] = map[string]any{"jwk": map[string]any{"kty": "EC", "crv": "P-256", "x": xy[0], "y": xy[1]}}
			token := f.token(body)
			if res := auth.Verify(token, f.proof(token)); res.OK || res.Error != "invalid_token" {
				t.Fatalf("got %+v", res)
			}
		})
	}
}

func TestTokensThatMustBeRefused(t *testing.T) {
	f := newFixture(t)
	auth := f.auth("enforce")
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	with := func(key string, value any) string {
		body := f.claims()
		body[key] = value
		return f.token(body)
	}
	valid := f.token(f.claims())
	parts := strings.Split(valid, ".")
	tampered := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	swapped := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+DefaultIssuer+`","aud":"`+DefaultAudience+`","sub":"someone-else"}`)) + "." + parts[2]
	now := f.clock().Unix()
	cases := map[string]struct {
		token string
		want  string
	}{
		"wrong issuer":          {with("iss", "https://evil.example.com"), "invalid_token"},
		"wrong audience":        {with("aud", "another-audience"), "invalid_token"},
		"wrong audience list":   {with("aud", []string{"a", "b"}), "invalid_token"},
		"expired":               {with("exp", now-3600), "expired"},
		"no expiry":             {with("exp", 0), "expired"},
		"issued in the future":  {with("iat", now+3600), "invalid_token"},
		"tampered signature":    {tampered, "invalid_token"},
		"swapped claims":        {swapped, "invalid_token"},
		"signed by another key": {f.sign(map[string]any{"alg": "EdDSA", "kid": f.kid}, f.claims(), otherKey), "invalid_token"},
		"alg none":              {f.sign(map[string]any{"alg": "none", "kid": f.kid}, f.claims(), f.edPriv), "invalid_token"},
		"alg HS256":             {f.sign(map[string]any{"alg": "HS256", "kid": f.kid}, f.claims(), f.edPriv), "invalid_token"},
		"no device key":         {with("cnf", map[string]any{}), "invalid_token"},
		"malformed":             {"not-a-token", "invalid_token"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := auth.Verify(tc.token, f.proof(tc.token))
			if res.OK || res.Status != http.StatusUnauthorized || res.Error != tc.want {
				t.Fatalf("got %+v, want %s", res, tc.want)
			}
		})
	}
	if res := auth.Verify(valid, f.proof(valid)); !res.OK {
		t.Fatalf("the untouched token was refused: %+v", res)
	}
}

func TestAnAudienceListContainingTheAddonsIsAccepted(t *testing.T) {
	f := newFixture(t)
	body := f.claims()
	body["aud"] = []string{"other", DefaultAudience}
	token := f.token(body)
	if res := f.auth("enforce").Verify(token, f.proof(token)); !res.OK {
		t.Fatalf("got %+v", res)
	}
}

func TestAnotherIssuerNeedsItsOwnSetting(t *testing.T) {
	f := newFixture(t)
	auth := f.auth("enforce")
	body := f.claims()
	body["iss"] = "https://auth.example.com"
	token := f.token(body)
	if res := auth.Verify(token, f.proof(token)); res.OK {
		t.Fatal("a token from an issuer that is not configured passed")
	}
	auth.Configure(Config{Mode: "enforce", Hosts: []string{fixtureHost}, Issuer: "https://auth.example.com/"})
	if res := auth.Verify(token, f.proof(token)); !res.OK {
		t.Fatalf("after configuring the issuer: %+v", res)
	}
}

func TestAProofForAnotherHostIsRefused(t *testing.T) {
	f := newFixture(t)
	auth := f.auth("enforce", "addon.example.com", "::1")
	token := f.token(f.claims())
	if res := auth.Verify(token, f.proofAt(token, "evil.example.com", f.clock())); res.OK || res.Error != "invalid_proof" {
		t.Fatalf("another host: %+v", res)
	}
	if res := auth.Verify(token, f.proofAt(token, "::1", f.clock())); !res.OK {
		t.Fatalf("an allowed ipv6 host: %+v", res)
	}
	body := f.claims()
	body["sub"] = "another-device"
	other := f.token(body)
	if res := auth.Verify(other, f.proof(token)); res.OK || res.Error != "invalid_proof" {
		t.Fatalf("a proof bound to another token: %+v", res)
	}
}

func TestDisabledModeChecksNothing(t *testing.T) {
	f := newFixture(t)
	auth := f.auth("disabled")
	for name, pair := range map[string][2]string{"no headers": {"", ""}, "garbage": {"x.y.z", "v1.1.x"}} {
		if res := auth.Verify(pair[0], pair[1]); !res.OK {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if hits := f.hits.Load(); hits != 0 {
		t.Fatalf("disabled mode fetched the JWKS %d times", hits)
	}
	if New(Options{}).Mode() != "disabled" {
		t.Fatal("the default mode is not disabled")
	}
}
