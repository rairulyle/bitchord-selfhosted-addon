package setup

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash := HashPassword("correct horse battery")
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=1,p=4$") {
		t.Fatalf("hash = %s", hash)
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Error("right password rejected")
	}
	if VerifyPassword(hash, "correct horse batter") {
		t.Error("wrong password accepted")
	}
	if HashPassword("same") == HashPassword("same") {
		t.Error("two hashes of one password share a salt")
	}
	parts := strings.Split(hash, "$")
	salt, key := parts[4], parts[5]
	bad := []string{
		"", "plain", "$argon2id$v=19$m=x$salt$hash", "$bcrypt$a$b$c$d",
		fmt.Sprintf("$argon2id$v=19$m=65536,t=0,p=4$%s$%s", salt, key),
		fmt.Sprintf("$argon2id$v=19$m=65536,t=1,p=0$%s$%s", salt, key),
		fmt.Sprintf("$argon2id$v=19$m=1073741824,t=1,p=4$%s$%s", salt, key),
		fmt.Sprintf("$argon2id$v=19$m=65536,t=1,p=4$$%s", key),
		fmt.Sprintf("$argon2id$v=18$m=65536,t=1,p=4$%s$%s", salt, key),
	}
	for _, hash := range bad {
		if VerifyPassword(hash, "anything") {
			t.Errorf("malformed hash %q verified", hash)
		}
	}
}

func TestSessionsAreSignedAndExpire(t *testing.T) {
	admin := &store.Admin{SessionKey: NewSessionKey()}
	key := sessionKey(admin)
	if len(key) != 32 {
		t.Fatalf("key length %d", len(key))
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cookie := issueSession(key, now.Add(SessionLifetime))
	if !sessionValid(key, cookie, now) || !sessionValid(key, cookie, now.Add(SessionLifetime-time.Second)) {
		t.Error("fresh session rejected")
	}
	if sessionValid(key, cookie, now.Add(SessionLifetime)) {
		t.Error("expired session accepted")
	}
	if sessionValid(sessionKey(&store.Admin{SessionKey: NewSessionKey()}), cookie, now) {
		t.Error("session accepted under a rotated key")
	}
	payload, _, _ := strings.Cut(cookie, ".")
	for _, bad := range []string{"", payload, payload + ".", "9999999999." + strings.Split(cookie, ".")[1], "x" + cookie} {
		if sessionValid(key, bad, now) {
			t.Errorf("tampered cookie %q accepted", bad)
		}
	}
	if sessionValid(nil, cookie, now) || sessionValid(sessionKey(nil), cookie, now) {
		t.Error("session accepted with no key")
	}
	token := csrfToken(key, cookie)
	if !csrfValid(key, cookie, token) || csrfValid(key, cookie, token+"x") || csrfValid(key, "other", token) || csrfValid(nil, cookie, token) {
		t.Error("csrf check wrong")
	}
}

func TestLimiterAllowsFiveAttemptsThenBlocksInsideTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := newLimiter(clock, 5, time.Minute)
	for i := range 4 {
		if !l.attempt("1.2.3.4") {
			t.Fatalf("attempt %d blocked", i)
		}
	}
	if !l.attempt("1.2.3.4") {
		t.Fatal("blocked on the fifth attempt")
	}
	if l.attempt("1.2.3.4") {
		t.Fatal("allowed a sixth attempt inside the window")
	}
	if !l.attempt("5.6.7.8") {
		t.Fatal("block not keyed by address")
	}
	now = now.Add(59 * time.Second)
	if l.attempt("1.2.3.4") {
		t.Fatal("released early")
	}
	now = now.Add(2 * time.Second)
	if !l.attempt("1.2.3.4") {
		t.Fatal("not released after the window")
	}
	l.reset("1.2.3.4")
	if _, kept := l.failures["1.2.3.4"]; kept {
		t.Fatal("reset left failures behind")
	}
}

func TestLimiterAttemptIsAtomicUnderConcurrency(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := newLimiter(clock, 5, time.Minute)
	var wg sync.WaitGroup
	var allowed atomic.Int32
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.attempt("1.2.3.4") {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 5 {
		t.Fatalf("allowed = %d, want exactly 5", got)
	}
}

func TestTokenRefsAreOneTimeAndExpire(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	refs := newTokenRefs(clock, 10*time.Minute)
	ref := refs.put(store.Plex, "secret-token", "lyle")
	if strings.Contains(ref, "secret-token") || len(ref) < 20 {
		t.Fatalf("ref = %q", ref)
	}
	got, ok := refs.take(ref)
	if !ok || got.kind != store.Plex || got.token != "secret-token" || got.account != "lyle" {
		t.Fatalf("take = %+v, %v", got, ok)
	}
	if _, ok := refs.take(ref); ok {
		t.Fatal("reference usable twice")
	}
	stale := refs.put(store.Jellyfin, "t", "u")
	now = now.Add(10 * time.Minute)
	if _, ok := refs.take(stale); ok {
		t.Fatal("expired reference usable")
	}
	if _, ok := refs.take("nope"); ok {
		t.Fatal("unknown reference usable")
	}
	old := refs.put(store.Plex, "old", "u")
	now = now.Add(11 * time.Minute)
	refs.put(store.Plex, "new", "u")
	if _, kept := refs.refs[old]; kept {
		t.Fatal("expired reference not swept on put")
	}
}
