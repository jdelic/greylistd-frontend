// Package web implements the HTTP server: login, the entries UI and health
// checks.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jdelic/greylistd-frontend/internal/auth"
	"github.com/jdelic/greylistd-frontend/internal/greylistd"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Greylistd is the subset of the greylistd client the server needs.
type Greylistd interface {
	List(ctx context.Context, lists ...greylistd.List) ([]greylistd.Entry, error)
	Add(ctx context.Context, list greylistd.List, data string) error
	Delete(ctx context.Context, data string) error
	Stats(ctx context.Context) (string, error)
	Ping(ctx context.Context) error
}

// Options configures a Server.
type Options struct {
	Greylistd Greylistd
	Sessions  *auth.Sessions
	OIDC      *auth.OIDC // nil if OIDC login is disabled
	OIDCName  string
	Password  *auth.PasswordChecker
	// Anonymous gives everybody access without logging in. Only valid when
	// neither OIDC nor password login is configured.
	Anonymous  bool
	Log        *slog.Logger // application log
	TrustProxy bool
	Version    string
	DisplayTZ  *time.Location
}

// Server is the HTTP handler of the application.
type Server struct {
	opt       Options
	tmpl      *template.Template
	mux       *http.ServeMux
	limiter   *loginLimiter
	static    http.Handler
	staticTag string
}

// New builds the server.
func New(opt Options) (*Server, error) {
	if opt.DisplayTZ == nil {
		opt.DisplayTZ = time.UTC
	}
	s := &Server{opt: opt, mux: http.NewServeMux(), limiter: newLoginLimiter(5, 5*time.Minute)}
	t, err := template.New("").Funcs(template.FuncMap{
		"listLabel": listLabel,
		"lower":     strings.ToLower,
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s.staticTag = opt.Version
	s.static = http.StripPrefix("/static/", cacheStatic(http.FileServerFS(sub)))

	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.Handle("GET /static/", s.static)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handlePasswordLogin)
	s.mux.HandleFunc("GET /auth/oidc/start", s.handleOIDCStart)
	s.mux.HandleFunc("GET /auth/oidc/callback", s.handleOIDCCallback)
	s.mux.HandleFunc("POST /logout", s.requireSession(s.handleLogout))

	s.mux.HandleFunc("GET /{$}", s.requireSession(s.handleEntries))
	s.mux.HandleFunc("POST /entries/add", s.requireSession(s.handleAdd))
	s.mux.HandleFunc("POST /entries/action", s.requireSession(s.handleAction))
	s.mux.HandleFunc("GET /stats", s.requireSession(s.handleStats))
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	s.mux.ServeHTTP(w, r)
}

type ctxKey int

const sessionKey ctxKey = 0

func sessionFrom(r *http.Request) *auth.Session {
	sess, _ := r.Context().Value(sessionKey).(*auth.Session)
	return sess
}

// Session methods besides "oidc" and "password".
const methodAnonymous = "anonymous"

func (s *Server) passwordEnabled() bool { return s.opt.Password != nil && s.opt.Password.Enabled() }

// loginConfigured reports whether any login method is configured.
func (s *Server) loginConfigured() bool { return s.opt.OIDC != nil || s.passwordEnabled() }

// session returns the request's session if it is valid and was created by a
// login method that is still enabled, so that e.g. an anonymous session does
// not survive switching from --allow-anonymous to a real login.
func (s *Server) session(r *http.Request) *auth.Session {
	sess := s.opt.Sessions.Get(r)
	if sess == nil {
		return nil
	}
	var ok bool
	switch sess.Method {
	case "oidc":
		ok = s.opt.OIDC != nil
	case "password":
		ok = s.passwordEnabled()
	case methodAnonymous:
		ok = s.opt.Anonymous
	}
	if !ok {
		return nil
	}
	return sess
}

// requireSession redirects users without a session to the login page and
// enforces the CSRF token on state-changing requests. In anonymous mode a
// session (which carries the CSRF token) is created on the first GET.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.session(r)
		if sess == nil && s.opt.Anonymous && r.Method == http.MethodGet {
			var err error
			if sess, err = s.opt.Sessions.Start(w, methodAnonymous, methodAnonymous, methodAnonymous); err != nil {
				s.opt.Log.Error("cannot start session", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if sess == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "session expired, please log in again", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if !sess.CheckCSRF(r.PostForm.Get("csrf")) {
				s.opt.Log.Warn("csrf token mismatch", "path", r.URL.Path, "client", s.clientIP(r), "user", sess.Name)
				http.Error(w, "invalid CSRF token, please reload the page", http.StatusForbidden)
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.opt.Greylistd.Ping(r.Context()); err != nil {
		s.opt.Log.Error("health check failed", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("greylistd unavailable\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// render executes a page template inside the layout.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data pageData) {
	data.Version = s.opt.Version
	data.StaticTag = s.staticTag
	if sess := sessionFrom(r); sess != nil {
		data.User = sess.Name
		data.CSRF = sess.CSRF
		data.Anonymous = sess.Method == methodAnonymous
	}
	var buf strings.Builder
	if err := s.tmpl.ExecuteTemplate(&buf, page, data); err != nil {
		s.opt.Log.Error("template error", "template", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

// clientIP returns the client address, honoring X-Forwarded-For only when the
// operator trusts the proxy in front of the server.
func (s *Server) clientIP(r *http.Request) string {
	return ClientIP(r, s.opt.TrustProxy)
}

// ClientIP extracts the client address of r.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// URLs carry ?v=<version>, so they can be cached for a long time.
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		h.ServeHTTP(w, r)
	})
}

// loginLimiter slows down password guessing: after max failures from one
// client within window, further attempts are refused until the window ends.
type loginLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *loginLimiter) prune(key string, now time.Time) []time.Time {
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = kept
	return kept
}

func (l *loginLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, time.Now())) >= l.max
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.fails[key] = append(l.prune(key, now), now)
	// Bound memory: drop stale clients opportunistically.
	if len(l.fails) > 10000 {
		for k := range l.fails {
			l.prune(k, now)
		}
	}
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

var errNoOIDC = errors.New("OpenID Connect login is not configured")
