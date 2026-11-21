// Package auth implements login sessions, the OpenID Connect login flow and
// the optional static username/password login.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Session is the state stored in the (signed, not encrypted) session cookie.
type Session struct {
	Subject string `json:"sub"`
	Name    string `json:"name,omitempty"` // display name (e-mail or username)
	Method  string `json:"m"`              // "oidc" or "password"
	CSRF    string `json:"csrf"`
	Expires int64  `json:"exp"`
}

// Signer signs and verifies small JSON payloads stored in cookies.
type Signer struct {
	key []byte
}

// NewSigner derives a signing key from secret. If secret is empty a random
// key is generated.
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
	}
	// Domain-separate the key so the raw secret is never used directly.
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("greylistd-frontend cookie signing v1"))
	return &Signer{key: m.Sum(nil)}, nil
}

var errBadCookie = errors.New("invalid cookie")

// Sign returns base64(json).base64(hmac) bound to the given purpose.
func (s *Signer) Sign(purpose string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + s.mac(purpose, payload), nil
}

// Verify checks the signature of value and decodes it into v.
func (s *Signer) Verify(purpose, value string, v any) error {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return errBadCookie
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.mac(purpose, payload))) != 1 {
		return errBadCookie
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return errBadCookie
	}
	return json.Unmarshal(b, v)
}

func (s *Signer) mac(purpose, payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// RandomToken returns a URL-safe random string with n bytes of entropy.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Sessions manages the session cookie.
type Sessions struct {
	Signer *Signer
	TTL    time.Duration
	Secure bool
	Now    func() time.Time
}

const (
	SessionCookie  = "glf_session"
	sessionPurpose = "session"
)

func (m *Sessions) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Cookie returns a cookie with the common attributes set.
func (m *Sessions) Cookie(name, value string, maxAge time.Duration) *http.Cookie {
	// Secure follows the scheme of --base-url so plain-http development setups
	// keep working; HttpOnly and SameSite are always set.
	c := &http.Cookie{ //nolint:gosec // G124: see above
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.Secure,
		SameSite: http.SameSiteLaxMode,
	}
	if maxAge < 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(maxAge / time.Second)
	}
	return c
}

// Start issues a fresh session cookie for the given identity.
func (m *Sessions) Start(w http.ResponseWriter, subject, name, method string) (*Session, error) {
	s := &Session{
		Subject: subject,
		Name:    name,
		Method:  method,
		CSRF:    RandomToken(24),
		Expires: m.now().Add(m.TTL).Unix(),
	}
	v, err := m.Signer.Sign(sessionPurpose, s)
	if err != nil {
		return nil, err
	}
	http.SetCookie(w, m.Cookie(SessionCookie, v, m.TTL))
	return s, nil
}

// Get returns the valid session of the request or nil.
func (m *Sessions) Get(r *http.Request) *Session {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil
	}
	var s Session
	if err := m.Signer.Verify(sessionPurpose, c.Value, &s); err != nil {
		return nil
	}
	if s.Subject == "" || s.CSRF == "" || m.now().Unix() >= s.Expires {
		return nil
	}
	return &s
}

// End clears the session cookie.
func (m *Sessions) End(w http.ResponseWriter) {
	http.SetCookie(w, m.Cookie(SessionCookie, "", -1))
}

// CheckCSRF compares a submitted CSRF token with the session's.
func (s *Session) CheckCSRF(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRF)) == 1
}

// PasswordChecker validates the static username/password login.
type PasswordChecker struct {
	userHash [32]byte
	passHash [32]byte
	enabled  bool
}

// NewPasswordChecker returns a checker; it rejects everything if username or
// password is empty.
func NewPasswordChecker(username, password string) *PasswordChecker {
	return &PasswordChecker{
		userHash: sha256.Sum256([]byte(username)),
		passHash: sha256.Sum256([]byte(password)),
		enabled:  username != "" && password != "",
	}
}

// Enabled reports whether a username and password are configured.
func (p *PasswordChecker) Enabled() bool { return p.enabled }

// Check compares in constant time (hashing first so lengths don't leak).
func (p *PasswordChecker) Check(username, password string) bool {
	if !p.enabled {
		return false
	}
	u := sha256.Sum256([]byte(username))
	pw := sha256.Sum256([]byte(password))
	userOK := subtle.ConstantTimeCompare(u[:], p.userHash[:])
	passOK := subtle.ConstantTimeCompare(pw[:], p.passHash[:])
	return userOK&passOK == 1
}
