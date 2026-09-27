package plex_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
)

func signIn(fake *fakes.PlexTV) *plex.SignIn {
	return &plex.SignIn{ClientID: "addon-uuid", Version: "1.2.3", PlexTV: fake.URL}
}

func TestPINFlowClaimsATokenOnceTheUserSignsIn(t *testing.T) {
	fake := fakes.NewPlexTV(t)
	s := signIn(fake)
	pin, err := s.NewPIN(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pin.ID != 1 || pin.Code != "CODE1" {
		t.Fatalf("pin = %+v", pin)
	}
	for _, want := range []string{"https://app.plex.tv/auth#?", "clientID=addon-uuid", "code=CODE1", "context%5Bdevice%5D%5Bproduct%5D=BitChord+Selfhosted+Addon"} {
		if !strings.Contains(pin.AuthURL, want) {
			t.Errorf("AuthURL %q lacks %s", pin.AuthURL, want)
		}
	}
	first := fake.Requests()[0]
	if first.Method != http.MethodPost || first.Path != "/api/v2/pins" || first.Query != "strong=true" ||
		first.Header.Get("X-Plex-Client-Identifier") != "addon-uuid" || first.Header.Get("X-Plex-Product") != plex.Product {
		t.Fatalf("pin request = %+v", first)
	}
	if _, ok, err := s.Claim(context.Background(), pin.ID); ok || err != nil {
		t.Fatalf("claimed before sign-in: %v, %v", ok, err)
	}
	fake.Claim(pin.ID)
	account, ok, err := s.Claim(context.Background(), pin.ID)
	if err != nil || !ok || account.Token != fakes.PlexTVToken || account.Username != fakes.PlexTVUsername {
		t.Fatalf("claim = %+v, %v, %v", account, ok, err)
	}
	if _, _, err := s.Claim(context.Background(), 99); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("unknown pin: %v", err)
	}
}

func TestServersOrdersLocalConnectionsFirstAndSkipsPlayers(t *testing.T) {
	fake := fakes.NewPlexTV(t)
	fake.Resources = fakes.PlexTVResources("http://home:32400", "http://dead:32400")
	got, err := signIn(fake).Servers(context.Background(), fakes.PlexTVToken)
	if err != nil {
		t.Fatal(err)
	}
	want := []plex.Resource{
		{Name: "Home", Connections: []string{"http://dead:32400", "http://home:32400", "https://1-2-3-4.abc.plex.direct:32400", "https://relay.plex.direct:8443"}},
		{Name: "Parents", Connections: []string{"http://dead:32400"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Servers = %+v", got)
	}
	last := fake.Requests()[len(fake.Requests())-1]
	if last.Header.Get("X-Plex-Token") != fakes.PlexTVToken || !strings.Contains(last.Query, "includeHttps=1") {
		t.Fatalf("resources request = %+v", last)
	}
	if _, err := signIn(fake).Servers(context.Background(), "wrong"); !errors.Is(err, media.ErrUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
}

func TestSignInReportsAnUnreachablePlexTV(t *testing.T) {
	s := &plex.SignIn{ClientID: "addon-uuid", Version: "1.2.3", PlexTV: fakes.DeadURL(t)}
	if _, err := s.NewPIN(context.Background()); err == nil || !strings.Contains(err.Error(), "plex.tv") {
		t.Fatalf("err = %v", err)
	}
}
