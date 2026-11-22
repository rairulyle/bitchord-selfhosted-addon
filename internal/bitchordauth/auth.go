package bitchordauth

import (
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
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

type Auth struct {
	hosts       []string
	issuer      string
	jwksURL     string
	maxProofAge time.Duration
	clockSkew   time.Duration
	client      *http.Client
	now         func() time.Time
	mode        string

	mu         sync.RWMutex
	keys       map[string]ed25519.PublicKey
	keysAt     time.Time
	lastForced time.Time
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
	issuer := opts.Issuer
	if issuer == "" {
		issuer = DefaultIssuer
	}
	jwksURL := opts.JWKSURL
	if jwksURL == "" {
		jwksURL = issuer + "/.well-known/jwks.json"
	}
	maxProofAge := opts.MaxProofAge
	if maxProofAge == 0 {
		maxProofAge = 120 * time.Second
	}
	clockSkew := opts.ClockSkew
	if clockSkew == 0 {
		clockSkew = 60 * time.Second
	}
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = "enforce"
	}

	a := &Auth{
		hosts:       cleanHosts(opts.Hosts),
		issuer:      issuer,
		jwksURL:     jwksURL,
		maxProofAge: maxProofAge,
		clockSkew:   clockSkew,
		client:      client,
		now:         nowFn,
		mode:        mode,
		keys:        make(map[string]ed25519.PublicKey),
		deviceKeys:  make(map[string]*ecdsa.PublicKey),
	}
	return a
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

func NormalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimPrefix(h, "https://")
	if idx := strings.Index(h, "/"); idx != -1 {
		h = h[:idx]
	}
	h = strings.Trim(h, "[]")
	if idx := strings.LastIndex(h, ":"); idx != -1 {
		h = h[:idx]
	}
	return strings.TrimSuffix(h, ".")
}

func (a *Auth) SetHosts(hosts []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hosts = cleanHosts(hosts)
}

func (a *Auth) Hosts() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, len(a.hosts))
	copy(out, a.hosts)
	return out
}

func (a *Auth) Mode() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mode
}

func (a *Auth) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = strings.ToLower(strings.TrimSpace(mode))
}

func (a *Auth) Verify(token, proof string) Result {
	if a.mode == "disabled" {
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

	signingKey, err := a.keyFor(header.Kid)
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
	if claims.Iss != a.issuer || !audienceContains(claims.Aud, DefaultAudience) {
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
	proofAge := time.Duration(abs(now.UnixMilli()-tsMs)) * time.Millisecond
	if proofAge > a.maxProofAge {
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

	a.mu.RLock()
	hosts := make([]string, len(a.hosts))
	copy(hosts, a.hosts)
	a.mu.RUnlock()

	for _, host := range hosts {
		proofMsg := fmt.Sprintf("%s\n%s\n%s\n%s", ProofContext, host, matches[1], ath)
		digest := sha256.Sum256([]byte(proofMsg))
		if ecdsa.Verify(devicePubKey, digest[:], rInt, sInt) {
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

func (a *Auth) keyFor(kid string) (ed25519.PublicKey, error) {
	now := a.now()
	a.mu.RLock()
	key, exists := a.keys[kid]
	stale := now.Sub(a.keysAt) > time.Hour
	unknown := !exists && now.Sub(a.lastForced) > 5*time.Minute
	a.mu.RUnlock()

	if stale || unknown {
		if err := a.loadKeys(unknown); err != nil {
			a.mu.RLock()
			defer a.mu.RUnlock()
			if len(a.keys) == 0 {
				return nil, err
			}
		}
		a.mu.RLock()
		key = a.keys[kid]
		a.mu.RUnlock()
	}
	return key, nil
}

func (a *Auth) loadKeys(isForced bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if isForced {
		a.lastForced = a.now()
	}

	req, err := http.NewRequest(http.MethodGet, a.jwksURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bitchord-addon-auth-go/1")

	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS HTTP %d", res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
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
		return err
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
	if len(next) > 0 {
		a.keys = next
		a.keysAt = a.now()
	}
	return nil
}

func (a *Auth) deviceKey(xStr, yStr string) (*ecdsa.PublicKey, error) {
	id := xStr + "." + yStr
	a.mu.RLock()
	key, exists := a.deviceKeys[id]
	a.mu.RUnlock()
	if exists {
		return key, nil
	}

	xBytes, err := b64urlDecode(xStr)
	if err != nil {
		return nil, err
	}
	yBytes, err := b64urlDecode(yStr)
	if err != nil {
		return nil, err
	}

	key = &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}
	if !key.Curve.IsOnCurve(key.X, key.Y) {
		return nil, errors.New("device public key is not on curve")
	}

	a.mu.Lock()
	if len(a.deviceKeys) > 10000 {
		a.deviceKeys = make(map[string]*ecdsa.PublicKey)
	}
	a.deviceKeys[id] = key
	a.mu.Unlock()

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

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
