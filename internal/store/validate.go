package store

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"unicode/utf8"
)

const LabelMaxLength = 16

var secretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)

func Validate(s Snapshot) error {
	var problems []error
	if s.PublicURL != "" {
		if err := ValidatePublicURL(s.PublicURL); err != nil {
			problems = append(problems, err)
		}
	}
	if !secretPattern.MatchString(s.Secret) {
		problems = append(problems, errors.New("secret must be at least 16 characters of letters, digits, '-' or '_'"))
	}
	if s.ClientID == "" {
		problems = append(problems, errors.New("client id is missing"))
	}
	seen := map[string]bool{}
	for _, server := range s.Servers {
		if !slugPattern.MatchString(server.Slug) || seen[server.Slug] {
			problems = append(problems, fmt.Errorf("server slug %q is invalid or repeated", server.Slug))
		}
		seen[server.Slug] = true
		if err := ValidateServer(server); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", server.Slug, err))
		}
	}
	return errors.Join(problems...)
}

// ValidatePublicURL accepts an https origin, or http on localhost, with no
// path, query or fragment.
func ValidatePublicURL(raw string) error {
	parsed, err := url.Parse(raw)
	switch {
	case err != nil || parsed.Host == "":
		return errors.New("public URL must be a full URL such as https://music.example.com")
	case parsed.Scheme != "https" && !(parsed.Scheme == "http" && parsed.Hostname() == "localhost"):
		return errors.New("public URL must start with https:// unless the host is localhost")
	case parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil:
		return errors.New("public URL must be an origin with no path")
	}
	return nil
}

func ValidateServerURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("server URL must be an http or https URL such as http://plex:32400")
	}
	return nil
}

func ValidateServer(server Server) error {
	var problems []error
	kindKnown := false
	for _, kind := range Kinds {
		kindKnown = kindKnown || server.Kind == kind
	}
	if !kindKnown {
		problems = append(problems, fmt.Errorf("unknown server kind %q", server.Kind))
	}
	if err := ValidateServerURL(server.URL); err != nil {
		problems = append(problems, err)
	}
	if server.Token == "" {
		problems = append(problems, errors.New("a token or API key is required"))
	}
	switch server.Auth {
	case AuthToken:
	case AuthPlexSignIn:
		if server.Kind != Plex {
			problems = append(problems, errors.New("only a Plex server can be signed in with Plex"))
		}
	case AuthJellyfinSignIn:
		if server.Kind != Jellyfin {
			problems = append(problems, errors.New("only a Jellyfin server can be signed in with a Jellyfin account"))
		}
	default:
		problems = append(problems, fmt.Errorf("unknown auth %q", server.Auth))
	}
	if utf8.RuneCountInString(server.Label) > LabelMaxLength {
		problems = append(problems, fmt.Errorf("label must be at most %d characters", LabelMaxLength))
	}
	return errors.Join(problems...)
}
