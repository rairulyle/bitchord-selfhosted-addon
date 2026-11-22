package store

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const LabelMaxLength = 16

var (
	secretPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
	linkKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{32,}$`)
	hostnamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

const MaxAppAuthHosts = 20

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
	if s.LinkKey != "" && !linkKeyPattern.MatchString(s.LinkKey) {
		problems = append(problems, errors.New("link key must be at least 32 characters of letters, digits, '-' or '_'"))
	}
	if err := ValidateAppAuth(s.AppAuth); err != nil {
		problems = append(problems, err)
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

func ValidateAppAuth(a AppAuth) error {
	var problems []error
	switch a.Mode {
	case "", AppAuthDisabled, AppAuthLog, AppAuthEnforce:
	default:
		problems = append(problems, fmt.Errorf("app authentication mode must be disabled, log or enforce, not %q", a.Mode))
	}
	if len(a.Hosts) > MaxAppAuthHosts {
		problems = append(problems, fmt.Errorf("at most %d extra hosts are allowed", MaxAppAuthHosts))
	}
	for _, host := range a.Hosts {
		if !validHost(host) {
			problems = append(problems, fmt.Errorf("allowed host %q must be a lower-case host name or IP address, with no scheme, port or path", host))
		}
	}
	if a.Issuer != "" {
		if err := ValidateIssuerURL(a.Issuer); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return !strings.ContainsAny(host, "[]")
	}
	return len(host) <= 253 && hostnamePattern.MatchString(host)
}

// ValidateIssuerURL accepts an https URL, or http on localhost, with no
// query, fragment, credentials or trailing slash.
func ValidateIssuerURL(raw string) error {
	parsed, err := url.Parse(raw)
	switch {
	case err != nil || parsed.Host == "":
		return errors.New("issuer URL must be a full URL such as https://auth.example.com")
	case parsed.Scheme != "https" && !(parsed.Scheme == "http" && parsed.Hostname() == "localhost"):
		return errors.New("issuer URL must start with https:// unless the host is localhost")
	case parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil || strings.HasSuffix(raw, "/"):
		return errors.New("issuer URL must have no query, fragment or trailing slash")
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

// ValidateSubmittedServerURL also refuses link-local and cloud metadata
// addresses. It runs only on what the setup page submits, never on load, so
// a file saved before the check existed still opens.
func ValidateSubmittedServerURL(raw string) error {
	if err := ValidateServerURL(raw); err != nil {
		return err
	}
	parsed, _ := url.Parse(raw)
	if isCloudMetadata(parsed.Hostname()) {
		return errors.New("server URL cannot target cloud instance metadata addresses")
	}
	return nil
}

func isCloudMetadata(host string) bool {
	if strings.EqualFold(host, "instance-data") || strings.EqualFold(host, "metadata.google.internal") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
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
	case AuthNavidromeSignIn:
		if server.Kind != Navidrome {
			problems = append(problems, errors.New("only a Navidrome server can be signed in with a Navidrome account"))
		}
	default:
		problems = append(problems, fmt.Errorf("unknown auth %q", server.Auth))
	}
	if utf8.RuneCountInString(server.Label) > LabelMaxLength {
		problems = append(problems, fmt.Errorf("label must be at most %d characters", LabelMaxLength))
	}
	if server.SyncInterval != "" && server.SyncInterval != "manual" && server.SyncInterval != "0" {
		d, err := time.ParseDuration(server.SyncInterval)
		if err != nil || d <= 0 {
			problems = append(problems, errors.New("sync interval must be a positive duration such as 15m, 1h, or manual"))
		}
	}
	return errors.Join(problems...)
}
