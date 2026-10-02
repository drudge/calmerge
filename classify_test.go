package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKeywordRe(t *testing.T) {
	tests := []struct {
		kw, text string
		want     bool
	}{
		{"ARC", "Invoice ARC", true},
		{"ARC", "ARC M365 Domain / Azure Transfer", true},
		{"ARC", "Support Training Operations", false},
		{"ARC", "search review", false},
		{"ARC", "arcade", false},
		{"RP", "RP - Leads touch point", true},
		{"Acme Rocket Co", "Acme  Rocket  Co planning", true},
		{"Globex Records", "globex records VIP", true},
		{"Hooli", "Hooli/ARC sync", true},
		{"Acme Rocket Co", `Acme "Rocket" Co`, true},
		{"Ricki's", "Ricki’s TechShip", true},
		{"Ricki's", "Rickis TechShip", false},
	}
	for _, tt := range tests {
		if got := strings.Contains(normText(tt.text), normText(tt.kw)); got != tt.want {
			t.Errorf("match(%q, %q) = %v, want %v", tt.kw, tt.text, got, tt.want)
		}
	}
}

func TestTeamsTenant(t *testing.T) {
	u := "https://teams.microsoft.com/l/meetup-join/19%3ameeting_abc%40thread.v2/0?context=%7b%22Tid%22%3a%22AAAA1111-bbbb-4ccc-8ddd-eeeeeeeeeeee%22%2c%22Oid%22%3a%2200000000-0000-4000-8000-000000000001%22%7d"
	if got, want := teamsTenant(u), "aaaa1111-bbbb-4ccc-8ddd-eeeeeeeeeeee"; got != want {
		t.Errorf("teamsTenant = %q, want %q", got, want)
	}
	if got := teamsTenant("https://meet.google.com/abc-defg-hij"); got != "" {
		t.Errorf("teamsTenant(meet) = %q, want empty", got)
	}
}

func TestParseEntities(t *testing.T) {
	ents, err := parseEntities(`[{"name":"Globex","color":"f5a623","keywords":["Globex"]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Color != "#f5a623" {
		t.Errorf("parseEntities = %+v", ents)
	}
	if ents, err := parseEntities(""); err != nil || ents != nil {
		t.Errorf("empty ENTITIES = %v, %v; want nil, nil", ents, err)
	}
	if _, err := parseEntities(`[{"keywords":["x"]}]`); err == nil {
		t.Error("nameless entity should error")
	}
	if newClassifier(nil) != nil {
		t.Error("no entities should disable the classifier")
	}
}

// teams builds a Teams join link for a tenant, the way Outlook feeds carry them.
func teams(tid string) string {
	return "https://teams.microsoft.com/l/meetup-join/19%3ameeting_x%40thread.v2/0?context=%7b%22Tid%22%3a%22" + tid + "%22%7d"
}

func TestClassify(t *testing.T) {
	const (
		tidSelf    = "11111111-1111-4111-8111-111111111111" // shows up across every entity
		tidInitech = "22222222-2222-4222-8222-222222222222"
		tidARC     = "aaaa1111-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	)
	c := newClassifier([]Entity{
		{Name: "Globex", Color: "#f5a623", Keywords: []string{"Globex", "Initech"}},
		{Name: "ARC", Keywords: []string{"ARC", "Acme Rocket Co"}, Domains: []string{"acmerocket.example"}},
		{Name: "Hooli", Keywords: []string{"Hooli"}, Domains: []string{"hooli.example"}},
		{Name: "RedPanda", Keywords: []string{"RedPanda", "RP"}, Categories: []string{"Red Panda"}},
		{Name: "Personal", Feeds: []string{"Family"}},
		{Name: "Northwind Holdings", Feeds: []string{"Work"}},
	})

	hooli := []Attendee{
		{Email: "scarter@hooli.example", Role: "organizer"},
		{Email: "pat.lee@acmerocket.example", Role: "required"},
		{Email: "lee.wong@vendor.example", Role: "required"},
		{Email: "me@northwind.example", Role: "required"},
	}
	ev := func(feed, name, agenda string, cats []string, att []Attendee, meet string) Event {
		return Event{
			Name:     name,
			SeriesId: seriesID(name),
			sig:      buildSignals(feed, name, agenda, "", cats, "", att, meet),
		}
	}
	evs := []Event{
		ev("Work", "Initech - Globex POS, Stand up", "", nil, nil, teams(tidSelf)),
		ev("Work", "Globex Records VIP: Billing discussion", "Regular weekly call to progress billing.", nil, nil, teams(tidInitech)),
		ev("Work", "Globex Records VIP: Status Discussion", "", nil, nil, teams(tidInitech)),
		ev("Work", "ARC M365 Domain / Azure Transfer", "Microsoft Azure", nil, nil, teams(tidARC)),
		ev("Freelance", "Invoice ARC", "", nil, nil, ""),
		ev("Work", "Support Training Operations", "", nil, hooli, "https://meet.google.com/abc-defg-hij"),
		ev("Work", "RP - Leads touch point", "Sentinel, servers and MS defender", nil, nil, teams(tidSelf)),
		ev("Work", "Quarterly review", "", []string{"Red Panda"}, nil, ""),
		ev("Family", "Riley's surprise party", "", nil, nil, ""),
		// No keyword, but it's on ARC's Teams tenant: the model should pick that up.
		ev("Work", "Winddown - Azure Resources Cleanup", "", nil, nil, teams(tidARC)),
		// Nothing to go on but the feed.
		ev("Work", "Focus time", "", nil, nil, ""),
	}
	c.classify(evs)

	want := map[string][2]string{
		"Initech - Globex POS, Stand up":         {"Globex", viaRule},
		"Globex Records VIP: Billing discussion": {"Globex", viaRule},
		"ARC M365 Domain / Azure Transfer":       {"ARC", viaRule},
		"Invoice ARC":                            {"ARC", viaRule},
		"Support Training Operations":            {"Hooli", viaRule}, // organizer's domain outweighs a ARC attendee
		"RP - Leads touch point":                 {"RedPanda", viaRule},
		"Quarterly review":                       {"RedPanda", viaCategory},
		"Riley's surprise party":                 {"Personal", viaFeed},
		"Winddown - Azure Resources Cleanup":     {"ARC", viaLearned}, // learned beats the Work feed default
		"Focus time":                             {"Northwind Holdings", viaFeed},
	}
	for _, e := range evs {
		w, ok := want[e.Name]
		if !ok {
			continue
		}
		if e.Entity != w[0] || e.EntityVia != w[1] {
			t.Errorf("%q => %q via %q, want %q via %q", e.Name, e.Entity, e.EntityVia, w[0], w[1])
		}
	}
	if evs[0].EntityColor != "#f5a623" {
		t.Errorf("entityColor = %q, want the Globex color", evs[0].EntityColor)
	}
}

func TestClassifyRuleTieStaysUnlabeled(t *testing.T) {
	c := newClassifier([]Entity{
		{Name: "ARC", Keywords: []string{"ARC"}},
		{Name: "Hooli", Keywords: []string{"Hooli"}},
	})
	evs := []Event{{Name: "Hooli / ARC sync", sig: signals{title: "Hooli / ARC sync"}}}
	c.classify(evs)
	if evs[0].Entity != "" {
		t.Errorf("tie labeled %q, want unlabeled", evs[0].Entity)
	}
}

func TestClassifyNilIsNoop(t *testing.T) {
	var c *classifier
	evs := []Event{{Name: "x"}}
	c.classify(evs) // must not panic
	if evs[0].Entity != "" {
		t.Error("nil classifier labeled an event")
	}
}

func TestNaiveBayesNeedsKnownTokens(t *testing.T) {
	nb := newNaiveBayes()
	nb.train(0, []string{"t:globex"})
	nb.train(1, []string{"t:arc"})
	nb.train(1, []string{"t:arc", "d:acmerocket.example"})
	if _, _, _, ok := nb.predict([]string{"t:lunch", "f:work"}); ok {
		t.Error("prediction with no known tokens should be refused")
	}
	if _, _, _, ok := nb.predict([]string{"t:arc", "t:lunch"}); ok {
		t.Error("prediction on a single known token should be refused")
	}
	c, p, clues, ok := nb.predict([]string{"t:arc", "d:acmerocket.example", "t:lunch"})
	if !ok || c != 1 || p < minLearnedConf {
		t.Errorf("predict = %d, %.2f, %v; want class 1 with p >= %.2f", c, p, ok, minLearnedConf)
	}
	if len(clues) == 0 || clues[0] != "attendee @acmerocket.example" && clues[0] != `"arc" in the title` {
		t.Errorf("clues = %q, want the ARC-only features first", clues)
	}
}

// TestClassifyExplains: every label says why in plain words, and so does an
// event nothing could place.
func TestClassifyExplains(t *testing.T) {
	const tidARC = "aaaa1111-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	c := newClassifier([]Entity{
		{Name: "Globex Records", Keywords: []string{"Globex"}},
		{Name: "ARC", Keywords: []string{"ARC"}},
		{Name: "Hooli", Domains: []string{"hooli.example"}},
		{Name: "Personal", Feeds: []string{"Family"}},
	})
	fixes, _ := loadCorrections("")
	c.fixes = fixes
	ev := func(feed, name string, att []Attendee, meet string) Event {
		return Event{Name: name, SeriesId: seriesID(name), sig: buildSignals(feed, name, "", "", nil, "", att, meet)}
	}
	evs := []Event{
		ev("Work", "Globex planning", nil, ""),
		ev("Work", "Weekly Touchbase", []Attendee{{Email: "scarter@hooli.example", Role: "organizer"}}, ""),
		ev("Work", "ARC M365 Azure Transfer", nil, teams(tidARC)),
		ev("Work", "Winddown Azure Cleanup", nil, teams(tidARC)),
		ev("Family", "Dentist", nil, ""),
		ev("Work", "Focus time", nil, ""),
		ev("Work", "Pinned", nil, ""),
	}
	_ = fixes.set(evs[6].SeriesId, "Hooli", "Pinned")
	c.classify(evs)

	want := []string{
		`title says "Globex"`,
		"organizer is @hooli.example",
		`title says "ARC"`,
		"Teams tenant aaaa1111",
		"default for the Family calendar",
		"no clues matched",
		"you set this",
	}
	for i, w := range want {
		if !strings.Contains(evs[i].EntityWhy, w) {
			t.Errorf("%q why = %q, want it to mention %q", evs[i].Name, evs[i].EntityWhy, w)
		}
	}
	if !strings.HasPrefix(evs[3].EntityWhy, "model, ") {
		t.Errorf("learned why = %q, want it to lead with the model's confidence", evs[3].EntityWhy)
	}
}

func TestFetchFeedCarriesCategories(t *testing.T) {
	ics := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VEVENT",
		"UID:cat-1",
		"DTSTAMP:20260601T000000Z",
		"SUMMARY:Board prep",
		"CATEGORIES:Globex,Follow up",
		"DTSTART:20260601T140000Z",
		"DTEND:20260601T150000Z",
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n")
	evs := fetchICS(t, ics)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if got := strings.Join(evs[0].Categories, "|"); got != "Globex|Follow up" {
		t.Errorf("categories = %q", got)
	}
}

// fetchICS serves ics from a test server and runs it through fetchFeed.
func fetchICS(t *testing.T, ics string) []Event {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ics))
	}))
	defer srv.Close()
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	evs, err := fetchFeed(srv.Client(), Feed{Name: "Work", URL: srv.URL}, start, start.AddDate(0, 0, 7), time.UTC, start, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// TestClassifyLearnsPeople checks that attendees seen on labeled events carry
// over: a meeting with no keyword and an unconfigured domain still lands on the
// client whose people are in it.
func TestClassifyLearnsPeople(t *testing.T) {
	c := newClassifier([]Entity{
		{Name: "Globex Records", Keywords: []string{"Globex", "Initech"}},
		{Name: "Hooli", Keywords: []string{"Hooli"}, Domains: []string{"hooli.example"}},
	})
	me := Attendee{Email: "me@northwind.example"}
	anne := Attendee{Email: "anne@initech.example"}
	jacob := Attendee{Email: "jacob@globexrecords.example"}
	sam := Attendee{Email: "scarter@hooli.example", Role: "organizer"}
	ev := func(name string, att ...Attendee) Event {
		return Event{Name: name, SeriesId: seriesID(name), sig: buildSignals("Work", name, "", "", nil, "", att, "")}
	}
	evs := []Event{
		ev("Globex Records VIP: Billing", anne, jacob, me),
		ev("Initech status", anne, me),
		ev("Weekly Touchbase", sam, me),
		ev("Contract regroup", anne, jacob, me), // no keyword, no configured domain
		ev("Quick sync", me),                    // only you: nothing to learn from
	}
	c.classify(evs)
	if got := evs[3]; got.Entity != "Globex Records" || got.EntityVia != viaLearned {
		t.Errorf("Contract regroup => %q via %q, want Globex Records via learned", got.Entity, got.EntityVia)
	}
	if got := evs[4]; got.Entity != "" {
		t.Errorf("Quick sync => %q, want unlabeled", got.Entity)
	}
}
