package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionRoundTripAndTamper(t *testing.T) {
	signer, _ := NewSigner([]byte(strings.Repeat("k", 32)))
	now := time.Unix(1_800_000_000, 0)
	m := &Sessions{Signer: signer, TTL: time.Hour, Now: func() time.Time { return now }}

	rec := httptest.NewRecorder()
	if _, err := m.Start(rec, "sub", "name", "password"); err != nil {
		t.Fatal(err)
	}
	cookie := rec.Result().Cookies()[0]

	req := func(v string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: v})
		return r
	}
	s := m.Get(req(cookie.Value))
	if s == nil || s.Subject != "sub" || !s.CheckCSRF(s.CSRF) || s.CheckCSRF("x") || s.CheckCSRF("") {
		t.Fatalf("round trip failed: %+v", s)
	}
	// Flip a payload character: signature must fail.
	tampered := []byte(cookie.Value)
	tampered[3] ^= 1
	if m.Get(req(string(tampered))) != nil {
		t.Error("tampered cookie accepted")
	}
	// A value signed for another purpose is not a session.
	other, _ := signer.Sign("flash", Session{Subject: "sub", CSRF: "c", Expires: now.Add(time.Hour).Unix()})
	if m.Get(req(other)) != nil {
		t.Error("cross-purpose cookie accepted")
	}
	// Expired.
	now = now.Add(2 * time.Hour)
	if m.Get(req(cookie.Value)) != nil {
		t.Error("expired session accepted")
	}
	// Other key.
	signer2, _ := NewSigner(nil)
	now = time.Unix(1_800_000_000, 0)
	if (&Sessions{Signer: signer2, TTL: time.Hour, Now: m.Now}).Get(req(cookie.Value)) != nil {
		t.Error("cookie accepted with a different key")
	}
}

func TestPasswordChecker(t *testing.T) {
	p := NewPasswordChecker("admin", "secret")
	if !p.Check("admin", "secret") || p.Check("admin", "Secret") || p.Check("Admin", "secret") || p.Check("", "") {
		t.Error("password check wrong")
	}
	if NewPasswordChecker("", "").Check("", "") {
		t.Error("disabled checker accepted empty credentials")
	}
}

func TestOIDCAllowed(t *testing.T) {
	o := NewOIDC(OIDCConfig{AllowedEmails: []string{"jonas@example.com"}, AllowedGroups: []string{"mailadmins"}})
	cases := []struct {
		id     Identity
		claims map[string]any
		want   bool
	}{
		{Identity{Email: "Jonas@Example.com"}, map[string]any{}, true},
		{Identity{Email: "jonas@example.com"}, map[string]any{"email_verified": false}, false},
		{Identity{Email: "other@example.com"}, map[string]any{}, false},
		{Identity{Email: "other@example.com", Groups: []string{"users", "mailadmins"}}, map[string]any{}, true},
		{Identity{}, map[string]any{}, false},
	}
	for i, c := range cases {
		if got := o.allowed(&c.id, c.claims); got != c.want {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
	if !NewOIDC(OIDCConfig{AllowAnyUser: true}).allowed(&Identity{}, nil) {
		t.Error("allow-any-user not honored")
	}
	if got := stringList([]any{"a", 1, "b"}); len(got) != 2 {
		t.Errorf("stringList: %v", got)
	}
}
