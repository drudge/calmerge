package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// uiFixture is a signed-in-able UI over a small, realistic day.
type uiFixture struct {
	u   *uiServer
	mux *http.ServeMux
	st  *store
	cfg config
}

func newUIFixture(t *testing.T) *uiFixture {
	t.Helper()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 29, 12, 30, 0, 0, ny) // Tue lunchtime
	cfg := config{loc: ny, aheadDays: 30, cacheTTL: 15 * time.Minute, authToken: "secret",
		feeds: []Feed{{Name: "Work", Color: "#5aa2f0"}, {Name: "Family", Color: "#cf7fd1"}}}
	cfg.classifier = newClassifier([]Entity{
		{Name: "Globex Records", Color: "#dc4747", Keywords: []string{"Globex"}},
		{Name: "Hooli", Color: "#fe952e", Domains: []string{"hooli.example"}, URL: "https://docs.example.com/hooli"},
		{Name: "Northwind Holdings", Color: "#4699ff", Feeds: []string{"Work"}},
		{Name: "Personal", Feeds: []string{"Family"}},
	})
	cfg.classifier.fixes, _ = loadCorrections("")
	cfg.classifier.memory, _ = loadLessons("", 365)

	ev := func(id, name, date, start, end string, att ...Attendee) Event {
		st, _ := time.ParseInLocation("2006-01-02 15:04", date+" "+start, ny)
		en, _ := time.ParseInLocation("2006-01-02 15:04", date+" "+end, ny)
		e := Event{
			Name: name, SeriesId: id, Date: date, Source: "Work",
			Start: st.Format(time.RFC3339), End: en.Format(time.RFC3339),
			Duration: humanizeDuration(en.Sub(st)), Past: !en.After(now), Attendees: att,
			MeetURL: "https://teams.microsoft.com/l/meetup-join/x", MeetKind: "Teams",
		}
		e.sig = buildSignals(e.Source, e.Name, "", "", nil, "", att, e.MeetURL)
		return e
	}
	sam := Attendee{Name: "Sam Carter", Email: "scarter@hooli.example", Role: "organizer"}
	evs := []Event{
		ev("s1", "Support Training Operations", "2026-09-29", "08:30", "10:30", sam),
		ev("s2", "Globex VIP: Billing", "2026-09-29", "09:30", "09:45"),
		ev("s3", "Offsite fun times", "2026-09-29", "14:00", "14:30"),
		ev("s4", "Weekly Touchbase", "2026-09-29", "16:00", "16:30", sam),
		ev("s5", "Focus time", "2026-09-30", "13:00", "15:00"),
		ev("s6", "Vendor sync", "2026-10-14", "11:00", "11:30"), // guesses weeks out
		ev("s6", "Vendor sync", "2026-10-21", "11:00", "11:30"),
	}
	cfg.classifier.classify(evs)
	st := &store{kick: make(chan struct{}, 1)}
	resp := Response{Events: evs, Days: groupDays(evs, now, ny)}
	st.set(resp, nil)

	f := &uiFixture{st: st, cfg: cfg}
	f.u = newUIServer(func() *config { return &f.cfg }, st)
	f.u.now = func() time.Time { return now }
	f.mux = http.NewServeMux()
	f.u.routes(f.mux)
	return f
}

// do runs a request; signed adds the session cookie, htmx the HX-Request header.
func (f *uiFixture) do(method, target string, form url.Values, signed, htmx bool) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if signed {
		r.AddCookie(&http.Cookie{Name: uiCookie, Value: sessionValue("secret")})
	}
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func TestUIAuth(t *testing.T) {
	f := newUIFixture(t)
	if w := f.do("GET", "/ui/agenda", nil, false, false); w.Code != http.StatusFound || w.Header().Get("Location") != "/ui/login" {
		t.Errorf("signed out = %d → %q, want redirect to login", w.Code, w.Header().Get("Location"))
	}
	if w := f.do("GET", "/ui/agenda", nil, false, true); w.Code != http.StatusUnauthorized || w.Header().Get("HX-Redirect") != "/ui/login" {
		t.Errorf("signed out htmx = %d %q, want 401 + HX-Redirect", w.Code, w.Header().Get("HX-Redirect"))
	}
	if w := f.do("POST", "/ui/login", url.Values{"token": {"nope"}}, false, false); w.Code != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", w.Code)
	}
	w := f.do("POST", "/ui/login", url.Values{"token": {"secret"}}, false, false)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login = %d", w.Code)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == uiCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.Value == "secret" {
		t.Fatalf("session cookie = %+v; want HttpOnly and not the raw token", cookie)
	}
	if w := f.do("GET", "/ui/agenda", nil, true, false); w.Code != http.StatusOK {
		t.Errorf("signed in = %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/ui/week", nil)
	r.Header.Set("Authorization", "Bearer secret")
	rw := httptest.NewRecorder()
	f.mux.ServeHTTP(rw, r)
	if rw.Code != http.StatusOK {
		t.Errorf("bearer = %d, want 200", rw.Code)
	}

	// No AUTH_TOKEN: nobody gets in, not even with an empty cookie.
	f.cfg.authToken = ""
	if w := f.do("POST", "/ui/login", url.Values{"token": {""}}, false, false); w.Code != http.StatusUnauthorized {
		t.Errorf("login with no token configured = %d, want 401", w.Code)
	}
	if w := f.do("GET", "/ui/agenda", nil, true, false); w.Code != http.StatusFound {
		t.Errorf("old cookie with token removed = %d, want redirect", w.Code)
	}
}

// TestUILoginThrottle: wrong tokens run out, after which even the right one
// waits for the next window. Each client is counted on its own.
func TestUILoginThrottle(t *testing.T) {
	f := newUIFixture(t)
	now := time.Date(2026, 9, 29, 12, 30, 0, 0, time.UTC)
	f.u.now = func() time.Time { return now }
	login := func(token, from string) int {
		r := httptest.NewRequest("POST", "/ui/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = from + ":4321"
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < loginMaxPerClient; i++ {
		if code := login("nope", "10.0.0.1"); code != http.StatusUnauthorized {
			t.Fatalf("wrong token #%d = %d, want 401", i+1, code)
		}
	}
	if code := login("secret", "10.0.0.1"); code != http.StatusTooManyRequests {
		t.Errorf("right token after %d misses = %d, want 429", loginMaxPerClient, code)
	}
	if code := login("secret", "10.0.0.2"); code != http.StatusSeeOther {
		t.Errorf("another client = %d, want 303", code)
	}
	if w := f.do("GET", "/ui/agenda", nil, true, false); w.Code != http.StatusOK {
		t.Errorf("signed-in session while throttled = %d, want 200", w.Code)
	}
	now = now.Add(loginWindow)
	if code := login("secret", "10.0.0.1"); code != http.StatusSeeOther {
		t.Errorf("right token in the next window = %d, want 303", code)
	}

	// Many addresses, one wrong guess each: the shared ceiling stops them.
	now = now.Add(loginWindow)
	for i := 0; i < loginMaxTotal; i++ {
		login("nope", fmt.Sprintf("10.1.0.%d", i))
	}
	if code := login("secret", "10.2.0.1"); code != http.StatusTooManyRequests {
		t.Errorf("login past the shared ceiling = %d, want 429", code)
	}
}

func TestUIWritesNeedHTMX(t *testing.T) {
	f := newUIFixture(t)
	w := f.do("POST", "/ui/correct", url.Values{"seriesId": {"s3"}, "entity": {"Hooli"}}, true, false)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site style form post = %d, want 403", w.Code)
	}
	if _, ok := f.cfg.classifier.fixes.get("s3"); ok {
		t.Error("forbidden write still saved a correction")
	}
}

func TestUIAgendaRenders(t *testing.T) {
	f := newUIFixture(t)
	w := f.do("GET", "/ui/agenda", nil, true, false)
	body := w.Body.String()
	for _, want := range []string{
		"Today · Tue Sep 29",          // today's heading
		"2 meetings · all finished",   // the morning folded away
		"Offsite fun times",           // afternoon still listed
		"2:30 – 4:00",                 // free time between Offsite and Touchbase
		"free 1h 30m",                 // …and its length
		"Join Teams",                  // meeting link chip
		`hx-post="/ui/correct"`,       // Confirm on the guesses
		"Northwind Holdings",          // feed default shown as a guess
		`href="/ui/static/app.css?v=`, // cache-busted assets
		`href="/ui/static/fonts.css?v=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agenda missing %q", want)
		}
	}
	if w := f.do("GET", "/ui/agenda?earlier=1", nil, true, true); !strings.Contains(w.Body.String(), "data-open") || strings.Contains(w.Body.String(), "<html") {
		t.Error("htmx request should get just the view, with today unfolded")
	}
}

// The Guesses filter is a queue of every upcoming guess, not just the ones on
// the page you happen to be looking at: one row per series, its next meeting.
// TestUIServesItsOwnFonts: the page asks nobody else for fonts, and every file
// fonts.css names is in the binary.
func TestUIServesItsOwnFonts(t *testing.T) {
	f := newUIFixture(t)
	if body := f.do("GET", "/ui/login", nil, false, false).Body.String(); strings.Contains(body, "googleapis") || strings.Contains(body, `href="http`) {
		t.Error("login page links an outside stylesheet")
	}
	css := f.do("GET", "/ui/static/fonts.css", nil, false, false).Body.String()
	files := regexp.MustCompile(`url\((fonts/[^)]+)\)`).FindAllStringSubmatch(css, -1)
	if len(files) != 12 {
		t.Fatalf("fonts.css names %d files, want 12", len(files))
	}
	for _, m := range files {
		w := f.do("GET", "/ui/static/"+m[1], nil, false, false)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "font/woff2" {
			t.Errorf("%s = %d %q, want 200 font/woff2", m[1], w.Code, w.Header().Get("Content-Type"))
		}
	}
}

// TestUIPanelLinksEntityDocs: the details panel links the entity's configured
// url, whatever tool it points at.
func TestUIPanelLinksEntityDocs(t *testing.T) {
	f := newUIFixture(t)
	body := f.do("GET", "/ui/agenda?sel=s4&on=2026-09-29", nil, true, false).Body.String()
	for _, want := range []string{`href="https://docs.example.com/hooli"`, "Hooli docs"} {
		if !strings.Contains(body, want) {
			t.Errorf("panel missing %q", want)
		}
	}
}

func TestUIGuessQueue(t *testing.T) {
	f := newUIFixture(t)
	body := f.do("GET", "/ui/week?filter=guesses&from=2026-11-02", nil, true, true).Body.String()
	for _, want := range []string{"Upcoming guesses", "Offsite fun times", "Focus time", "Vendor sync", "Wed Oct 14"} {
		if !strings.Contains(body, want) {
			t.Errorf("queue missing %q", want)
		}
	}
	for _, not := range []string{"Oct 21", "Support Training Operations", `aria-label="Next"`} {
		if strings.Contains(body, not) {
			t.Errorf("queue shows %q", not)
		}
	}
	if !strings.Contains(body, `href="/ui/agenda?filter=guesses"`) {
		t.Error("the Guesses chip should open the queue from anywhere")
	}
}

func TestUICorrectRoundTrip(t *testing.T) {
	f := newUIFixture(t)
	w := f.do("POST", "/ui/correct", url.Values{"seriesId": {"s3"}, "entity": {"Globex Records"}, "view": {"/ui/agenda?sel=s3&on=2026-09-29"}}, true, true)
	if w.Code != http.StatusOK {
		t.Fatalf("correct = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Offsite fun times → Globex Records", `hx-target="#toasts"`, "cm-settle", "Saved", "Undo"} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q", want)
		}
	}
	resp, _, _, _ := f.st.get()
	for _, e := range resp.Events {
		if e.SeriesId == "s3" && (e.Entity != "Globex Records" || e.EntityVia != viaManual) {
			t.Errorf("store not re-tagged: %q via %q", e.Entity, e.EntityVia)
		}
	}
	w = f.do("POST", "/ui/correct", url.Values{"seriesId": {"s3"}, "entity": {"Nope Co"}}, true, true)
	if !strings.Contains(w.Body.String(), "Couldn&#39;t save that") {
		t.Errorf("bad entity should come back as an error toast: %s", w.Body.String())
	}
}

func TestUIStatusRefreshesStaleView(t *testing.T) {
	f := newUIFixture(t)
	sv := f.u.statusView()
	w := f.do("GET", "/ui/status?seen="+sv.Stamp+"&view="+url.QueryEscape("/ui/week"), nil, true, true)
	if strings.Contains(w.Body.String(), `hx-target="#view-content"`) {
		t.Error("up-to-date page got a view refresh")
	}
	if !strings.Contains(w.Body.String(), `hx-target="#status-pill" hx-swap="outerMorph"`) {
		t.Error("status should morph the pill in place, not replace it")
	}
	f.st.reclassify(f.cfg.classifier, f.cfg.loc) // data changed (e.g. a correction via MCP)
	w = f.do("GET", "/ui/status?seen="+sv.Stamp+"&view="+url.QueryEscape("/ui/week"), nil, true, true)
	if !strings.Contains(w.Body.String(), `hx-target="#view-content"`) {
		t.Error("stale page didn't get a view refresh")
	}
	w = f.do("GET", "/ui/status?seen=0&view="+url.QueryEscape("https://evil.example/ui/week"), nil, true, true)
	if strings.Contains(w.Body.String(), `hx-target="#view-content"`) {
		t.Error("status followed a view URL that isn't ours")
	}
}

func TestBuildWeekLayout(t *testing.T) {
	f := newUIFixture(t)
	resp, _, _, _ := f.st.get()
	now := f.u.clock().In(f.cfg.loc)
	monday := time.Date(2026, 9, 28, 0, 0, 0, 0, f.cfg.loc)
	keep := func(Event) bool { return true }
	mk := func(e Event) eventRow { return eventRow{Title: e.Name, Time: "t", Dur: "d"} }
	wv := buildWeek(resp.Events, monday, now, f.cfg.loc, keep, mk)
	if len(wv.Cols) != 5 {
		t.Fatalf("cols = %d, want Mon–Fri", len(wv.Cols))
	}
	tue := wv.Cols[1]
	if !tue.IsToday || !tue.HasNow {
		t.Error("Tuesday should be today with a now line")
	}
	if len(tue.Blocks) != 4 {
		t.Fatalf("Tuesday blocks = %d", len(tue.Blocks))
	}
	long, short := tue.Blocks[0], tue.Blocks[1]
	if string(long.Left) != "left: 0px; width: calc(100% - 0px)" || string(short.Left) != "left: 44px; width: calc(100% - 44px)" {
		t.Errorf("overlap indent: long %q, short %q", long.Left, short.Left)
	}
	if short.Height != weekMinBlockPx {
		t.Errorf("15-minute block height = %d, want the %dpx minimum", short.Height, weekMinBlockPx)
	}
	if long.Top != 30*weekHourPx/60 || !long.ShowTime {
		t.Errorf("8:30 block top = %d showTime=%v", long.Top, long.ShowTime)
	}
}

func TestTidyAgenda(t *testing.T) {
	in := "*\nCatch up\n  *\nPriorities\n     *\nSentinel, servers\nplain line"
	want := "• Catch up\n  • Priorities\n     • Sentinel, servers\nplain line"
	if got := tidyAgenda(in); got != want {
		t.Errorf("tidyAgenda =\n%q\nwant\n%q", got, want)
	}
}

func TestOOO(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	tests := []struct {
		name      string
		allDay    bool
		own       bool
		who, note string
	}{
		{"OOO", true, true, "", "Oct 7 – 9 · day 1 of 3 · back Mon Oct 12"},
		{"PTO - dentist", true, true, "", "Oct 7 – 9 · day 1 of 3 · back Mon Oct 12"},
		{"Jordan - Vacation Alert", true, false, "Jordan", "Oct 7 – 9 · day 1 of 3 · back Mon Oct 12"},
		{"Offsite planning", true, false, "", ""},
		{"OOO", false, false, "", ""}, // a timed "OOO" block is just a meeting
	}
	for _, tt := range tests {
		ev := Event{Name: tt.name, AllDay: tt.allDay, StartDate: "2026-10-07", EndDate: "2026-10-09", SpanDays: 3, DayIndex: 1, EntityVia: viaFeed}
		own, who := oooKind(ev)
		if own != tt.own || who != tt.who {
			t.Errorf("%q: own=%v who=%q, want %v %q", tt.name, own, who, tt.own, tt.who)
		}
		if tt.own || tt.who != "" {
			if got := oooNote(ev, ny); got != tt.note {
				t.Errorf("%q note = %q, want %q", tt.name, got, tt.note)
			}
			if isGuess(ev) {
				t.Errorf("%q: time off counted as a guess", tt.name)
			}
		}
	}
}

func TestUIRefreshPollsFast(t *testing.T) {
	f := newUIFixture(t)
	w := f.do("POST", "/ui/refresh", nil, true, true)
	body := w.Body.String()
	if !strings.Contains(body, `hx-trigger="every 1s"`) || !strings.Contains(body, "Refreshing") {
		t.Errorf("refresh should show Refreshing and poll every second: %s", body)
	}
	select {
	case <-f.st.kick:
	default:
		t.Error("refresh didn't kick the refresher")
	}
}
