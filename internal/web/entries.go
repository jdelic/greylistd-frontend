package web

import (
	"cmp"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jdelic/greylistd-frontend/internal/greylistd"
)

type pageData struct {
	Title     string
	Active    string
	User      string
	CSRF      string
	Version   string
	StaticTag string
	Flash     *flash
	Error     string

	// login page
	OIDCEnabled   bool
	OIDCName      string
	PasswordLogin bool
	LoginCSRF     string

	// entries page
	Entries []entryView
	Total   int
	Counts  map[greylistd.List]int
	Query   string
	Back    string
	List    string // "" = all
	Sort    string
	Dir     string
	Tabs    []tab
	Columns []column
	AddList string

	// stats page
	Stats string
}

type entryView struct {
	Data        string
	List        greylistd.List
	LastSeen    string
	LastSeenISO string
	Ago         string
	Count       int
}

type tab struct {
	Label  string
	Key    string
	Count  int
	URL    string
	Active bool
}

type column struct {
	Key    string
	Label  string
	URL    string
	Active bool
	Desc   bool
}

func listLabel(l greylistd.List) string {
	switch l {
	case greylistd.White:
		return "white"
	case greylistd.Grey:
		return "grey"
	case greylistd.Black:
		return "black"
	}
	return string(l)
}

var sortKeys = []struct{ key, label string }{
	{"data", "Domain / IP"},
	{"list", "List"},
	{"seen", "Last seen"},
	{"count", "Hits"},
}

func entriesURL(q, list, sort, dir string) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if list != "" {
		v.Set("list", list)
	}
	if sort != "" && sort != "seen" {
		v.Set("sort", sort)
	}
	if dir != "" && (dir != "desc" || (sort != "" && sort != "seen")) {
		v.Set("dir", dir)
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	listParam := r.URL.Query().Get("list")
	if _, err := greylistd.ParseList(listParam); err != nil {
		listParam = ""
	}
	sortKey := r.URL.Query().Get("sort")
	if !slices.ContainsFunc(sortKeys, func(k struct{ key, label string }) bool { return k.key == sortKey }) {
		sortKey = "seen"
	}
	dir := r.URL.Query().Get("dir")
	if dir != "asc" && dir != "desc" {
		if sortKey == "seen" || sortKey == "count" {
			dir = "desc"
		} else {
			dir = "asc"
		}
	}

	data := pageData{
		Title:   "Entries",
		Active:  "entries",
		Flash:   s.takeFlash(w, r),
		Query:   q,
		List:    listParam,
		Sort:    sortKey,
		Dir:     dir,
		Counts:  map[greylistd.List]int{},
		AddList: "white",
		Back:    entriesURL(q, listParam, sortKey, dir),
	}

	entries, err := s.opt.Greylistd.List(r.Context())
	if err != nil {
		s.opt.Log.Error("cannot list greylistd entries", "error", err)
		data.Error = "Cannot read the lists from greylistd: " + err.Error()
	}
	data.Total = len(entries)
	for _, e := range entries {
		data.Counts[e.List]++
	}

	needle := strings.ToLower(q)
	var shown []greylistd.Entry
	for _, e := range entries {
		if listParam != "" && string(e.List) != listParam {
			continue
		}
		if needle != "" && !strings.Contains(e.Data, needle) {
			continue
		}
		shown = append(shown, e)
	}
	sortEntries(shown, sortKey, dir == "desc")

	now := time.Now()
	for _, e := range shown {
		data.Entries = append(data.Entries, entryView{
			Data:        e.Data,
			List:        e.List,
			LastSeen:    e.LastSeen.In(s.opt.DisplayTZ).Format("2006-01-02 15:04"),
			LastSeenISO: e.LastSeen.UTC().Format(time.RFC3339),
			Ago:         ago(now.Sub(e.LastSeen)),
			Count:       e.Count,
		})
	}

	data.Tabs = append(data.Tabs, tab{Label: "All", Key: "", Count: data.Total, URL: entriesURL(q, "", sortKey, dir), Active: listParam == ""})
	for _, l := range greylistd.Lists {
		data.Tabs = append(data.Tabs, tab{
			Label: strings.ToUpper(string(l[:1])) + string(l[1:]), Key: string(l), Count: data.Counts[l],
			URL: entriesURL(q, string(l), sortKey, dir), Active: listParam == string(l),
		})
	}
	for _, k := range sortKeys {
		c := column{Key: k.key, Label: k.label, Active: k.key == sortKey}
		nextDir := "asc"
		if k.key == "seen" || k.key == "count" {
			nextDir = "desc"
		}
		if c.Active {
			c.Desc = dir == "desc"
			if dir == "asc" {
				nextDir = "desc"
			} else {
				nextDir = "asc"
			}
		}
		c.URL = entriesURL(q, listParam, k.key, nextDir)
		data.Columns = append(data.Columns, c)
	}

	s.render(w, r, http.StatusOK, "entries", data)
}

var listOrder = map[greylistd.List]int{greylistd.White: 0, greylistd.Grey: 1, greylistd.Black: 2}

// dataKey orders IP addresses numerically before domains, and domains by
// their labels from right to left, so that subdomains group with their
// parent domain ("example.com" < "mail.example.com" < "other.com").
func dataKey(a, b string) int {
	ipA, errA := netip.ParseAddr(firstWord(a))
	ipB, errB := netip.ParseAddr(firstWord(b))
	switch {
	case errA == nil && errB == nil:
		if c := ipA.Compare(ipB); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return cmp.Or(slices.Compare(reversedLabels(a), reversedLabels(b)), strings.Compare(a, b))
}

func firstWord(s string) string {
	w, _, _ := strings.Cut(s, " ")
	return w
}

func reversedLabels(s string) []string {
	l := strings.Split(firstWord(s), ".")
	slices.Reverse(l)
	return l
}

func sortEntries(entries []greylistd.Entry, key string, desc bool) {
	slices.SortStableFunc(entries, func(a, b greylistd.Entry) int {
		var c int
		switch key {
		case "data":
			c = dataKey(a.Data, b.Data)
		case "list":
			c = cmp.Compare(listOrder[a.List], listOrder[b.List])
		case "count":
			c = cmp.Compare(a.Count, b.Count)
		default:
			c = a.LastSeen.Compare(b.LastSeen)
		}
		if desc {
			c = -c
		}
		// Stable, readable tie-breaker.
		return cmp.Or(c, dataKey(a.Data, b.Data))
	})
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

// splitInput splits the add form's free text into individual items.
func splitInput(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	list, err := greylistd.ParseList(r.PostForm.Get("list"))
	if err != nil {
		s.redirectFlash(w, r, &flash{Error: "Please choose a list."})
		return
	}
	items := splitInput(r.PostForm.Get("data"))
	if len(items) == 0 {
		s.redirectFlash(w, r, &flash{Error: "Please enter at least one domain or IP address."})
		return
	}
	if len(items) > 500 {
		s.redirectFlash(w, r, &flash{Error: "Please add at most 500 entries at once."})
		return
	}
	var done, failed []string
	for _, it := range items {
		norm, err := greylistd.NormalizeData(it)
		if err == nil {
			err = s.opt.Greylistd.Add(r.Context(), list, norm)
		}
		if err != nil {
			s.opt.Log.Warn("add failed", "user", sess.Name, "list", list, "data", it, "error", err)
			failed = append(failed, fmt.Sprintf("%s (%v)", it, cleanErr(err)))
			continue
		}
		s.opt.Log.Info("entry added", "user", sess.Name, "list", list, "data", norm)
		done = append(done, norm)
	}
	s.redirectFlash(w, r, resultFlash("Added to "+string(list)+"list", done, failed))
}

// handleAction handles both the per-row buttons (name="row",
// value="<action>:<data>") and the bulk buttons (name="bulk",
// value="<action>") that apply to all checked "sel" boxes.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	var action string
	var items []string
	if row := r.PostForm.Get("row"); row != "" {
		a, d, ok := strings.Cut(row, ":")
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		action, items = a, []string{d}
	} else {
		action = r.PostForm.Get("bulk")
		items = r.PostForm["sel"]
	}
	if len(items) == 0 {
		s.redirectFlash(w, r, &flash{Error: "Nothing selected."})
		return
	}

	var verb string
	var apply func(string) error
	switch action {
	case "white", "grey", "black":
		list := greylistd.List(action)
		verb = "Moved to " + action + "list"
		apply = func(d string) error { return s.opt.Greylistd.Add(r.Context(), list, d) }
	case "delete":
		verb = "Deleted"
		apply = func(d string) error { return s.opt.Greylistd.Delete(r.Context(), d) }
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	var done, failed []string
	for _, it := range items {
		if err := apply(it); err != nil {
			s.opt.Log.Warn("action failed", "user", sess.Name, "action", action, "data", it, "error", err)
			failed = append(failed, fmt.Sprintf("%s (%v)", it, cleanErr(err)))
			continue
		}
		s.opt.Log.Info("entry changed", "user", sess.Name, "action", action, "data", it)
		done = append(done, it)
	}
	s.redirectFlash(w, r, resultFlash(verb, done, failed))
}

func cleanErr(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "greylistd: ")
	return msg
}

func resultFlash(verb string, done, failed []string) *flash {
	f := &flash{}
	if len(done) > 0 {
		f.Message = verb + ": " + summarize(done)
	}
	if len(failed) > 0 {
		f.Error = "Failed: " + summarize(failed)
	}
	return f
}

func summarize(items []string) string {
	const max = 5
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:max], ", "), len(items)-max)
}

// redirectFlash stores f and redirects back to the entries page, keeping the
// current filter/sort (passed along as the "back" form field).
func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, f *flash) {
	if v, err := s.opt.Sessions.Signer.Sign("flash", f); err == nil {
		http.SetCookie(w, s.opt.Sessions.Cookie(flashCookie, v, time.Minute))
	}
	http.Redirect(w, r, backURL(r.PostForm.Get("back")), http.StatusSeeOther) //nolint:gosec // G710: backURL only ever returns "/" plus a re-encoded query
}

// backURL turns the submitted "back" field into a same-origin redirect target:
// always the entries page, keeping only its known query parameters.
func backURL(back string) string {
	u, err := url.Parse(back)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Path != "/" {
		return "/"
	}
	q := u.Query()
	return entriesURL(q.Get("q"), q.Get("list"), q.Get("sort"), q.Get("dir"))
}

const flashCookie = "glf_flash"

type flash struct {
	Message string `json:"m,omitempty"`
	Error   string `json:"e,omitempty"`
}

func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, s.opt.Sessions.Cookie(flashCookie, "", -1))
	var f flash
	if s.opt.Sessions.Signer.Verify("flash", c.Value, &f) != nil {
		return nil
	}
	return &f
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	data := pageData{Title: "Statistics", Active: "stats"}
	st, err := s.opt.Greylistd.Stats(r.Context())
	if err != nil {
		data.Error = "Cannot read statistics from greylistd: " + err.Error()
	}
	data.Stats = strings.TrimRight(st, "\n")
	s.render(w, r, http.StatusOK, "stats", data)
}
