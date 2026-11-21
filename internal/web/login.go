package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/jdelic/greylistd-frontend/internal/auth"
)

const flowCookie = "glf_oidc"

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.opt.Anonymous || s.session(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.loginConfigured() {
		s.renderLogin(w, r, http.StatusServiceUnavailable, "No login method is configured. "+
			"Configure OpenID Connect or a username and password, or allow access for everybody with --allow-anonymous.")
		return
	}
	loggedOut := r.URL.Query().Has("logged-out")
	// With OIDC as the only login method there is nothing to choose, so go
	// straight to the identity provider. Not right after logging out, though:
	// the provider's own session would log the user right back in.
	if s.opt.OIDC != nil && !s.passwordEnabled() && !loggedOut {
		http.Redirect(w, r, "/auth/oidc/start", http.StatusSeeOther)
		return
	}
	s.renderLoginPage(w, r, http.StatusOK, "", loggedOut)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.renderLoginPage(w, r, status, msg, false)
}

func (s *Server) renderLoginPage(w http.ResponseWriter, r *http.Request, status int, msg string, loggedOut bool) {
	data := pageData{
		Title:         "Login",
		Error:         msg,
		OIDCEnabled:   s.opt.OIDC != nil,
		OIDCName:      s.opt.OIDCName,
		PasswordLogin: s.passwordEnabled(),
	}
	if loggedOut {
		data.Message = "You have been logged out."
	}
	if !data.PasswordLogin {
		s.render(w, r, status, "login", data)
		return
	}
	// The password form is posted before a session exists, so it carries a
	// login-CSRF token bound to a cookie (double submit).
	token := auth.RandomToken(18)
	http.SetCookie(w, s.opt.Sessions.Cookie(loginCSRFCookie, token, 15*time.Minute))
	data.LoginCSRF = token
	s.render(w, r, status, "login", data)
}

const loginCSRFCookie = "glf_login_csrf"

func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	if !s.passwordEnabled() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	c, err := r.Cookie(loginCSRFCookie)
	if err != nil || c.Value == "" || c.Value != r.PostForm.Get("csrf") {
		s.renderLogin(w, r, http.StatusForbidden, "Your login form expired. Please try again.")
		return
	}
	ip := s.clientIP(r)
	if s.limiter.blocked(ip) {
		s.opt.Log.Warn("password login rate limited", "client", ip)
		s.renderLogin(w, r, http.StatusTooManyRequests, "Too many failed attempts. Try again in a few minutes.")
		return
	}
	user := r.PostForm.Get("username")
	if !s.opt.Password.Check(user, r.PostForm.Get("password")) {
		s.limiter.fail(ip)
		s.opt.Log.Warn("password login failed", "client", ip, "username", user)
		time.Sleep(500 * time.Millisecond)
		s.renderLogin(w, r, http.StatusUnauthorized, "Invalid username or password.")
		return
	}
	s.limiter.reset(ip)
	http.SetCookie(w, s.opt.Sessions.Cookie(loginCSRFCookie, "", -1))
	if _, err := s.opt.Sessions.Start(w, "password:"+user, user, "password"); err != nil {
		s.opt.Log.Error("cannot start session", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.opt.Log.Info("login", "method", "password", "user", user, "client", ip)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if s.opt.OIDC == nil {
		http.Error(w, errNoOIDC.Error(), http.StatusNotFound)
		return
	}
	u, fs, err := s.opt.OIDC.Begin(r.Context())
	if err != nil {
		s.opt.Log.Error("oidc login unavailable", "error", err)
		s.renderLogin(w, r, http.StatusServiceUnavailable, "The identity provider is currently unreachable.")
		return
	}
	v, err := s.opt.Sessions.Signer.Sign("oidc-flow", fs)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, s.opt.Sessions.Cookie(flowCookie, v, 10*time.Minute))
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.opt.OIDC == nil {
		http.NotFound(w, r)
		return
	}
	http.SetCookie(w, s.opt.Sessions.Cookie(flowCookie, "", -1))
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.opt.Log.Warn("oidc provider returned error", "error", e, "description", q.Get("error_description"))
		s.renderLogin(w, r, http.StatusUnauthorized, "Login was cancelled or denied by the identity provider.")
		return
	}
	var fs auth.FlowState
	c, err := r.Cookie(flowCookie)
	if err != nil || s.opt.Sessions.Signer.Verify("oidc-flow", c.Value, &fs) != nil {
		s.renderLogin(w, r, http.StatusBadRequest, "Your login attempt expired. Please try again.")
		return
	}
	id, err := s.opt.OIDC.Finish(r.Context(), &fs, q.Get("state"), q.Get("code"))
	if errors.Is(err, auth.ErrNotAllowed) {
		s.opt.Log.Warn("oidc login denied", "subject", id.Subject, "email", id.Email, "client", s.clientIP(r))
		s.renderLogin(w, r, http.StatusForbidden, "Your account ("+displayName(id)+") is not allowed to use this application.")
		return
	}
	if err != nil {
		s.opt.Log.Error("oidc login failed", "error", err, "client", s.clientIP(r))
		s.renderLogin(w, r, http.StatusUnauthorized, "Login failed. Please try again.")
		return
	}
	if _, err := s.opt.Sessions.Start(w, "oidc:"+id.Subject, displayName(id), "oidc"); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.opt.Log.Info("login", "method", "oidc", "subject", id.Subject, "email", id.Email, "client", s.clientIP(r))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func displayName(id *auth.Identity) string {
	switch {
	case id == nil:
		return ""
	case id.Email != "":
		return id.Email
	case id.Name != "":
		return id.Name
	}
	return id.Subject
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	if sess.Method == methodAnonymous {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.opt.Sessions.End(w)
	s.opt.Log.Info("logout", "user", sess.Name)
	http.Redirect(w, r, "/login?logged-out", http.StatusSeeOther)
}
