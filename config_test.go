package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sampleTOML = `
tz = "America/Chicago"
lookahead_days = 14
refresh_minutes = 5
include_agenda = false
self_emails = ["Me@Example.com", " mailto:me@personal.example "]

[[feeds]]
name = "Work"
url = "https://example.com/work.ics"

[[feeds]]
name = "Family"
url = "https://example.com/family.ics"
color = "cf7fd1"

[[entities]]
name = "Globex Records"
parent = "Northwind Holdings"
color = "dc4747"
keywords = ["Globex", "Initech"]

[[entities]]
name = "Personal"
feeds = ["Family"]
`

// isolateEnv clears every env var loadConfig reads and points the data files
// at a temp dir.
func isolateEnv(t *testing.T) string {
	t.Helper()
	for _, k := range []string{"FEEDS", "ENTITIES", "TZ_NAME", "LOOKAHEAD_DAYS", "LOOKBACK_DAYS",
		"CACHE_TTL_MIN", "HTTP_TIMEOUT_SEC", "INCLUDE_ATTENDEES", "INCLUDE_AGENDA", "AUTH_TOKEN", "LISTEN",
		"LESSONS_MAX_AGE_DAYS", "CONFIG_FILE", "REQUIRE_AUTH", "SELF_EMAILS", "SKIP_DECLINED"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	t.Setenv("CORRECTIONS_FILE", filepath.Join(dir, "corrections.json"))
	t.Setenv("LESSONS_FILE", filepath.Join(dir, "lessons.json"))
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigFromTOML(t *testing.T) {
	dir := isolateEnv(t)
	t.Setenv("LOOKAHEAD_DAYS", "60") // the file's value wins
	t.Setenv("LOOKBACK_DAYS", "2")   // the file doesn't set it, so env fills in
	path := filepath.Join(dir, "calmerge.toml")
	writeFile(t, path, sampleTOML)

	c, err := loadConfig(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.loc.String() != "America/Chicago" || c.aheadDays != 14 || c.lookbackDays != 2 {
		t.Errorf("tz=%s ahead=%d back=%d", c.loc, c.aheadDays, c.lookbackDays)
	}
	if c.cacheTTL != 5*time.Minute || c.includeAgenda || !c.includeAttendees {
		t.Errorf("ttl=%s agenda=%v attendees=%v", c.cacheTTL, c.includeAgenda, c.includeAttendees)
	}
	if want := []string{"me@example.com", "me@personal.example"}; !reflect.DeepEqual(c.selfEmails, want) {
		t.Errorf("selfEmails = %q, want %q", c.selfEmails, want)
	}
	if len(c.feeds) != 2 || c.feeds[0].Color != "#5aa2f0" || c.feeds[1].Color != "#cf7fd1" {
		t.Errorf("feeds = %+v", c.feeds)
	}
	ents := c.classifier.Entities()
	if len(ents) != 2 || ents[0].Parent != "Northwind Holdings" || ents[0].Color != "#dc4747" {
		t.Errorf("entities = %+v", ents)
	}
}

func TestLoadConfigEnvOnly(t *testing.T) {
	dir := isolateEnv(t)
	t.Setenv("FEEDS", `[{"name":"Work","url":"https://example.com/p.ics"}]`)
	c, err := loadConfig(filepath.Join(dir, "missing.toml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.feeds) != 1 || c.aheadDays != 30 || c.classifier != nil {
		t.Errorf("feeds=%d ahead=%d classifier=%v", len(c.feeds), c.aheadDays, c.classifier)
	}
}

// TestDefaultFeedColors: feeds without a color take the palette in order and
// wrap around; a feed's own color is left alone.
func TestDefaultFeedColors(t *testing.T) {
	dir := isolateEnv(t)
	feeds := make([]Feed, len(defaultPalette)+1)
	for i := range feeds {
		feeds[i] = Feed{Name: fmt.Sprintf("cal%d", i), URL: "https://example.com/c.ics"}
	}
	feeds[1].Color = "112233"
	raw, _ := json.Marshal(feeds)
	t.Setenv("FEEDS", string(raw))
	c, err := loadConfig(filepath.Join(dir, "missing.toml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{0: defaultPalette[0], 1: "#112233", 2: defaultPalette[2], len(defaultPalette): defaultPalette[0]}
	for i, col := range want {
		if c.feeds[i].Color != col {
			t.Errorf("feed %d color = %q, want %q", i, c.feeds[i].Color, col)
		}
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct{ name, toml, want string }{
		{"bad toml", "tz = ", "config"},
		{"no feeds", `tz = "UTC"`, "no feeds"},
		{"feed without url", "[[feeds]]\nname = \"x\"", "name and url"},
		{"bad tz", "tz = \"Mars/Base\"\n[[feeds]]\nname=\"a\"\nurl=\"u\"", "time zone"},
		{"duplicate entity", "[[feeds]]\nname=\"a\"\nurl=\"u\"\n[[entities]]\nname=\"Hooli\"\n[[entities]]\nname=\"hooli\"", "twice"},
		{"nameless entity", "[[feeds]]\nname=\"a\"\nurl=\"u\"\n[[entities]]\nkeywords=[\"x\"]", "no name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := isolateEnv(t)
			path := filepath.Join(dir, "calmerge.toml")
			writeFile(t, path, tt.toml)
			_, err := loadConfig(path, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestWatchConfigHotReload: an edit lands without a restart, a broken edit is
// ignored, and corrections/lessons stores survive the swap.
func TestWatchConfigHotReload(t *testing.T) {
	dir := isolateEnv(t)
	path := filepath.Join(dir, "calmerge.toml")
	writeFile(t, path, sampleTOML)
	sum := fileSum(path)
	c, err := loadConfig(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixes := c.classifier.fixes
	var cur atomic.Pointer[config]
	cur.Store(&c)
	st := &store{kick: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchConfig(ctx, path, sum, 20*time.Millisecond, &cur, st); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	writeFile(t, path, strings.Replace(sampleTOML, "lookahead_days = 14", "lookahead_days = 21", 1)+
		"\n[[entities]]\nname = \"Hooli\"\nkeywords = [\"Hooli\"]\n")
	waitFor("reload", func() bool { return cur.Load().aheadDays == 21 })
	if n := len(cur.Load().classifier.Entities()); n != 3 {
		t.Errorf("entities after reload = %d, want 3", n)
	}
	if cur.Load().classifier.fixes != fixes {
		t.Error("corrections store was replaced instead of carried over")
	}
	select {
	case <-st.kick:
	case <-time.After(time.Second):
		t.Error("reload didn't kick a refresh")
	}

	writeFile(t, path, "this is = not [valid toml")
	time.Sleep(100 * time.Millisecond)
	if cur.Load().aheadDays != 21 {
		t.Error("a broken edit replaced the working config")
	}
}

// TestPrintConfigRoundTrip: env settings printed as TOML load back to the same
// config, which is what makes -print-config a safe env-to-file migration.
func TestPrintConfigRoundTrip(t *testing.T) {
	dir := isolateEnv(t)
	envPath := filepath.Join(dir, "calmerge.env")
	// Written the way Dokploy shows env: raw JSON, no shell quoting.
	writeFile(t, envPath, strings.Join([]string{
		"# copied from Dokploy",
		`FEEDS=[{"name":"Work","url":"https://example.com/p.ics?a=1&b=2"},{"name":"Family","url":"https://example.com/f.ics","color":"cf7fd1","user":"me","pass":"pw"}]`,
		`ENTITIES=[{"name":"Acme \"Rocket\" Co","parent":"Northwind Holdings","keywords":["ARC","Acme Rocket Co"],"domains":["acmerocket.example"]},{"name":"Personal","feeds":["Family"]}]`,
		"LOOKBACK_DAYS=1",
		"SELF_EMAILS=me@example.com, Me@Personal.example",
		"SKIP_DECLINED=true",
		"INCLUDE_AGENDA='false'",
		"export LESSONS_MAX_AGE_DAYS=0",
		"AUTH_TOKEN=supersecret",
		"",
	}, "\n"))
	if err := loadEnvFile(envPath); err != nil {
		t.Fatal(err)
	}
	fromEnv, err := loadConfig(filepath.Join(dir, "none.toml"), nil)
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := printConfig(&b, fromEnv); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "supersecret") {
		t.Fatal("AUTH_TOKEN leaked into the printed config")
	}

	// Load the printed file with the env cleared: the file alone must carry it.
	isolateEnv(t)
	tomlPath := filepath.Join(dir, "calmerge.toml")
	writeFile(t, tomlPath, b.String())
	fromFile, err := loadConfig(tomlPath, nil)
	if err != nil {
		t.Fatalf("printed config doesn't load: %v\n%s", err, b.String())
	}
	if !reflect.DeepEqual(fromFile.feeds, fromEnv.feeds) {
		t.Errorf("feeds differ:\n env  %+v\n file %+v", fromEnv.feeds, fromFile.feeds)
	}
	if !reflect.DeepEqual(fromFile.classifier.Entities(), fromEnv.classifier.Entities()) {
		t.Errorf("entities differ")
	}
	if want := []string{"me@example.com", "me@personal.example"}; !reflect.DeepEqual(fromFile.selfEmails, want) || !fromFile.skipDeclined {
		t.Errorf("selfEmails=%q skipDeclined=%v", fromFile.selfEmails, fromFile.skipDeclined)
	}
	if fromFile.lookbackDays != 1 || fromFile.includeAgenda || fromFile.classifier.memory.maxAge != 0 {
		t.Errorf("lookback=%d agenda=%v maxAge=%d", fromFile.lookbackDays, fromFile.includeAgenda, fromFile.classifier.memory.maxAge)
	}
	if fromFile.classifier.entities[0].Keywords[1] != "Acme Rocket Co" || fromFile.classifier.entities[0].Name != `Acme "Rocket" Co` {
		t.Errorf("entity = %+v", fromFile.classifier.entities[0].Entity)
	}
}
