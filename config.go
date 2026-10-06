package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
)

// fileConfig is the TOML config file (CONFIG_FILE, default
// /config/calmerge.toml). Every key is optional: anything the file leaves out
// falls back to its env var, then the built-in default. So the file can take
// over one setting at a time, and env-only deployments keep working.
type fileConfig struct {
	TZ                 string   `toml:"tz"`
	LookaheadDays      int      `toml:"lookahead_days"`
	LookbackDays       int      `toml:"lookback_days"`
	RefreshMinutes     int      `toml:"refresh_minutes"`
	HTTPTimeoutSeconds int      `toml:"http_timeout_seconds"`
	IncludeAttendees   bool     `toml:"include_attendees"`
	IncludeAgenda      bool     `toml:"include_agenda"`
	RequireAuth        bool     `toml:"require_auth"`
	SelfEmails         []string `toml:"self_emails"`
	SkipDeclined       bool     `toml:"skip_declined"`
	CorrectionsFile    string   `toml:"corrections_file"`
	LessonsFile        string   `toml:"lessons_file"`
	LessonsMaxAgeDays  int      `toml:"lessons_max_age_days"`
	Feeds              []Feed   `toml:"feeds"`
	Entities           []Entity `toml:"entities"`
}

// configPollInterval is how often the config file is checked for changes.
const configPollInterval = 5 * time.Second

// loadConfig builds the config from the TOML file at path (if it exists) with
// env vars as the fallback. LISTEN and AUTH_TOKEN are env-only: one needs a
// restart anyway and the other is a secret. prev, when set, is the config
// being replaced by a hot reload; its corrections and lessons stores carry
// over so nothing re-reads or races on those files.
func loadConfig(path string, prev *config) (config, error) {
	var c config
	var fc fileConfig
	var md toml.MetaData
	haveFile := false
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if md, err = toml.Decode(string(b), &fc); err != nil {
				return c, fmt.Errorf("config %s: %w", path, err)
			}
			haveFile = true
			if und := md.Undecoded(); len(und) > 0 {
				log.Printf("config %s: ignoring unknown keys %v (typo?)", path, und)
			}
		case errors.Is(err, fs.ErrNotExist):
		default:
			return c, fmt.Errorf("config %s: %w", path, err)
		}
	}
	inFile := func(key string) bool { return haveFile && md.IsDefined(key) }
	num := func(key string, fileVal int, env string, def int) int {
		if inFile(key) {
			return fileVal
		}
		return envInt(env, def)
	}
	flag := func(key string, fileVal bool, env string, def bool) bool {
		if inFile(key) {
			return fileVal
		}
		return envBool(env, def)
	}
	str := func(key, fileVal, env, def string) string {
		if inFile(key) {
			return fileVal
		}
		return envStr(env, def)
	}

	// Feeds.
	if inFile("feeds") {
		c.feeds = fc.Feeds
	} else {
		raw := os.Getenv("FEEDS")
		if strings.TrimSpace(raw) == "" {
			return c, fmt.Errorf("no feeds: add [[feeds]] to %s or set FEEDS (JSON array of {name,url})", path)
		}
		if err := json.Unmarshal([]byte(raw), &c.feeds); err != nil {
			return c, fmt.Errorf("FEEDS is not valid JSON: %w", err)
		}
	}
	if len(c.feeds) == 0 {
		return c, fmt.Errorf("no feeds configured")
	}
	for i := range c.feeds {
		f := &c.feeds[i]
		if strings.TrimSpace(f.Name) == "" || strings.TrimSpace(f.URL) == "" {
			return c, fmt.Errorf("feed #%d needs both name and url", i+1)
		}
		if f.Color == "" {
			f.Color = defaultPalette[i%len(defaultPalette)]
		}
		f.Color = normalizeColor(f.Color)
	}

	// Scalars.
	c.listen = envStr("LISTEN", ":8076")
	c.authToken = strings.TrimSpace(os.Getenv("AUTH_TOKEN"))
	tzName := str("tz", fc.TZ, "TZ_NAME", "America/New_York")
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return c, fmt.Errorf("bad time zone %q: %w", tzName, err)
	}
	c.loc = loc
	c.lookbackDays = num("lookback_days", fc.LookbackDays, "LOOKBACK_DAYS", 0)
	c.aheadDays = num("lookahead_days", fc.LookaheadDays, "LOOKAHEAD_DAYS", 30)
	c.cacheTTL = time.Duration(num("refresh_minutes", fc.RefreshMinutes, "CACHE_TTL_MIN", 15)) * time.Minute
	c.httpTimeout = time.Duration(num("http_timeout_seconds", fc.HTTPTimeoutSeconds, "HTTP_TIMEOUT_SEC", 20)) * time.Second
	c.includeAttendees = flag("include_attendees", fc.IncludeAttendees, "INCLUDE_ATTENDEES", true)
	c.includeAgenda = flag("include_agenda", fc.IncludeAgenda, "INCLUDE_AGENDA", true)
	c.requireAuth = flag("require_auth", fc.RequireAuth, "REQUIRE_AUTH", false)
	if inFile("self_emails") {
		c.selfEmails = normalizeEmails(fc.SelfEmails)
	} else {
		c.selfEmails = normalizeEmails(strings.Split(os.Getenv("SELF_EMAILS"), ","))
	}
	c.skipDeclined = flag("skip_declined", fc.SkipDeclined, "SKIP_DECLINED", false)
	if c.cacheTTL < time.Minute {
		return c, fmt.Errorf("refresh interval must be at least 1 minute")
	}
	if c.aheadDays < 0 || c.lookbackDays < 0 {
		return c, fmt.Errorf("lookahead/lookback days can't be negative")
	}

	// Entities, corrections, lessons.
	var ents []Entity
	if inFile("entities") {
		ents = fc.Entities
		if err := prepareEntities(ents); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
	} else if ents, err = parseEntities(os.Getenv("ENTITIES")); err != nil {
		return c, err
	}
	c.classifier = newClassifier(ents)
	if c.classifier == nil {
		return c, nil
	}

	var old *classifier
	if prev != nil {
		old = prev.classifier
	}
	fixesPath := str("corrections_file", fc.CorrectionsFile, "CORRECTIONS_FILE", "/data/corrections.json")
	if old != nil && old.fixes != nil && old.fixes.path == fixesPath {
		c.classifier.fixes = old.fixes
	} else if c.classifier.fixes, err = loadCorrections(fixesPath); err != nil {
		return c, err
	}
	lessonsPath := str("lessons_file", fc.LessonsFile, "LESSONS_FILE", "/data/lessons.json")
	maxAge := num("lessons_max_age_days", fc.LessonsMaxAgeDays, "LESSONS_MAX_AGE_DAYS", 365)
	if old != nil && old.memory != nil && old.memory.path == lessonsPath {
		c.classifier.memory = old.memory
		c.classifier.memory.mu.Lock()
		c.classifier.memory.maxAge = maxAge
		c.classifier.memory.mu.Unlock()
	} else if c.classifier.memory, err = loadLessons(lessonsPath, maxAge); err != nil {
		return c, err
	}
	return c, nil
}

// normalizeEmails trims, lowercases and drops a mailto: prefix from each
// address so they match attendee emails as written in any feed. Blanks are
// dropped; nil when nothing is left.
func normalizeEmails(in []string) []string {
	var out []string
	for _, e := range in {
		if e = strings.ToLower(stripMailto(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// fileSum fingerprints the config file's contents ("" when it's missing).
// Comparing contents rather than mtimes survives editors and mounts that
// don't bump the timestamp.
func fileSum(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return string(sum[:])
}

// watchConfig polls the config file every interval and swaps in a fresh
// config whenever its contents change, then kicks a refresh so the change
// shows right away. A broken edit is logged and ignored; the last good config
// keeps serving. loadedSum is the fileSum taken before the running config was
// loaded, so an edit that lands in between still gets picked up. Runs until
// ctx is done.
func watchConfig(ctx context.Context, path, loadedSum string, interval time.Duration, cur *atomic.Pointer[config], st *store) {
	last := loadedSum
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sum := fileSum(path)
		if sum == last {
			continue
		}
		last = sum
		next, err := loadConfig(path, cur.Load())
		if err != nil {
			log.Printf("config reload failed, still using the previous config: %v", err)
			continue
		}
		cur.Store(&next)
		log.Printf("config reloaded from %s: %d feeds, %d entities", path, len(next.feeds), len(next.classifier.Entities()))
		st.refreshSoon()
	}
}

// loadEnvFile sets KEY=VALUE lines from path into the environment, the way
// Docker reads an env file: no shell parsing, so JSON values like FEEDS
// keep their quotes. Blank lines and # comments are skipped, an optional
// "export " prefix is dropped, and one pair of matching outer quotes is
// stripped.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // FEEDS / ENTITIES lines get long
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if err := os.Setenv(strings.TrimSpace(key), val); err != nil {
			return fmt.Errorf("%s:%d: %w", path, n, err)
		}
	}
	return sc.Err()
}

// printConfig writes the effective config as a calmerge.toml, so an env-based
// deployment can move to the file in one step. AUTH_TOKEN and LISTEN stay
// env-only and are left out.
func printConfig(w io.Writer, c config) error {
	out := fileConfig{
		TZ:                 c.loc.String(),
		LookaheadDays:      c.aheadDays,
		LookbackDays:       c.lookbackDays,
		RefreshMinutes:     int(c.cacheTTL / time.Minute),
		HTTPTimeoutSeconds: int(c.httpTimeout / time.Second),
		IncludeAttendees:   c.includeAttendees,
		IncludeAgenda:      c.includeAgenda,
		RequireAuth:        c.requireAuth,
		SelfEmails:         c.selfEmails,
		SkipDeclined:       c.skipDeclined,
		Feeds:              c.feeds,
		// With tagging off there are no stores to read; print what would apply.
		CorrectionsFile:   envStr("CORRECTIONS_FILE", "/data/corrections.json"),
		LessonsFile:       envStr("LESSONS_FILE", "/data/lessons.json"),
		LessonsMaxAgeDays: envInt("LESSONS_MAX_AGE_DAYS", 365),
	}
	if cl := c.classifier; cl != nil {
		out.CorrectionsFile = cl.fixes.path
		out.LessonsFile = cl.memory.path
		out.LessonsMaxAgeDays = cl.memory.maxAge
		for _, e := range cl.entities {
			out.Entities = append(out.Entities, e.Entity)
		}
	}
	fmt.Fprintln(w, "# calmerge config, generated by `calmerge -print-config`.")
	fmt.Fprintln(w, "# Mount at /config/calmerge.toml. See calmerge.example.toml for what each key does.")
	fmt.Fprintln(w, "# AUTH_TOKEN and LISTEN stay env vars and aren't included.")
	fmt.Fprintln(w)
	enc := toml.NewEncoder(w)
	enc.Indent = ""
	return enc.Encode(out)
}
