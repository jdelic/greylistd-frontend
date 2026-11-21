package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig configures the OpenID Connect login.
type OIDCConfig struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	Scopes        []string
	AllowedEmails []string // lower-case
	AllowedGroups []string
	GroupsClaim   string
	AllowAnyUser  bool
}

// OIDC performs the authorization code flow (with PKCE and nonce). Provider
// discovery happens lazily on first use and is retried on failure, so the
// server can start while the identity provider is unreachable.
type OIDC struct {
	cfg OIDCConfig

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// NewOIDC returns an OIDC flow for cfg.
func NewOIDC(cfg OIDCConfig) *OIDC {
	return &OIDC{cfg: cfg}
}

func (o *OIDC) init(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(ctx, o.cfg.Issuer)
	if err != nil {
		return fmt.Errorf("oidc discovery for %s: %w", o.cfg.Issuer, err)
	}
	scopes := o.cfg.Scopes
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}
	o.provider = p
	o.verifier = p.Verifier(&oidc.Config{ClientID: o.cfg.ClientID})
	o.oauth = &oauth2.Config{
		ClientID:     o.cfg.ClientID,
		ClientSecret: o.cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  o.cfg.RedirectURL,
		Scopes:       scopes,
	}
	return nil
}

// FlowState is kept in a short-lived signed cookie between the redirect to the
// provider and the callback.
type FlowState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Expires  int64  `json:"exp"`
}

// Begin returns the provider's authorization URL and the flow state to store.
func (o *OIDC) Begin(ctx context.Context) (string, *FlowState, error) {
	if err := o.init(ctx); err != nil {
		return "", nil, err
	}
	fs := &FlowState{
		State:    RandomToken(24),
		Nonce:    RandomToken(24),
		Verifier: oauth2.GenerateVerifier(),
		Expires:  time.Now().Add(10 * time.Minute).Unix(),
	}
	u := o.oauth.AuthCodeURL(fs.State, oidc.Nonce(fs.Nonce), oauth2.S256ChallengeOption(fs.Verifier))
	return u, fs, nil
}

// Identity is the authenticated user.
type Identity struct {
	Subject string
	Email   string
	Name    string
	Groups  []string
}

// ErrNotAllowed is returned when the user authenticated but is not permitted.
var ErrNotAllowed = errors.New("user is not allowed to use this application")

// Finish exchanges the authorization code and validates the ID token.
func (o *OIDC) Finish(ctx context.Context, fs *FlowState, state, code string) (*Identity, error) {
	if err := o.init(ctx); err != nil {
		return nil, err
	}
	if fs == nil || state == "" || state != fs.State || time.Now().Unix() > fs.Expires {
		return nil, errors.New("invalid or expired login state")
	}
	tok, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(fs.Verifier))
	if err != nil {
		return nil, fmt.Errorf("code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, errors.New("token response contains no id_token")
	}
	idt, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("id_token verification: %w", err)
	}
	if idt.Nonce != fs.Nonce {
		return nil, errors.New("id_token nonce mismatch")
	}

	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return nil, fmt.Errorf("id_token claims: %w", err)
	}
	// Many providers only put the profile claims into the userinfo
	// response; merge it in (ID token claims win).
	if ui, err := o.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
		uc := map[string]any{}
		if err := ui.Claims(&uc); err == nil && (uc["sub"] == nil || uc["sub"] == idt.Subject) {
			for k, v := range uc {
				if _, exists := claims[k]; !exists {
					claims[k] = v
				}
			}
		}
	}

	id := &Identity{Subject: idt.Subject}
	id.Email, _ = claims["email"].(string)
	id.Name, _ = claims["name"].(string)
	if id.Name == "" {
		id.Name, _ = claims["preferred_username"].(string)
	}
	id.Groups = stringList(claims[o.cfg.GroupsClaim])

	if !o.allowed(id, claims) {
		return id, ErrNotAllowed
	}
	return id, nil
}

func (o *OIDC) allowed(id *Identity, claims map[string]any) bool {
	if o.cfg.AllowAnyUser {
		return true
	}
	if id.Email != "" && len(o.cfg.AllowedEmails) > 0 {
		// Only trust the e-mail if the provider does not say it is unverified.
		if v, ok := claims["email_verified"].(bool); !ok || v {
			if slices.Contains(o.cfg.AllowedEmails, strings.ToLower(id.Email)) {
				return true
			}
		}
	}
	for _, g := range id.Groups {
		if slices.Contains(o.cfg.AllowedGroups, g) {
			return true
		}
	}
	return false
}

func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		return strings.Fields(strings.ReplaceAll(t, ",", " "))
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
