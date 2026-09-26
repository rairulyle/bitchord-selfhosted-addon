package setup

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

const (
	MinPasswordLength = 12
	SessionLifetime   = 7 * 24 * time.Hour
	argonTime         = 1
	argonMemory       = 64 * 1024
	argonThreads      = 4
	argonKeyLength    = 32
)

func HashPassword(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

const argonMaxMemory = 1 << 20

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	if iterations < 1 || threads < 1 || memory > argonMaxMemory {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func NewSessionKey() string {
	key := make([]byte, 32)
	rand.Read(key)
	return base64.RawStdEncoding.EncodeToString(key)
}

func sessionKey(admin *store.Admin) []byte {
	if admin == nil {
		return nil
	}
	key, err := base64.RawStdEncoding.DecodeString(admin.SessionKey)
	if err != nil {
		return nil
	}
	return key
}

// A session cookie is "<expiry unix>.<mac>", signed with the admin's session
// key, so rotating the key logs every browser out.
func issueSession(key []byte, expires time.Time) string {
	payload := strconv.FormatInt(expires.Unix(), 10)
	return payload + "." + sign(key, "session:"+payload)
}

func sessionValid(key []byte, cookie string, now time.Time) bool {
	payload, mac, ok := strings.Cut(cookie, ".")
	if !ok || len(key) == 0 {
		return false
	}
	expires, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || now.Unix() >= expires {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(sign(key, "session:"+payload)))
}

func csrfToken(key []byte, cookie string) string { return sign(key, "csrf:"+cookie) }

func csrfValid(key []byte, cookie, token string) bool {
	return len(key) > 0 && hmac.Equal([]byte(token), []byte(csrfToken(key, cookie)))
}

func sign(key []byte, message string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// limiter pauses a key after max failures inside window: five wrong
// passwords from one address, or five failed sign-ins for one server URL.
type limiter struct {
	mu       sync.Mutex
	now      func() time.Time
	max      int
	window   time.Duration
	failures map[string][]time.Time
}

func newLimiter(now func() time.Time, max int, window time.Duration) *limiter {
	return &limiter{now: now, max: max, window: window, failures: map[string][]time.Time{}}
}

// attempt reports whether key may proceed: it checks and records the
// attempt under one lock, so concurrent callers cannot all pass the check
// before any of them is recorded.
func (l *limiter) attempt(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.recent(key)
	if len(recent) >= l.max {
		return false
	}
	l.failures[key] = append(recent, l.now())
	return true
}

func (l *limiter) forgive(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if failures := l.failures[key]; len(failures) > 0 {
		l.failures[key] = failures[:len(failures)-1]
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

func (l *limiter) recent(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	var kept []time.Time
	for _, at := range l.failures[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if kept == nil {
		delete(l.failures, key)
	} else {
		l.failures[key] = kept
	}
	return kept
}

// tokenRefs holds a credential obtained by sign-in for the few minutes
// between the browser learning it succeeded and the form being saved. The
// browser only ever sees the reference, never the token.
type tokenRefs struct {
	mu   sync.Mutex
	now  func() time.Time
	ttl  time.Duration
	refs map[string]pendingToken
}

type pendingToken struct {
	kind     store.Kind
	token    string
	account  string
	deviceID string
	expires  time.Time
}

func newTokenRefs(now func() time.Time, ttl time.Duration) *tokenRefs {
	return &tokenRefs{now: now, ttl: ttl, refs: map[string]pendingToken{}}
}

func (t *tokenRefs) put(kind store.Kind, token, account, deviceID string) string {
	raw := make([]byte, 16)
	rand.Read(raw)
	ref := base64.RawURLEncoding.EncodeToString(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for key, pending := range t.refs {
		if !now.Before(pending.expires) {
			delete(t.refs, key)
		}
	}
	t.refs[ref] = pendingToken{kind: kind, token: token, account: account, deviceID: deviceID, expires: now.Add(t.ttl)}
	return ref
}

// peek reads a reference without consuming it, for a connection test before save.
func (t *tokenRefs) peek(ref string) (pendingToken, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	pending, ok := t.refs[ref]
	if !ok || !t.now().Before(pending.expires) {
		return pendingToken{}, false
	}
	return pending, true
}

// restore puts back a taken reference with its original expiry, for a save
// that failed after taking it.
func (t *tokenRefs) restore(ref string, pending pendingToken) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.refs[ref] = pending
}

// take returns a reference's credential once; a second take, or one after
// the ttl, finds nothing.
func (t *tokenRefs) take(ref string) (pendingToken, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	pending, ok := t.refs[ref]
	delete(t.refs, ref)
	if !ok || !t.now().Before(pending.expires) {
		return pendingToken{}, false
	}
	return pending, true
}
