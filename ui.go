package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The review UI: server-rendered Go templates, htmx 4 (+ hx-live) for
// partial updates, Tailwind for styles. Everything it needs is embedded, so
// the binary stays self-contained. The CSS is built ahead of time
// (go generate) and committed; see AGENTS.md.
//
//go:generate tailwindcss -i ui/tailwind.css -o ui/static/app.css --minify

//go:embed ui/templates/*.html
var uiTemplateFS embed.FS

//go:embed ui/static
var uiStaticFS embed.FS

const (
	uiCookie    = "calmerge_ui"
	uiCookieAge = 30 * 24 * time.Hour
)

// uiServer holds what the review UI handlers share.
type uiServer struct {
	cur   func() *config
	st    *store
	tmpl  *template.Template
	now   func() time.Time // for tests; nil means time.Now
	limit loginLimiter
}

func newUIServer(cur func() *config, st *store) *uiServer {
	u := &uiServer{cur: cur, st: st}
	ver := assetVersion()
	// asset links carry a hash of the embedded files, so a deploy (or a CSS
	// rebuild in dev) is picked up at once despite the long cache lifetime.
	asset := func(name string) string { return "/ui/static/" + name + "?v=" + ver }
	u.tmpl = template.Must(template.New("ui").Funcs(uiFuncs).Funcs(template.FuncMap{"asset": asset}).ParseFS(uiTemplateFS, "ui/templates/*.html"))
	return u
}

// assetVersion fingerprints every embedded static file.
func assetVersion() string {
	h := sha256.New()
	_ = fs.WalkDir(uiStaticFS, "ui/static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := uiStaticFS.ReadFile(p)
		h.Write([]byte(p))
		h.Write(b)
		return err
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// routes mounts the UI under /ui (and sends / there).
func (u *uiServer) routes(mux *http.ServeMux) {
	static, _ := fs.Sub(uiStaticFS, "ui/static")
	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /ui/login", u.loginPage)
	mux.HandleFunc("POST /ui/login", u.login)
	mux.HandleFunc("POST /ui/logout", u.logout)
	mux.Handle("GET /ui/{$}", u.auth(http.RedirectHandler("/ui/agenda", http.StatusFound)))
	mux.Handle("GET /ui/agenda", u.auth(http.HandlerFunc(u.agendaPage)))
	mux.Handle("GET /ui/week", u.auth(http.HandlerFunc(u.weekPage)))
	mux.Handle("GET /ui/status", u.auth(http.HandlerFunc(u.status)))
	mux.Handle("POST /ui/refresh", u.auth(u.htmxOnly(http.HandlerFunc(u.refresh))))
	mux.Handle("POST /ui/correct", u.auth(u.htmxOnly(http.HandlerFunc(u.correct))))
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/agenda", http.StatusFound))
}

func (u *uiServer) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// cacheStatic lets browsers keep the embedded assets for a day.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		next.ServeHTTP(w, r)
	})
}

// ---- auth ----------------------------------------------------------------

// sessionValue derives the cookie from the token, so rotating AUTH_TOKEN
// signs everyone out and the cookie never holds the token itself.
func sessionValue(token string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("calmerge-ui-v1"))
	return hex.EncodeToString(m.Sum(nil))
}

// signedIn accepts the session cookie or, for scripts, the bearer token.
// Fails closed: with no AUTH_TOKEN set nobody gets in.
func (u *uiServer) signedIn(r *http.Request) bool {
	token := u.cur().authToken
	if token == "" {
		return false
	}
	if authOK(r, token) {
		return true
	}
	c, err := r.Cookie(uiCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(sessionValue(token))) == 1
}

// Sign-in guesses allowed per loginWindow: a handful from one client, and a
// ceiling across all of them. The ceiling is what holds when a caller hides
// behind many addresses, and it keeps the limiter's map small.
const (
	loginWindow       = time.Minute
	loginMaxPerClient = 5
	loginMaxTotal     = 30
)

// loginLimiter slows token guessing on the sign-in form. It counts wrong
// tokens in fixed windows; once a client (or everyone together) hits the
// limit, sign-in is refused until the window rolls over, right token or not.
// Session cookies and bearer tokens already in use keep working.
type loginLimiter struct {
	mu    sync.Mutex
	start time.Time
	fails map[string]int
	total int
}

// roll starts a fresh window when the current one has run out. Caller holds mu.
func (l *loginLimiter) roll(now time.Time) {
	if l.fails == nil || now.Sub(l.start) >= loginWindow {
		l.start, l.fails, l.total = now, map[string]int{}, 0
	}
}

// blocked reports whether key has to wait out the window.
func (l *loginLimiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	return l.fails[key] >= loginMaxPerClient || l.total >= loginMaxTotal
}

// failed records one wrong token from key.
func (l *loginLimiter) failed(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	l.fails[key]++
	l.total++
}

// clientKey names who is signing in, for the limiter. Through the tunnel
// every connection comes from cloudflared, so the visitor's address is the
// one Cloudflare stamps on the request; otherwise it's the peer address.
func clientKey(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func (u *uiServer) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.signedIn(r) {
			next.ServeHTTP(w, r)
			return
		}
		if isHTMX(r) {
			w.Header().Set("HX-Redirect", "/ui/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/ui/login", http.StatusFound)
	})
}

// htmxOnly guards writes against cross-site forms: a page on another site
// can't set the HX-Request header without a CORS preflight we never allow.
func (u *uiServer) htmxOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isHTMX(r) {
			http.Error(w, "writes must come from the calmerge page", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || r.Header.Get("Cf-Visitor") != ""
}

func (u *uiServer) loginPage(w http.ResponseWriter, r *http.Request) {
	if u.signedIn(r) {
		http.Redirect(w, r, "/ui/agenda", http.StatusFound)
		return
	}
	u.render(w, "login", map[string]any{"NoToken": u.cur().authToken == ""})
}

func (u *uiServer) login(w http.ResponseWriter, r *http.Request) {
	token := u.cur().authToken
	key, now := clientKey(r), u.clock()
	if token != "" && u.limit.blocked(key, now) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		u.render(w, "login", map[string]any{"Throttled": true})
		return
	}
	got := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		if token != "" {
			u.limit.failed(key, now)
		}
		w.WriteHeader(http.StatusUnauthorized)
		u.render(w, "login", map[string]any{"Failed": true, "NoToken": token == ""})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: uiCookie, Value: sessionValue(token), Path: "/ui",
		MaxAge: int(uiCookieAge / time.Second), HttpOnly: true,
		Secure: secureRequest(r), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/ui/agenda", http.StatusSeeOther)
}

func (u *uiServer) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: "", Path: "/ui", MaxAge: -1, HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// ---- pages ---------------------------------------------------------------

func (u *uiServer) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := u.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("ui: render %s: %v", name, err)
	}
}

// page renders a whole page, or just the swappable view for htmx requests
// (pager, filters, selecting a meeting).
func (u *uiServer) page(w http.ResponseWriter, r *http.Request, v pageView) {
	if isHTMX(r) {
		u.render(w, "view", v)
		return
	}
	u.render(w, "page", v)
}

func (u *uiServer) agendaPage(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, u.buildPage(parseQuery("agenda", r.URL.Query()), ""))
}

func (u *uiServer) weekPage(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, u.buildPage(parseQuery("week", r.URL.Query()), ""))
}

// status is the header pill: polled while calmerge warms up or refreshes, and
// every 30s otherwise. When the data behind the page changed since the page
// last saw it, it also re-renders the view in place.
func (u *uiServer) status(w http.ResponseWriter, r *http.Request) {
	sv := u.statusView()
	seen := r.URL.Query().Get("seen")
	var view *pageView
	if seen != "" && sv.Stamp != "" && seen != sv.Stamp {
		if back, ok := localView(r.URL.Query().Get("view")); ok {
			kind := "agenda"
			if strings.HasSuffix(back.Path, "/week") {
				kind = "week"
			}
			pv := u.buildPage(parseQuery(kind, back.Query()), "")
			view = &pv
		}
	}
	u.render(w, "status-response", map[string]any{"Status": sv, "View": view})
}

func (u *uiServer) refresh(w http.ResponseWriter, r *http.Request) {
	u.st.refreshSoon()
	sv := u.statusView()
	sv.Syncing, sv.PollEvery = true, "1s" // poll fast until the refresh lands
	u.render(w, "status-response", map[string]any{"Status": sv})
}

// correct pins (or unpins) a series from the page, then answers with the
// re-rendered view and a toast. The change is re-tagged in memory, so the
// response already shows it.
func (u *uiServer) correct(w http.ResponseWriter, r *http.Request) {
	cfg := u.cur()
	id, entity := r.PostFormValue("seriesId"), r.PostFormValue("entity")
	res, err := applyCorrection(cfg.classifier, u.st, cfg.loc, id, entity)
	back, ok := localView(r.PostFormValue("view"))
	kind := "agenda"
	var q url.Values
	if ok {
		q = back.Query()
		if strings.HasSuffix(back.Path, "/week") {
			kind = "week"
		}
	}
	pq := parseQuery(kind, q)
	if err != nil {
		v := u.buildPage(pq, "")
		v.Toast = &toastView{Error: true, Title: "Couldn't save that", Body: err.Error()}
		u.render(w, "correct-response", v)
		return
	}
	v := u.buildPage(pq, res.SeriesID)
	t := &toastView{SeriesID: res.SeriesID, View: v.Self}
	undo := ""
	if res.PreviousVia == viaManual {
		undo = res.Previous
	}
	t.UndoVals = hxVals(map[string]string{"seriesId": res.SeriesID, "entity": orNone(undo), "view": v.Self})
	if res.Entity == "" {
		t.Title = res.Name + " is back to automatic"
		t.Body = "Rules and the model decide its client again."
	} else {
		t.Title = res.Name + " → " + res.Entity
		t.Body = "Every “" + res.Name + "” follows this now, and the model learned from it."
	}
	v.Toast = t
	u.render(w, "correct-response", v)
}

// localView accepts only a same-site path under /ui/ as "the page to
// re-render", never a full URL.
func localView(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(u.Path, "/ui/") {
		return nil, false
	}
	return u, true
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// hxVals encodes an hx-vals JSON object; html/template escapes it for the
// attribute, so names with quotes (Acme "Rocket" Co) survive.
func hxVals(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}
