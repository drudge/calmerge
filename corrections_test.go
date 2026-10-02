package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCorrectionsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrections.json")
	c, err := loadCorrections(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.get("abc"); ok {
		t.Fatal("fresh store should be empty")
	}
	if err := c.set("abc", "Globex Records", "Offsite fun times"); err != nil {
		t.Fatal(err)
	}

	again, err := loadCorrections(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := again.get("abc"); !ok || got != "Globex Records" {
		t.Errorf("reloaded get = %q, %v", got, ok)
	}

	if err := again.set("abc", "", ""); err != nil {
		t.Fatal(err)
	}
	third, _ := loadCorrections(path)
	if _, ok := third.get("abc"); ok {
		t.Error("cleared correction came back after reload")
	}
}

func TestCorrectionsCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrections.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCorrections(path); err == nil {
		t.Error("corrupt file should fail loudly, not be overwritten later")
	}
}

func TestCorrectionsSaveFailureRollsBack(t *testing.T) {
	c, err := loadCorrections(filepath.Join(t.TempDir(), "missing-dir", "corrections.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.set("abc", "Hooli", "x"); err == nil {
		t.Fatal("save into a missing directory should fail")
	}
	if _, ok := c.get("abc"); ok {
		t.Error("failed save left the correction in memory")
	}
}

func TestClassifyCorrectionWinsAndTeaches(t *testing.T) {
	fixes, _ := loadCorrections("")
	c := newClassifier([]Entity{
		{Name: "Globex Records", Keywords: []string{"Globex"}},
		{Name: "Hooli", Keywords: []string{"Hooli"}},
		{Name: "Northwind Holdings", Feeds: []string{"Work"}},
	})
	c.fixes = fixes
	anne := Attendee{Email: "anne@initech.example"}
	jacob := Attendee{Email: "jacob@globexrecords.example"}
	ev := func(name string, att ...Attendee) Event {
		return Event{Name: name, SeriesId: seriesID(name), sig: buildSignals("Work", name, "", "", nil, "", att, "")}
	}
	evs := []Event{
		ev("Hooli sync"),                         // rule says Hooli; the user says otherwise
		ev("Offsite fun times", anne, jacob),     // nothing automatic places it
		ev("Loyalty contract chat", anne, jacob), // same people, no keyword
		ev("Globex planning"),
		ev("Hooli weekly", Attendee{Email: "scarter@hooli.example"}), // a second class for the model
	}
	_ = fixes.set(evs[0].SeriesId, "Globex Records", evs[0].Name)
	_ = fixes.set(evs[1].SeriesId, "Globex Records", evs[1].Name)
	c.classify(evs)

	if evs[0].Entity != "Globex Records" || evs[0].EntityVia != viaManual {
		t.Errorf("corrected rule hit => %q via %q, want Globex Records via manual", evs[0].Entity, evs[0].EntityVia)
	}
	if evs[1].EntityVia != viaManual {
		t.Errorf("corrected event via %q, want manual", evs[1].EntityVia)
	}
	if evs[2].Entity != "Globex Records" || evs[2].EntityVia != viaLearned {
		t.Errorf("look-alike => %q via %q, want Globex Records via learned (taught by the correction)", evs[2].Entity, evs[2].EntityVia)
	}

	// A pin to an entity that's no longer configured is ignored.
	_ = fixes.set(evs[3].SeriesId, "Gone Co", evs[3].Name)
	c.classify(evs)
	if evs[3].Entity != "Globex Records" || evs[3].EntityVia != viaRule {
		t.Errorf("stale pin => %q via %q, want the rule result", evs[3].Entity, evs[3].EntityVia)
	}
}

// bearerTransport adds the calmerge token to every MCP request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPSetEventEntity(t *testing.T) {
	cfg := mcpTestConfig(t)
	cfg.classifier = newClassifier([]Entity{
		{Name: "Globex Records"},
		{Name: "Hooli"},
	})
	fixes, err := loadCorrections(filepath.Join(t.TempDir(), "corrections.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.classifier.fixes = fixes

	st := storeWithDays(cfg, 2)
	for i := range st.resp.Events {
		st.resp.Events[i].SeriesId = "offsite1"
		st.resp.Events[i].Name = "Offsite fun times"
		st.resp.Events[i].Entity = "Hooli"
		st.resp.Events[i].EntityVia = viaLearned
	}
	st.kick = make(chan struct{}, 1)

	srv := httptest.NewServer(newMCPHandler(fixedConfig(cfg), st))
	defer srv.Close()
	call := func(token string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		ctx := context.Background()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx,
			&mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: bearerTransport{token}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "set_event_entity", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	text := func(res *mcp.CallToolResult) string { return res.Content[0].(*mcp.TextContent).Text }

	if res := call("", map[string]any{"seriesId": "offsite1", "entity": "Globex Records"}); !res.IsError || !strings.Contains(text(res), "bearer") {
		t.Errorf("no token: %v %q, want bearer error", res.IsError, text(res))
	}
	if res := call("secret", map[string]any{"seriesId": "offsite1", "entity": "Nope"}); !res.IsError || !strings.Contains(text(res), "Globex Records, Hooli") {
		t.Errorf("unknown entity: %v %q, want list of valid names", res.IsError, text(res))
	}
	if res := call("secret", map[string]any{"seriesId": "missing", "entity": "Hooli"}); !res.IsError {
		t.Errorf("unknown seriesId should error: %q", text(res))
	}

	res := call("secret", map[string]any{"seriesId": "offsite1", "entity": "globex records"})
	if res.IsError {
		t.Fatalf("set failed: %q", text(res))
	}
	var out setEntityResult
	if err := json.Unmarshal([]byte(text(res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Entity != "Globex Records" || out.Previous != "Hooli" || out.PreviousVia != viaLearned || out.Name != "Offsite fun times" {
		t.Errorf("result = %+v", out)
	}
	if got, _ := fixes.get("offsite1"); got != "Globex Records" {
		t.Errorf("saved = %q, want canonical name", got)
	}
	// Visible at once: the cached events are re-tagged, no feed refresh.
	resp, _, _, _ := st.get()
	if ev := resp.Events[0]; ev.Entity != "Globex Records" || ev.EntityVia != viaManual {
		t.Errorf("cached event = %q via %q, want Globex Records via manual right away", ev.Entity, ev.EntityVia)
	}
	if day := resp.Days[0].Events[0]; day.Entity != "Globex Records" {
		t.Errorf("day group not rebuilt: %q", day.Entity)
	}

	if res := call("secret", map[string]any{"seriesId": "offsite1", "entity": "none"}); res.IsError {
		t.Fatalf("clear failed: %q", text(res))
	}
	if _, ok := fixes.get("offsite1"); ok {
		t.Error("none should remove the correction")
	}
}
