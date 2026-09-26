package jellyfin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

func TestSignInReturnsAnAccessTokenTheServerHonours(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	session, err := jellyfin.SignIn(context.Background(), nil, fake.URL, "addon-uuid", "1.2.3", fakes.JellyfinUser, fakes.JellyfinPassword)
	if err != nil {
		t.Fatal(err)
	}
	if session.Token != fakes.JellyfinToken || session.Username != fakes.JellyfinUser || session.UserID != fakes.JellyfinUserID {
		t.Fatalf("session = %+v", session)
	}
	request := fake.Requests()[0]
	if header := request.Header.Get("Authorization"); !strings.HasPrefix(header, "MediaBrowser ") || strings.Contains(header, "Token=") || !strings.Contains(header, `DeviceId="addon-uuid"`) {
		t.Fatalf("Authorization = %q", header)
	}
	c := jellyfin.New(jellyfin.Options{BaseURL: fake.URL, APIKey: session.Token, DeviceID: "addon-uuid"})
	if _, err := c.Libraries(context.Background()); err != nil {
		t.Fatalf("token from sign-in rejected: %v", err)
	}
}

func TestSignInRejectsAWrongPassword(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	_, err := jellyfin.SignIn(context.Background(), nil, fake.URL, "", "1.2.3", fakes.JellyfinUser, "nope")
	if !errors.Is(err, media.ErrUnauthorized) || !strings.Contains(err.Error(), "rejected the sign-in") || strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthorizationOmitsTheTokenFieldWhenEmpty(t *testing.T) {
	with := jellyfin.Authorization("1.2.3", "", "key")
	without := jellyfin.Authorization("1.2.3", "dev-1", "")
	if !strings.HasSuffix(with, `Token="key"`) || !strings.Contains(with, `DeviceId="bitchord-selfhosted-addon"`) {
		t.Errorf("with token = %q", with)
	}
	if strings.Contains(without, "Token") || !strings.Contains(without, `DeviceId="dev-1"`) {
		t.Errorf("without token = %q", without)
	}
}
