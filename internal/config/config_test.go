package config

import (
	"io"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestEnvAndFlagPrecedence(t *testing.T) {
	c, err := Load([]string{"--listen", ":9000"}, env(map[string]string{
		"GREYLISTD_FRONTEND_LISTEN":        ":7000",
		"GREYLISTD_FRONTEND_SOCKET":        "/tmp/sock",
		"GREYLISTD_FRONTEND_SESSION_TTL":   "2h",
		"GREYLISTD_FRONTEND_AUTH_USERNAME": "admin",
		"GREYLISTD_FRONTEND_AUTH_PASSWORD": "pw",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" {
		t.Errorf("flag should win over env, got %q", c.Listen)
	}
	if c.SocketPath != "/tmp/sock" || c.SessionTTL != 2*time.Hour {
		t.Errorf("env not applied: %+v", c)
	}
	if !c.PasswordEnabled() || c.OIDCEnabled() {
		t.Error("login methods wrong")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"anon and login":   {[]string{"--allow-anonymous", "--auth-username", "a", "--auth-password", "b"}, "cannot be combined"},
		"half password":    {[]string{"--auth-username", "a"}, "must be set together"},
		"oidc no base url": {[]string{"--oidc-issuer", "https://idp", "--oidc-client-id", "x", "--oidc-allow-any-user"}, "--base-url is required"},
		"oidc no allow":    {[]string{"--oidc-issuer", "https://idp", "--oidc-client-id", "x", "--base-url", "https://a"}, "--oidc-allowed-emails"},
		"short secret":     {[]string{"--auth-username", "a", "--auth-password", "b", "--session-secret", "short"}, "at least 32 bytes"},
		"bad tz":           {[]string{"--auth-username", "a", "--auth-password", "b", "--timezone", "Mars/Base"}, "--timezone"},
	}
	for name, tc := range cases {
		_, err := Load(tc.args, env(nil), io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", name, err, tc.want)
		}
	}
	// No login method is valid: the server then shows an error page, or
	// lets everybody in with --allow-anonymous.
	for _, args := range [][]string{nil, {"--allow-anonymous"}} {
		c, err := Load(args, env(nil), io.Discard)
		if err != nil || c.LoginConfigured() || c.AllowAnonymous != (args != nil) {
			t.Errorf("%v: got %+v, %v", args, c, err)
		}
	}
	c, err := Load([]string{"--oidc-issuer", "https://idp", "--oidc-client-id", "x", "--base-url", "https://a/",
		"--oidc-allowed-emails", "Jonas@Example.com, b@example.com"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://a" || !c.SecureCookies() || len(c.OIDCAllowedEmails) != 2 || c.OIDCAllowedEmails[0] != "jonas@example.com" {
		t.Errorf("unexpected config: %+v", c)
	}
}

func TestDecodeSecret(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	if b, _ := decodeSecret(hex64); len(b) != 32 {
		t.Errorf("hex secret decoded to %d bytes", len(b))
	}
	raw := "this is a long enough raw secret value!"
	if b, _ := decodeSecret(raw); string(b) != raw {
		t.Error("raw secret changed")
	}
}
