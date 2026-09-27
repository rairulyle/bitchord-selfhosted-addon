package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

type Session struct {
	Token    string
	Username string
	UserID   string
}

// SignIn trades a username and password for an access token through
// /Users/AuthenticateByName. The password is used for this one request only.
func SignIn(ctx context.Context, client *http.Client, baseURL, deviceID, version, username, password string) (Session, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	body, err := json.Marshal(map[string]string{"Username": username, "Pw": password})
	if err != nil {
		return Session{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/Users/AuthenticateByName", bytes.NewReader(body))
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", Authorization(version, deviceID, ""))
	res, err := client.Do(req)
	if err != nil {
		return Session{}, fmt.Errorf("jellyfin: %w", err)
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return Session{}, fmt.Errorf("jellyfin rejected the sign-in: %w", media.ErrUnauthorized)
	case res.StatusCode != http.StatusOK:
		return Session{}, fmt.Errorf("jellyfin: %w", media.StatusError(res.StatusCode))
	}
	var answer struct {
		AccessToken string `json:"AccessToken"`
		User        struct {
			Name string `json:"Name"`
			ID   string `json:"Id"`
		} `json:"User"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&answer); err != nil {
		return Session{}, fmt.Errorf("jellyfin: malformed answer: %w", err)
	}
	if answer.AccessToken == "" {
		return Session{}, fmt.Errorf("jellyfin: malformed answer: no access token")
	}
	return Session{Token: answer.AccessToken, Username: answer.User.Name, UserID: answer.User.ID}, nil
}

// SignOut ends the session that token belongs to through /Sessions/Logout,
// which revokes the token.
func SignOut(ctx context.Context, client *http.Client, baseURL, version, deviceID, token string) error {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/Sessions/Logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", Authorization(version, deviceID, token))
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin: %w", err)
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("jellyfin: %w", media.ErrUnauthorized)
	case res.StatusCode/100 != 2:
		return fmt.Errorf("jellyfin: %w", media.StatusError(res.StatusCode))
	}
	return nil
}
