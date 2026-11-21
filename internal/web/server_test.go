package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdelic/greylistd-frontend/internal/auth"
	"github.com/jdelic/greylistd-frontend/internal/greylistd"
)

// memGreylistd is an in-memory stand-in for greylistd.
type memGreylistd struct {
	mu      sync.Mutex
	entries map[string]greylistd.Entry
}

func (m *memGreylistd) List(context.Context, ...greylistd.List) ([]greylistd.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []greylistd.Entry
	for _, e := range m.entries {
		out = append(out, e)
	}
	return out, nil
}

func (m *memGreylistd) Add(_ context.Context, l greylistd.List, d string) error {
	d, err := greylistd.NormalizeData(d)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[d]
	e.Data, e.List, e.LastSeen = d, l, time.Now()
	e.Count++
	m.entries[d] = e
	return nil
}

func (m *memGreylistd) Delete(_ context.Context, d string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[d]; !ok {
		return greylistd.ErrNotFound
	}
	delete(m.entries, d)
	return nil
}

func (m *memGreylistd) Stats(context.Context) (string, error) { return "stats", nil }
func (m *memGreylistd) Ping(context.Context) error            { return nil }

func (m *memGreylistd) list(d string) greylistd.List {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[d].List
}

func newTestServer(t *testing.T) (*httptest.Server, *memGreylistd) {
	t.Helper()
	return newTestServerWith(t, func(*Options) {})
}

// newTestServerWith starts a server with password login, after letting
// configure change the options.
func newTestServerWith(t *testing.T, configure func(*Options)) (*httptest.Server, *memGreylistd) {
	t.Helper()
	gl := &memGreylistd{entries: map[string]greylistd.Entry{}}
	_ = gl.Add(context.Background(), greylistd.Grey, "grey.example")
	_ = gl.Add(context.Background(), greylistd.Black, "bad.example")
	signer, err := auth.NewSigner(nil)
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{
		Greylistd: gl,
		Sessions:  &auth.Sessions{Signer: signer, TTL: time.Hour},
		Password:  auth.NewPasswordChecker("admin", "secret"),
		Log:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Version:   "test",
	}
	configure(&opt)
	s, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, gl
}

func newClient(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func post(t *testing.T, c *http.Client, u string, v url.Values) (int, string) {
	t.Helper()
	resp, err := c.PostForm(u, v)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func csrf(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no csrf token in page:\n%s", body)
	}
	return m[1]
}

func login(t *testing.T, ts *httptest.Server, c *http.Client) string {
	t.Helper()
	_, page := get(t, c, ts.URL+"/login")
	code, body := post(t, c, ts.URL+"/login", url.Values{
		"csrf": {csrf(t, page)}, "username": {"admin"}, "password": {"secret"},
	})
	if code != http.StatusOK || !strings.Contains(body, "entries-form") {
		t.Fatalf("login failed: %d\n%s", code, body)
	}
	return csrf(t, body)
}

func TestRequiresLogin(t *testing.T) {
	ts, _ := newTestServer(t)
	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, _ := post(t, c, ts.URL+"/entries/add", url.Values{"data": {"x.example"}, "list": {"white"}}); code != http.StatusUnauthorized {
		t.Fatalf("anonymous add: %d", code)
	}
}

func TestPasswordLoginRejectsWrongPassword(t *testing.T) {
	ts, _ := newTestServer(t)
	c := newClient(t)
	_, page := get(t, c, ts.URL+"/login")
	code, body := post(t, c, ts.URL+"/login", url.Values{"csrf": {csrf(t, page)}, "username": {"admin"}, "password": {"wrong"}})
	if code != http.StatusUnauthorized || !strings.Contains(body, "Invalid username or password") {
		t.Fatalf("got %d", code)
	}
	// Without the login CSRF cookie/token pair the form is refused.
	code, _ = post(t, newClient(t), ts.URL+"/login", url.Values{"csrf": {"forged"}, "username": {"admin"}, "password": {"secret"}})
	if code != http.StatusForbidden {
		t.Fatalf("login without csrf cookie: %d", code)
	}
}

func TestAddAndActions(t *testing.T) {
	ts, gl := newTestServer(t)
	c := newClient(t)
	token := login(t, ts, c)

	code, body := post(t, c, ts.URL+"/entries/add", url.Values{
		"csrf": {token}, "list": {"white"}, "data": {"One.example, two.example\nthree.example -bad"},
	})
	if code != http.StatusOK {
		t.Fatalf("add: %d", code)
	}
	if !strings.Contains(body, "Added to whitelist: one.example, two.example, three.example") ||
		!strings.Contains(body, "Failed: -bad") {
		t.Fatalf("missing flash messages:\n%s", body)
	}
	for _, d := range []string{"one.example", "two.example", "three.example"} {
		if gl.list(d) != greylistd.White {
			t.Errorf("%s not whitelisted", d)
		}
	}

	// Per-row action.
	post(t, c, ts.URL+"/entries/action", url.Values{"csrf": {token}, "row": {"white:grey.example"}})
	if gl.list("grey.example") != greylistd.White {
		t.Error("row whitelist failed")
	}
	// Bulk action.
	post(t, c, ts.URL+"/entries/action", url.Values{"csrf": {token}, "bulk": {"black"}, "sel": {"one.example", "two.example"}})
	if gl.list("one.example") != greylistd.Black || gl.list("two.example") != greylistd.Black {
		t.Error("bulk blacklist failed")
	}
	post(t, c, ts.URL+"/entries/action", url.Values{"csrf": {token}, "bulk": {"delete"}, "sel": {"one.example", "missing.example"}})
	if gl.list("one.example") != "" {
		t.Error("bulk delete failed")
	}

	// A wrong CSRF token changes nothing.
	code, _ = post(t, c, ts.URL+"/entries/action", url.Values{"csrf": {"nope"}, "row": {"delete:two.example"}})
	if code != http.StatusForbidden || gl.list("two.example") != greylistd.Black {
		t.Fatalf("csrf not enforced: %d", code)
	}
}

func TestFilterAndSort(t *testing.T) {
	ts, gl := newTestServer(t)
	for _, d := range []string{"mail.b.example", "a.example", "b.example", "10.0.0.2", "9.0.0.1"} {
		_ = gl.Add(context.Background(), greylistd.White, d)
	}
	c := newClient(t)
	login(t, ts, c)
	rowRe := regexp.MustCompile(`<tr data-filter="([^"]+)">`)
	rows := func(q string) []string {
		_, body := get(t, c, ts.URL+"/?"+q)
		var out []string
		for _, m := range rowRe.FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		return out
	}
	if got, want := rows("list=white&sort=data&dir=asc"), []string{"9.0.0.1", "10.0.0.2", "a.example", "b.example", "mail.b.example"}; !slices.Equal(got, want) {
		t.Errorf("sort by data: got %v, want %v", got, want)
	}
	if got, want := rows("q=b.ex&sort=data&dir=desc"), []string{"mail.b.example", "b.example"}; !slices.Equal(got, want) {
		t.Errorf("filter: got %v, want %v", got, want)
	}
	if got := rows("list=black"); !slices.Equal(got, []string{"bad.example"}) {
		t.Errorf("list filter: got %v", got)
	}
}

func TestBackRedirectStaysLocal(t *testing.T) {
	ts, _ := newTestServer(t)
	c := newClient(t)
	token := login(t, ts, c)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for back, want := range map[string]string{
		"/?list=white":         "/?list=white",
		"https://evil.example": "/",
		"//evil.example":       "/",
	} {
		resp, err := c.PostForm(ts.URL+"/entries/action", url.Values{"csrf": {token}, "row": {"delete:bad.example"}, "back": {back}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get("Location"); got != want {
			t.Errorf("back=%q: redirected to %q, want %q", back, got, want)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	l := newLoginLimiter(2, time.Minute)
	l.fail("a")
	if l.blocked("a") {
		t.Fatal("blocked too early")
	}
	l.fail("a")
	if !l.blocked("a") || l.blocked("b") {
		t.Fatal("limiter state wrong")
	}
	l.reset("a")
	if l.blocked("a") {
		t.Fatal("reset failed")
	}
}

func noRedirects(t *testing.T) *http.Client {
	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func location(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func TestOIDCOnlyRedirectsToProvider(t *testing.T) {
	ts, _ := newTestServerWith(t, func(o *Options) {
		o.Password = auth.NewPasswordChecker("", "")
		o.OIDC = auth.NewOIDC(auth.OIDCConfig{Issuer: "https://idp.invalid", AllowAnyUser: true})
	})
	c := noRedirects(t)
	if code, loc := location(t, c, ts.URL+"/login"); code != http.StatusSeeOther || loc != "/auth/oidc/start" {
		t.Fatalf("login page: %d %q", code, loc)
	}
	// After logging out the page is shown instead, or the IdP's session
	// would log the user straight back in.
	code, body := get(t, c, ts.URL+"/login?logged-out")
	if code != http.StatusOK || !strings.Contains(body, "logged out") || !strings.Contains(body, "/auth/oidc/start") {
		t.Fatalf("logged-out page: %d\n%s", code, body)
	}
	// With a password login as well, the user chooses.
	ts, _ = newTestServerWith(t, func(o *Options) {
		o.OIDC = auth.NewOIDC(auth.OIDCConfig{Issuer: "https://idp.invalid", AllowAnyUser: true})
	})
	if code, _ := location(t, c, ts.URL+"/login"); code != http.StatusOK {
		t.Fatalf("login page with two methods: %d", code)
	}
}

func TestNoLoginConfigured(t *testing.T) {
	ts, _ := newTestServerWith(t, func(o *Options) { o.Password = nil })
	c := noRedirects(t)
	if code, loc := location(t, c, ts.URL+"/"); code != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("entries: %d %q", code, loc)
	}
	code, body := get(t, c, ts.URL+"/login")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "No login method is configured") || strings.Contains(body, "<form") {
		t.Fatalf("login page: %d\n%s", code, body)
	}
	if code, _ := post(t, c, ts.URL+"/login", url.Values{"username": {""}, "password": {""}}); code != http.StatusNotFound {
		t.Fatalf("password login: %d", code)
	}
}

func TestAnonymousAccess(t *testing.T) {
	signer, _ := auth.NewSigner(nil)
	sessions := &auth.Sessions{Signer: signer, TTL: time.Hour}
	ts, gl := newTestServerWith(t, func(o *Options) {
		o.Password, o.Anonymous, o.Sessions = nil, true, sessions
	})
	c := newClient(t)
	code, body := get(t, c, ts.URL+"/")
	if code != http.StatusOK || !strings.Contains(body, "entries-form") || strings.Contains(body, `action="/logout"`) {
		t.Fatalf("entries: %d\n%s", code, body)
	}
	code, _ = post(t, c, ts.URL+"/entries/add", url.Values{"csrf": {csrf(t, body)}, "data": {"anon.example"}, "list": {"white"}})
	if code != http.StatusOK || gl.list("anon.example") != greylistd.White {
		t.Fatalf("add: %d %v", code, gl.list("anon.example"))
	}
	// Even anonymous changes need the CSRF token.
	if code, _ := post(t, c, ts.URL+"/entries/add", url.Values{"data": {"x.example"}, "list": {"white"}}); code != http.StatusForbidden {
		t.Fatalf("add without csrf: %d", code)
	}
	if code, loc := location(t, noRedirects(t), ts.URL+"/login"); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("login page: %d %q", code, loc)
	}

	// The anonymous session is worthless once a login is required.
	ts2, _ := newTestServerWith(t, func(o *Options) { o.Sessions = sessions })
	u, _ := url.Parse(ts.URL)
	u2, _ := url.Parse(ts2.URL)
	c.Jar.SetCookies(u2, c.Jar.Cookies(u))
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if code, loc := location(t, c, ts2.URL+"/"); code != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("anonymous session accepted with password login: %d %q", code, loc)
	}
}
