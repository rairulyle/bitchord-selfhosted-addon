package bitchordauth

import (
	"container/list"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultIssuer   = "https://bitchord-auth.kushagrasingh.in"
	DefaultAudience = "bitchord-addons"
	TokenHeader     = "X-BitChord-Token"
	ProofHeader     = "X-BitChord-Proof"
	ProofContext    = "bitchord-addon-proof/v1"
)

var proofRegexp = regexp.MustCompile(`^v1\.(\d{10,16})\.([A-Za-z0-9_-]{86})$`)

type Claims struct {
	Sub string `json:"sub"`
	Ver int    `json:"ver"`
	Exp int64  `json:"exp"`
	Sl  string `json:"sl"`
}

type Result struct {
	OK      bool
	Status  int
	Error   string
	Message string
	Claims  Claims
}

type Options struct {
	Hosts       []string
	Issuer      string
	JWKSURL     string
	MaxProofAge time.Duration
	ClockSkew   time.Duration
	HTTPClient  *http.Client
	Now         func() time.Time
	Mode        string // "enforce", "log", or "disabled"
}

// Config is what the setup page controls. Configure swaps it in whole, so a
// request never sees the mode of one save and the hosts of another.
type Config struct {
	Mode   string
	Hosts  []string
	Issuer string
}

const (
	keysMaxAge          = time.Hour
	unknownKidRetry     = 5 * time.Minute
	fetchBackoff        = 30 * time.Second
	maxJWKSBytes        = 64 << 10
	maxCachedDevices    = 10000
	maxRememberedProofs = 4096
)

type settings struct {
	mode    string
	hosts   []string
	issuer  string
	jwksURL string
}

type Auth struct {
	jwksOverride string
	maxProofAge  time.Duration
	clockSkew    time.Duration
	client       *http.Client
	now          func() time.Time

	settings atomic.Pointer[settings]

	// mu guards the key set and the fetch state below. It is never held
	// across a network call: one fetch runs at a time, and callers that need
	// its result wait on fetching instead.
	mu         sync.Mutex
	keysURL    string
	keys       map[string]ed25519.PublicKey
	keysAt     time.Time
	lastForced time.Time
	retryAt    time.Time
	fetchErr   error
	fetching   chan struct{}

	seen *replays

	devMu      sync.Mutex
	deviceKeys map[string]*ecdsa.PublicKey
}

func New(opts Options) *Auth {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	maxProofAge := opts.MaxProofAge
	if maxProofAge == 0 {
		maxProofAge = 120 * time.Second
	}
	clockSkew := opts.ClockSkew
	if clockSkew == 0 {
		clockSkew = 60 * time.Second
	}
	a := &Auth{
		jwksOverride: opts.JWKSURL,
		maxProofAge:  maxProofAge,
		clockSkew:    clockSkew,
		client:       client,
		now:          nowFn,
		deviceKeys:   make(map[string]*ecdsa.PublicKey),
		seen:         newReplays(maxRememberedProofs),
	}
	a.Configure(Config{Mode: opts.Mode, Hosts: opts.Hosts, Issuer: opts.Issuer})
	return a
}

// Configure applies new settings. An empty mode is disabled and an empty
// issuer is DefaultIssuer.
func (a *Auth) Configure(c Config) {
	issuer := strings.TrimRight(strings.TrimSpace(c.Issuer), "/")
	if issuer == "" {
		issuer = DefaultIssuer
	}
	jwksURL := a.jwksOverride
	if jwksURL == "" {
		jwksURL = issuer + "/.well-known/jwks.json"
	}
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	if mode == "" {
		mode = "disabled"
	}
	a.settings.Store(&settings{mode: mode, hosts: cleanHosts(c.Hosts), issuer: issuer, jwksURL: jwksURL})
}

func cleanHosts(hosts []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, h := range hosts {
		norm := NormalizeHost(h)
		if norm != "" {
			if _, ok := seen[norm]; !ok {
				seen[norm] = struct{}{}
				out = append(out, norm)
			}
		}
	}
	return out
}

// NormalizeHost reduces a host, host:port or URL to the bare lower-case
// host name a proof is signed for, so "[::1]:8080" becomes "::1".
func NormalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimPrefix(h, "https://")
	if idx := strings.IndexAny(h, "/?#"); idx != -1 {
		h = h[:idx]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	return strings.TrimSuffix(h, ".")
}

func (a *Auth) Hosts() []string {
	return slices.Clone(a.settings.Load().hosts)
}

func (a *Auth) Mode() string {
	return a.settings.Load().mode
}

func (a *Auth) Verify(token, proof string) Result {
	cfg := a.settings.Load()
	if cfg.mode == "disabled" {
		return Result{OK: true}
	}
	if token == "" || proof == "" {
		return fail(http.StatusUnauthorized, "missing", "This addon only works in the official BitChord app")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fail(http.StatusUnauthorized, "invalid_token", "Malformed token")
	}

	headerBytes, err := b64urlDecode(parts[0])
	if err != nil {
		return fail(http.StatusUnauthorized, "invalid_token", "Malformed token header")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return fail(http.StatusUnauthorized, "invalid_token", "Malformed token header")
	}
	if header.Alg != "EdDSA" || header.Kid == "" {
		return fail(http.StatusUnauthorized, "invalid_token", "Unsupported token")
	}

	signingKey, err := a.keyFor(cfg.jwksURL, header.Kid)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "unavailable", "Could not load BitChord signing keys; try again shortly")
	}
	if signingKey == nil {
		return fail(http.StatusUnauthorized, "invalid_token", "Token signed by an unknown key")
	}

	sigBytes, err := b64urlDecode(parts[2])
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return fail(http.StatusUnauthorized, "invalid_token", "Token signature is invalid")
	}
	signedPayload := []byte(parts[0] + "." + parts[1])
	if !ed25519.Verify(signingKey, signedPayload, sigBytes) {
		return fail(http.StatusUnauthorized, "invalid_token", "Token signature is invalid")
	}

	payloadBytes, err := b64urlDecode(parts[1])
	if err != nil {
		return fail(http.StatusUnauthorized, "invalid_token", "Malformed token claims")
	}
	var claims struct {
		Iss string          `json:"iss"`
		Aud json.RawMessage `json:"aud"`
		Sub string          `json:"sub"`
		Iat int64           `json:"iat"`
		Exp int64           `json:"exp"`
		Ver int             `json:"ver"`
		Sl  string          `json:"sl"`
		Cnf struct {
			JWK struct {
				Kty string `json:"kty"`
				Crv string `json:"crv"`
				X   string `json:"x"`
				Y   string `json:"y"`
			} `json:"jwk"`
		} `json:"cnf"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return fail(http.StatusUnauthorized, "invalid_token", "Malformed token claims")
	}

	now := a.now()
	nowSec := now.Unix()
	if claims.Iss != cfg.issuer || !audienceContains(claims.Aud, DefaultAudience) {
		return fail(http.StatusUnauthorized, "invalid_token", "Token is not for BitChord addons")
	}
	skewSec := int64(a.clockSkew.Seconds())
	if claims.Exp+skewSec < nowSec {
		return fail(http.StatusUnauthorized, "expired", "Token expired")
	}
	if claims.Iat-skewSec > nowSec {
		return fail(http.StatusUnauthorized, "invalid_token", "Token issued in the future")
	}
	jwk := claims.Cnf.JWK
	if jwk.Kty != "EC" || jwk.Crv != "P-256" || jwk.X == "" || jwk.Y == "" {
		return fail(http.StatusUnauthorized, "invalid_token", "Token is not bound to a device key")
	}

	matches := proofRegexp.FindStringSubmatch(proof)
	if matches == nil {
		return fail(http.StatusUnauthorized, "invalid_proof", "Malformed proof")
	}
	tsMs, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return fail(http.StatusUnauthorized, "invalid_proof", "Malformed proof timestamp")
	}
	proofAt := time.UnixMilli(tsMs)
	switch age := now.Sub(proofAt); {
	case age < -a.clockSkew:
		return fail(http.StatusUnauthorized, "invalid_proof", "Proof is dated in the future; check the device clock")
	case age > a.maxProofAge:
		return fail(http.StatusUnauthorized, "stale_proof", "Proof is too old; check the device clock")
	}

	rawProofSig, err := b64urlDecode(matches[2])
	if err != nil || len(rawProofSig) != 64 {
		return fail(http.StatusUnauthorized, "invalid_proof", "Proof signature is invalid for this addon")
	}
	rInt := new(big.Int).SetBytes(rawProofSig[:32])
	sInt := new(big.Int).SetBytes(rawProofSig[32:])

	devicePubKey, err := a.deviceKey(jwk.X, jwk.Y)
	if err != nil {
		return fail(http.StatusUnauthorized, "invalid_token", "Invalid device key in token")
	}

	sha := sha256.Sum256([]byte(token))
	ath := base64.RawURLEncoding.EncodeToString(sha[:])

	for _, host := range cfg.hosts {
		proofMsg := fmt.Sprintf("%s\n%s\n%s\n%s", ProofContext, host, matches[1], ath)
		digest := sha256.Sum256([]byte(proofMsg))
		if ecdsa.Verify(devicePubKey, digest[:], rInt, sInt) {
			if !a.seen.remember(ath+"."+proof, now, proofAt.Add(a.maxProofAge)) {
				return fail(http.StatusUnauthorized, "replayed_proof", "Proof was already used")
			}
			return Result{
				OK:     true,
				Status: http.StatusOK,
				Claims: Claims{
					Sub: claims.Sub,
					Ver: claims.Ver,
					Exp: claims.Exp,
					Sl:  claims.Sl,
				},
			}
		}
	}

	return fail(http.StatusUnauthorized, "invalid_proof", "Proof signature is invalid for this addon")
}

func (a *Auth) VerifyRequest(r *http.Request) Result {
	token := r.Header.Get(TokenHeader)
	proof := r.Header.Get(ProofHeader)
	return a.Verify(token, proof)
}

var errNoKeys = errors.New("no BitChord signing keys loaded")

// keyFor answers from the cached key set. A known key is served even when the
// set is stale, while a refresh runs in the background. An unknown kid waits
// for a fetch, at most one every unknownKidRetry, so a stream of made-up kids
// cannot hammer the issuer. A failed fetch backs off for fetchBackoff.
func (a *Auth) keyFor(jwksURL, kid string) (ed25519.PublicKey, error) {
	a.mu.Lock()
	if a.keysURL != jwksURL {
		a.keysURL, a.keys, a.keysAt, a.lastForced, a.retryAt, a.fetchErr = jwksURL, nil, time.Time{}, time.Time{}, time.Time{}, nil
	}
	now := a.now()
	key, known := a.keys[kid]
	stale := now.Sub(a.keysAt) > keysMaxAge
	if known {
		if stale {
			a.startFetchLocked(jwksURL)
		}
		a.mu.Unlock()
		return key, nil
	}
	done := a.fetching
	if done == nil && (len(a.keys) == 0 || stale || now.Sub(a.lastForced) > unknownKidRetry) {
		if done = a.startFetchLocked(jwksURL); done != nil && len(a.keys) > 0 && !stale {
			a.lastForced = now
		}
	}
	a.mu.Unlock()
	if done != nil {
		<-done
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if key, known := a.keys[kid]; known && a.keysURL == jwksURL {
		return key, nil
	}
	if len(a.keys) == 0 {
		if a.fetchErr != nil {
			return nil, a.fetchErr
		}
		return nil, errNoKeys
	}
	return nil, nil
}

// startFetchLocked starts a fetch unless one is running or the last one
// failed within fetchBackoff, and returns the channel closed when the
// running fetch ends, or nil when none runs. a.mu must be held.
func (a *Auth) startFetchLocked(jwksURL string) chan struct{} {
	if a.fetching != nil {
		return a.fetching
	}
	if a.now().Before(a.retryAt) {
		return nil
	}
	done := make(chan struct{})
	a.fetching = done
	go func() {
		keys, err := a.fetchKeys(jwksURL)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.keysURL == jwksURL {
			if err != nil {
				a.retryAt, a.fetchErr = a.now().Add(fetchBackoff), err
			} else {
				a.keys, a.keysAt, a.retryAt, a.fetchErr = keys, a.now(), time.Time{}, nil
			}
		}
		a.fetching = nil
		close(done)
	}()
	return done
}

func (a *Auth) fetchKeys(jwksURL string) (map[string]ed25519.PublicKey, error) {
	req, err := http.NewRequest(http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bitchord-addon-auth-go/1")

	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS HTTP %d", res.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxJWKSBytes {
		return nil, fmt.Errorf("JWKS larger than %d bytes", maxJWKSBytes)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}

	next := make(map[string]ed25519.PublicKey)
	for _, jwk := range doc.Keys {
		if jwk.Kty != "OKP" || jwk.Crv != "Ed25519" || jwk.Kid == "" || jwk.X == "" {
			continue
		}
		raw, err := b64urlDecode(jwk.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		next[jwk.Kid] = ed25519.PublicKey(raw)
	}
	if len(next) == 0 {
		return nil, errors.New("JWKS holds no Ed25519 keys")
	}
	return next, nil
}

func (a *Auth) deviceKey(xStr, yStr string) (*ecdsa.PublicKey, error) {
	id := xStr + "." + yStr
	a.devMu.Lock()
	key, exists := a.deviceKeys[id]
	a.devMu.Unlock()
	if exists {
		return key, nil
	}

	xBytes, err := b64urlDecode(xStr)
	if err != nil || len(xBytes) != 32 {
		return nil, errors.New("device key x is not a 32-byte coordinate")
	}
	yBytes, err := b64urlDecode(yStr)
	if err != nil || len(yBytes) != 32 {
		return nil, errors.New("device key y is not a 32-byte coordinate")
	}
	key, err = ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, xBytes...), yBytes...))
	if err != nil {
		return nil, err
	}

	a.devMu.Lock()
	if len(a.deviceKeys) > maxCachedDevices {
		a.deviceKeys = make(map[string]*ecdsa.PublicKey)
	}
	a.deviceKeys[id] = key
	a.devMu.Unlock()

	return key, nil
}

func fail(status int, code, message string) Result {
	return Result{
		OK:      false,
		Status:  status,
		Error:   code,
		Message: message,
	}
}

func (r Result) WriteRejection(w http.ResponseWriter) {
	desc := strings.ReplaceAll(r.Message, `"`, "")
	desc = strings.ReplaceAll(desc, `\`, "")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`BitChord realm="%s", error="%s", error_description="%s"`, DefaultAudience, r.Error, desc))
	w.WriteHeader(r.Status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   "bitchord_auth",
		"code":    r.Error,
		"message": r.Message,
	})
}

func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func audienceContains(raw json.RawMessage, target string) bool {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == target
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, a := range list {
			if a == target {
				return true
			}
		}
	}
	return false
}

// replays remembers proofs until they would be too old to pass anyway, so a
// captured request cannot be sent again inside that window. It holds at most
// max entries; when full it forgets the oldest first.
type replays struct {
	mu    sync.Mutex
	max   int
	order *list.List
	seen  map[string]*list.Element
}

type replayEntry struct {
	key   string
	until time.Time
}

func newReplays(max int) *replays {
	return &replays{max: max, order: list.New(), seen: make(map[string]*list.Element)}
}

// remember reports false when key is already held and has not expired.
func (c *replays) remember(key string, now, until time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for front := c.order.Front(); front != nil && !front.Value.(*replayEntry).until.After(now); front = c.order.Front() {
		delete(c.seen, front.Value.(*replayEntry).key)
		c.order.Remove(front)
	}
	if el, ok := c.seen[key]; ok {
		if el.Value.(*replayEntry).until.After(now) {
			return false
		}
		c.order.Remove(el)
		delete(c.seen, key)
	}
	for c.order.Len() >= c.max {
		front := c.order.Front()
		delete(c.seen, front.Value.(*replayEntry).key)
		c.order.Remove(front)
	}
	c.seen[key] = c.order.PushBack(&replayEntry{key: key, until: until})
	return true
}
