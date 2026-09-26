package setup

import (
	"strings"
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
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=x$salt$hash", "$bcrypt$a$b$c$d"} {
		if VerifyPassword(bad, "anything") {
			t.Errorf("malformed hash %q verified", bad)
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

func TestLimiterBlocksAfterFiveFailuresInsideTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := newLimiter(clock, 5, time.Minute)
	for range 4 {
		l.fail("1.2.3.4")
	}
	if l.blocked("1.2.3.4") {
		t.Fatal("blocked after four failures")
	}
	l.fail("1.2.3.4")
	if !l.blocked("1.2.3.4") || l.blocked("5.6.7.8") {
		t.Fatal("block not keyed by address")
	}
	now = now.Add(59 * time.Second)
	if !l.blocked("1.2.3.4") {
		t.Fatal("released early")
	}
	now = now.Add(2 * time.Second)
	if l.blocked("1.2.3.4") {
		t.Fatal("not released after the window")
	}
	l.fail("1.2.3.4")
	l.reset("1.2.3.4")
	if len(l.failures) != 0 {
		t.Fatal("reset left failures behind")
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
