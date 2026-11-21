// Package config loads the configuration from command-line flags and
// 12factor-style environment variables.
//
// Every flag "--some-name" can also be set through the environment variable
// GREYLISTD_FRONTEND_SOME_NAME. Precedence is flag > environment > default.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// EnvPrefix is prepended to the upper-cased flag name to form the name of the
// corresponding environment variable.
const EnvPrefix = "GREYLISTD_FRONTEND_"

// Config is the complete runtime configuration.
type Config struct {
	Listen        string
	SocketPath    string
	SocketTimeout time.Duration
	TimeZone      string // time zone greylistd's "list" timestamps are in
	BaseURL       string // externally visible base URL, e.g. https://greylist.example.com
	TrustProxy    bool   // honor X-Forwarded-For
	LogLevel      string

	SessionSecret []byte
	SessionTTL    time.Duration

	OIDCIssuer        string
	OIDCClientID      string
	OIDCClientSecret  string
	OIDCScopes        []string
	OIDCAllowedEmails []string
	OIDCAllowedGroups []string
	OIDCGroupsClaim   string
	OIDCAllowAnyUser  bool
	OIDCProviderName  string

	AuthUsername string
	AuthPassword string

	ShowVersion bool
}

// EnvName returns the environment variable for a flag name.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Load parses args (without the program name) and the environment.
func Load(args []string, getenv func(string) string, stderr io.Writer) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("greylistd-frontend", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var sessionSecret, scopes, allowedEmails, allowedGroups string

	fs.StringVar(&c.Listen, "listen", ":8080", "address the HTTP server listens on")
	fs.StringVar(&c.SocketPath, "socket", "/var/run/greylistd/socket", "path of greylistd's UNIX socket")
	fs.DurationVar(&c.SocketTimeout, "socket-timeout", 10*time.Second, "timeout for a single greylistd command")
	fs.StringVar(&c.TimeZone, "timezone", "", "IANA time zone greylistd's host uses for its timestamps (default: $TZ, then UTC)")
	fs.StringVar(&c.BaseURL, "base-url", "", "externally visible base URL (required for OpenID Connect), e.g. https://greylist.example.com")
	fs.BoolVar(&c.TrustProxy, "trust-proxy", false, "take the client address for logs and rate limiting from X-Forwarded-For (only behind a trusted reverse proxy)")
	fs.StringVar(&c.LogLevel, "log-level", "info", "application log level: debug, info, warn, error")

	fs.StringVar(&sessionSecret, "session-secret", "", "secret (>= 32 bytes, raw, hex or base64) used to sign session cookies; random if unset (sessions then end on restart)")
	fs.DurationVar(&c.SessionTTL, "session-ttl", 12*time.Hour, "lifetime of a login session")

	fs.StringVar(&c.OIDCIssuer, "oidc-issuer", "", "OpenID Connect issuer URL; enables OIDC login")
	fs.StringVar(&c.OIDCClientID, "oidc-client-id", "", "OpenID Connect client ID")
	fs.StringVar(&c.OIDCClientSecret, "oidc-client-secret", "", "OpenID Connect client secret")
	fs.StringVar(&scopes, "oidc-scopes", "openid,email,profile", "comma-separated OIDC scopes to request")
	fs.StringVar(&allowedEmails, "oidc-allowed-emails", "", "comma-separated e-mail addresses allowed to log in via OIDC")
	fs.StringVar(&allowedGroups, "oidc-allowed-groups", "", "comma-separated groups (from --oidc-groups-claim) allowed to log in via OIDC")
	fs.StringVar(&c.OIDCGroupsClaim, "oidc-groups-claim", "groups", "ID token / userinfo claim holding the user's groups")
	fs.BoolVar(&c.OIDCAllowAnyUser, "oidc-allow-any-user", false, "allow every user the OIDC provider authenticates (no e-mail/group restriction)")
	fs.StringVar(&c.OIDCProviderName, "oidc-provider-name", "OpenID Connect", "name of the OIDC provider shown on the login button")

	fs.StringVar(&c.AuthUsername, "auth-username", "", "username for the optional password login")
	fs.StringVar(&c.AuthPassword, "auth-password", "", "password for the optional password login (prefer the environment variable)")

	fs.BoolVar(&c.ShowVersion, "version", false, "print the version and exit")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage: greylistd-frontend [flags]\n\n"+
			"Every flag can also be set via the environment, e.g. --oidc-client-id as %s.\n\n", EnvName("oidc-client-id"))
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}

	// Fill in every flag that was not given on the command line from the
	// environment.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || envErr != nil {
			return
		}
		if v := getenv(EnvName(f.Name)); v != "" {
			if err := f.Value.Set(v); err != nil {
				envErr = fmt.Errorf("invalid value for %s: %w", EnvName(f.Name), err)
			}
		}
	})
	if envErr != nil {
		return nil, envErr
	}

	c.OIDCScopes = splitList(scopes)
	c.OIDCAllowedEmails = splitList(strings.ToLower(allowedEmails))
	c.OIDCAllowedGroups = splitList(allowedGroups)
	if c.TimeZone == "" {
		c.TimeZone = getenv("TZ")
	}

	if sessionSecret != "" {
		b, err := decodeSecret(sessionSecret)
		if err != nil {
			return nil, err
		}
		c.SessionSecret = b
	}

	if c.ShowVersion {
		return c, nil
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("--base-url must be an absolute http(s) URL, got %q", c.BaseURL))
		}
		c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	}
	if !c.OIDCEnabled() && !c.PasswordEnabled() {
		errs = append(errs, errors.New("no login method configured: set --oidc-issuer and/or --auth-username/--auth-password"))
	}
	if (c.AuthUsername == "") != (c.AuthPassword == "") {
		errs = append(errs, errors.New("--auth-username and --auth-password must be set together"))
	}
	if c.OIDCEnabled() {
		if c.OIDCClientID == "" {
			errs = append(errs, errors.New("--oidc-client-id is required with --oidc-issuer"))
		}
		if c.BaseURL == "" {
			errs = append(errs, errors.New("--base-url is required with --oidc-issuer (it forms the redirect URI)"))
		}
		if !c.OIDCAllowAnyUser && len(c.OIDCAllowedEmails) == 0 && len(c.OIDCAllowedGroups) == 0 {
			errs = append(errs, errors.New("OIDC login needs --oidc-allowed-emails and/or --oidc-allowed-groups, or explicitly --oidc-allow-any-user"))
		}
	}
	if c.SessionTTL < time.Minute {
		errs = append(errs, errors.New("--session-ttl must be at least 1m"))
	}
	if c.SessionSecret != nil && len(c.SessionSecret) < 32 {
		errs = append(errs, errors.New("--session-secret must be at least 32 bytes"))
	}
	if c.TimeZone != "" {
		if _, err := time.LoadLocation(c.TimeZone); err != nil {
			errs = append(errs, fmt.Errorf("--timezone: %w", err))
		}
	}
	return errors.Join(errs...)
}

// OIDCEnabled reports whether OpenID Connect login is configured.
func (c *Config) OIDCEnabled() bool { return c.OIDCIssuer != "" }

// PasswordEnabled reports whether the username/password login is configured.
func (c *Config) PasswordEnabled() bool { return c.AuthUsername != "" && c.AuthPassword != "" }

// Location returns the configured time zone (UTC if unset).
func (c *Config) Location() *time.Location {
	if c.TimeZone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(c.TimeZone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// SecureCookies reports whether cookies should carry the Secure attribute.
func (c *Config) SecureCookies() bool {
	return strings.HasPrefix(c.BaseURL, "https://")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// decodeSecret accepts a hex or base64 encoded secret and falls back to the
// raw bytes of the string.
func decodeSecret(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil && len(b) >= 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) >= 32 {
			return b, nil
		}
	}
	return []byte(s), nil
}
