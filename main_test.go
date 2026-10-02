package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apognu/gocal"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCleanDescription(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain agenda untouched",
			in:   "1. Review roadmap\n2. Budget\n3. Q&A",
			want: "1. Review roadmap\n2. Budget\n3. Q&A",
		},
		{
			name: "agenda then teams underscore block",
			in: "Agenda:\n- Demo\n- Retro\n" +
				"________________________________________________________________________________\n" +
				"Microsoft Teams meeting\n" +
				"Join on your computer, mobile app or room device\n" +
				"Click here to join the meeting\n" +
				"Meeting ID: 123 456 789 0\n" +
				"Passcode: abcdef\n" +
				"________________________________________________________________________________",
			want: "Agenda:\n- Demo\n- Retro",
		},
		{
			name: "google meet delimited block removed",
			in: "Pre-read the doc.\n" +
				"-::~:~::~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:-\n" +
				"Join with Google Meet: https://meet.google.com/abc-defg-hij\n" +
				"Or dial: +1 555-555-5555 PIN: 123456789#\n" +
				"-::~:~::~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:-",
			want: "Pre-read the doc.",
		},
		{
			name: "zoom invite block removed",
			in: "Standup\n" +
				"Jane Doe is inviting you to a scheduled Zoom meeting.\n" +
				"Join Zoom Meeting\n" +
				"https://us02web.zoom.us/j/123456789\n" +
				"Meeting ID: 123 456 789\n" +
				"Passcode: 999\n" +
				"One tap mobile\n" +
				"Dial by your location",
			want: "Standup",
		},
		{
			name: "html description de-tagged",
			in:   "<p>Bring <b>laptops</b></p>\\n<p>&amp; chargers</p>",
			want: "Bring laptops\n& chargers",
		},
		{
			name: "ics escaped commas and semicolons",
			in:   `Topics\, in order\; intro\, demo`,
			want: "Topics, in order; intro, demo",
		},
		{
			name: "only boilerplate yields empty",
			in: "________________________________________________________________________________\n" +
				"Microsoft Teams meeting\n" +
				"Join on your computer, mobile app or room device\n" +
				"________________________________________________________________________________",
			want: "",
		},
		{
			name: "newer teams footer with inner rule",
			in: "Weekly billing call.\nThanks,\nAnne\n\n" +
				"________________________________________________________________________________\n" +
				"Microsoft Teams Need help?<https://aka.ms/JoinTeamsMeeting?omkt=en-US>\n" +
				"Join the meeting now<https://teams.microsoft.com/l/meetup-join/19%3ameeting_abc%40thread.v2/0>\n" +
				"Meeting ID: 123 456 789 012\n" +
				"Passcode: aB3cD4\n" +
				"________________________________\n" +
				"Need help?<https://aka.ms/JoinTeamsMeeting?omkt=en-US> | System reference<https://teams.microsoft.com/l/meetup-join/19%3ameeting_abc%40thread.v2/0>\n" +
				"Dial in by phone\n" +
				"+1 555-555-0100,,123456789# United States, Chicago\n" +
				"Find a local number<https://dialin.teams.microsoft.com/abc>\n" +
				"Phone conference ID: 123 456 789#\n" +
				"Join on a video conferencing device\n" +
				"Tenant key: teams@vc.example.com\n" +
				"Video ID: 111 222 333 4\n" +
				"More info<https://example.com/vtc>\n" +
				"For organizers: Meeting options<https://teams.microsoft.com/meetingOptions/?organizerId=abc> | Reset dial-in PIN<https://dialin.teams.microsoft.com/usp/pstnconferencing>\n" +
				"________________________________________________________________________________\n" +
				"[Company Logo]\n",
			want: "Weekly billing call.\nThanks,\nAnne",
		},
		{
			name: "stray newer teams lines without rules",
			in: "Catch up\n\n" +
				"Need help?<https://aka.ms/JoinTeamsMeeting?omkt=en-US> | System reference<https://teams.microsoft.com/l/meetup-join/19%3ameeting_abc%40thread.v2/0>\n" +
				"For organizers: Meeting options<https://teams.microsoft.com/meetingOptions/?organizerId=abc>\n" +
				"[https://www.example.com/images/logo.png]",
			want: "Catch up",
		},
		{
			name: "google calendar sync header and when tail",
			in: "Support Training Operations\n\n" +
				"This email keeps the event up to date in your calendar.\n" +
				"Set up inbox filters to hide this and similar calendar sync emails.\n\n" +
				"Meeting link\n" +
				"meet.google.com/abc-defg-hij\n" +
				"Join by phone\n" +
				"(US) +1 555-555-0100\n" +
				"PIN: 123456789\n\n" +
				"Hello Everyone,\n\nWe will meet daily.\n\nThank you\n" +
				"When\n" +
				"Weekly from 8:30am to 10:30am on weekdays (Eastern Time - New York)",
			want: "Support Training Operations\n\nHello Everyone,\n\nWe will meet daily.\n\nThank you",
		},
		{
			name: "when line kept outside google invites",
			in:   "Offsite planning\nWhen\nTBD, pick a date in the poll",
			want: "Offsite planning\nWhen\nTBD, pick a date in the poll",
		},
		{
			name: "real sentences that start like boilerplate are kept",
			in:   "Meeting link is in the chat\nMore info on the budget below\nPIN: the doc to the channel",
			want: "Meeting link is in the chat\nMore info on the budget below\nPIN: the doc to the channel",
		},
		{
			name: "corporate disclaimer after teams block dropped",
			in: "Agenda:\n  1.  Intro\n  2.  Azure\n\n" +
				"________________________________________________________________________________\n" +
				"Microsoft Teams Need help?<https://aka.ms/JoinTeamsMeeting?omkt=en-US>\n" +
				"________________________________________________________________________________\n" +
				"** This email message is for the sole use of the intended recipient(s) and may contain confidential and privileged information.\n" +
				"Any unauthorized review, use, disclosure or distribution is prohibited. **",
			want: "Agenda:\n  1.  Intro\n  2.  Azure",
		},
		{
			name: "only a disclaimer yields empty",
			in:   "CONFIDENTIALITY NOTICE: The contents of this email are private.",
			want: "",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanDescription(tt.in); got != tt.want {
				t.Errorf("cleanDescription()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestStripTitleEcho(t *testing.T) {
	tests := []struct {
		agenda, name, want string
	}{
		{"Review savings for AWS", "Review savings for AWS", ""},
		{"Standup\n\nHello team", "standup", "Hello team"},
		{"Hello team\nStandup", "Standup", "Hello team\nStandup"},
		{"Hello team", "", "Hello team"},
		{"", "Standup", ""},
	}
	for _, tt := range tests {
		if got := stripTitleEcho(tt.agenda, tt.name); got != tt.want {
			t.Errorf("stripTitleEcho(%q, %q) = %q, want %q", tt.agenda, tt.name, got, tt.want)
		}
	}
}

func TestParseAttendees(t *testing.T) {
	in := []gocal.Attendee{
		{Cn: "Grace Hopper", Value: "mailto:grace@example.com", Status: "ACCEPTED"},
		{Cn: "John Connor", Value: "MAILTO:john@example.com", Status: "NEEDS-ACTION"},
		{Cn: "", Value: "mailto:noname@example.com", Status: "DECLINED"},
		{Cn: "Room A", Value: ""},                      // name only, no email
		{Cn: "", Value: ""},                            // dropped: neither
		{Cn: "Dup", Value: "mailto:GRACE@example.com"}, // dropped: dup email (case-insensitive)
	}
	want := []Attendee{
		{Name: "Grace Hopper", Email: "grace@example.com", Status: "accepted"},
		{Name: "John Connor", Email: "john@example.com", Status: "needs-action"},
		{Name: "", Email: "noname@example.com", Status: "declined"},
		{Name: "Room A", Email: "", Status: ""},
	}
	if got := parseAttendees(in); !reflect.DeepEqual(got, want) {
		t.Errorf("parseAttendees()\n got: %#v\nwant: %#v", got, want)
	}
	if got := parseAttendees(nil); got != nil {
		t.Errorf("parseAttendees(nil) = %#v, want nil", got)
	}
}

func TestOrganizerName(t *testing.T) {
	if got := organizerName(nil); got != "" {
		t.Errorf("organizerName(nil) = %q, want \"\"", got)
	}
	if got := organizerName(&gocal.Organizer{Cn: "Ada", Value: "mailto:ada@example.com"}); got != "Ada" {
		t.Errorf("organizerName(cn) = %q, want \"Ada\"", got)
	}
	if got := organizerName(&gocal.Organizer{Value: "mailto:ada@example.com"}); got != "ada@example.com" {
		t.Errorf("organizerName(value-only) = %q, want \"ada@example.com\"", got)
	}
}

// googleInviteDesc mirrors the DESCRIPTION that forwarded Google Calendar
// invites leave on published Outlook feeds (which strip structured ATTENDEE).
const googleInviteDesc = "Review savings for AWS\n" +
	"Join with Google Meet – You have been invited by Sam Carter to attend an event named Review savings for AWS on Wednesday Feb 4, 2026 ⋅ 11am – 12pm (Eastern Time - New York).\n" +
	"\n" +
	"<https://meet.google.com/abc-defg-hij?hs=224>\n" +
	"Join with Google Meet\n" +
	"Meeting link\n" +
	"meet.google.com/abc-defg-hij<https://meet.google.com/abc-defg-hij?hs=224>\n" +
	"Join by phone\n" +
	"(US) +1 629-888-3246<tel:+1-629-888-3246;980867733#>\n" +
	"PIN: 980867733\n" +
	"When\n" +
	"Wednesday Feb 4, 2026 ⋅ 11am – 12pm (Eastern Time - New York)\n" +
	"Guests\n" +
	"Sam Carter<mailto:scarter@hooli.example> - organizer\n" +
	"me@northwind.example<mailto:me@northwind.example>\n" +
	"Blake Kim<mailto:bkim@hooli.example> - optional\n" +
	"View all guest info<https://calendar.google.com/calendar/event?action=VIEW>\n" +
	"Reply for me@northwind.example<mailto:me@northwind.example>\n" +
	"Invitation from Google Calendar<https://calendar.google.com/calendar/>\n"

func TestParseDescriptionGuests(t *testing.T) {
	org, att := parseDescriptionGuests(googleInviteDesc)
	if org != "Sam Carter" {
		t.Errorf("organizer = %q, want %q", org, "Sam Carter")
	}
	want := []Attendee{
		{Name: "Sam Carter", Email: "scarter@hooli.example", Role: "organizer"},
		{Email: "me@northwind.example", Role: "required"},
		{Name: "Blake Kim", Email: "bkim@hooli.example", Role: "optional"},
	}
	if !reflect.DeepEqual(att, want) {
		t.Errorf("attendees\n got: %#v\nwant: %#v", att, want)
	}

	// The "Reply for ...<mailto:>" line after the roster must not be parsed as a guest.
	for _, a := range att {
		if a.Email == "me@northwind.example" && a.Role != "required" {
			t.Errorf("me role = %q, want required (Reply-for line leaked?)", a.Role)
		}
	}

	// A description with no roster yields nothing.
	if o, a := parseDescriptionGuests("Just some notes, no guests."); o != "" || a != nil {
		t.Errorf("no-roster = (%q, %#v), want (\"\", nil)", o, a)
	}
}

func TestCleanDescriptionGoogleInvite(t *testing.T) {
	// The entire Google footer (meeting link, phone, When, Guests, RSVP) is
	// stripped, leaving only the user-facing preamble.
	if got := cleanDescription(googleInviteDesc); got != "Review savings for AWS" {
		t.Errorf("cleanDescription(googleInvite) = %q, want %q", got, "Review savings for AWS")
	}
}

func TestMergeAttendees(t *testing.T) {
	// Base = structured ATTENDEE (has status, no role).
	base := []Attendee{{Name: "Sam Carter", Email: "scarter@hooli.example", Status: "accepted"}}
	// Extra = DESCRIPTION roster (has role, no status); note case-different dup + a new person.
	extra := []Attendee{
		{Name: "Sam C.", Email: "SCARTER@hooli.example", Role: "organizer"},
		{Email: "bkim@hooli.example", Role: "optional"},
	}
	got := mergeAttendees(base, extra)
	want := []Attendee{
		// existing status kept (no clobber), role filled from extra, original name kept.
		{Name: "Sam Carter", Email: "scarter@hooli.example", Status: "accepted", Role: "organizer"},
		{Email: "bkim@hooli.example", Role: "optional"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeAttendees\n got: %#v\nwant: %#v", got, want)
	}
}

func TestSeriesID(t *testing.T) {
	a := seriesID("ABC-123@example.com")
	b := seriesID("  ABC-123@example.com  ") // trimmed -> same
	c := seriesID("different@example.com")
	if a == "" || len(a) != 12 {
		t.Errorf("seriesID len = %d (%q), want 12", len(a), a)
	}
	if a != b {
		t.Errorf("seriesID not stable across whitespace: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("seriesID collided for different UIDs: %q", a)
	}
	if got := seriesID("  "); got != "" {
		t.Errorf("seriesID(empty) = %q, want \"\"", got)
	}
}

// TestFetchFeedAllDaySpan checks that a multi-day all-day event whose start
// precedes the fetch window still reports its true (un-clipped) span and that
// dayIndex reflects the real position within that span.
func TestFetchFeedAllDaySpan(t *testing.T) {
	const ics = "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//test//test//EN\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:vacation-jordan@example.com\r\n" +
		"DTSTAMP:20260601T120000Z\r\n" +
		"SUMMARY:Jordan - Vacation Alert\r\n" +
		"DTSTART;VALUE=DATE:20260615\r\n" +
		"DTEND;VALUE=DATE:20260627\r\n" + // exclusive -> inclusive end 2026-06-26, span 12 days
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/calendar")
		_, _ = w.Write([]byte(ics))
	}))
	defer srv.Close()

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 17, 9, 0, 0, 0, ny)
	// Fetch window starts 2026-06-17 (after the event's 06-15 start), runs 30 days.
	start := time.Date(2026, 6, 17, 0, 0, 0, 0, ny)
	end := start.AddDate(0, 0, 30)

	evs, err := fetchFeed(srv.Client(), Feed{Name: "Fam", URL: srv.URL, Color: "#fff"}, start, end, ny, now, true, true)
	if err != nil {
		t.Fatal(err)
	}

	// Window clips the first two days (06-15, 06-16): expect 06-17 .. 06-26 = 10 entries.
	if len(evs) != 10 {
		t.Fatalf("got %d instances, want 10", len(evs))
	}

	first, last := evs[0], evs[len(evs)-1]
	if first.SeriesId == "" || first.SeriesId != last.SeriesId {
		t.Errorf("seriesId not shared across instances: %q vs %q", first.SeriesId, last.SeriesId)
	}
	for _, e := range evs {
		if e.StartDate != "2026-06-15" || e.EndDate != "2026-06-26" {
			t.Errorf("span dates = %s..%s, want 2026-06-15..2026-06-26", e.StartDate, e.EndDate)
		}
		if e.SpanDays != 12 || !e.MultiDay {
			t.Errorf("spanDays=%d multiDay=%v, want 12/true", e.SpanDays, e.MultiDay)
		}
		if e.SeriesId != first.SeriesId {
			t.Errorf("seriesId drifted: %q", e.SeriesId)
		}
	}
	// First visible day is 06-17 = day 3 of the true span, not 1.
	if first.Date != "2026-06-17" || first.DayIndex != 3 {
		t.Errorf("first visible instance = %s dayIndex %d, want 2026-06-17/3", first.Date, first.DayIndex)
	}
	if last.Date != "2026-06-26" || last.DayIndex != 12 {
		t.Errorf("last instance = %s dayIndex %d, want 2026-06-26/12", last.Date, last.DayIndex)
	}
}

func TestCivilDays(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	d := func(y int, m time.Month, day int) time.Time {
		return time.Date(y, m, day, 0, 0, 0, 0, ny)
	}
	tests := []struct {
		name     string
		from, to time.Time
		want     int
	}{
		{"same day", d(2026, 6, 15), d(2026, 6, 15), 0},
		{"consecutive", d(2026, 6, 15), d(2026, 6, 16), 1},
		{"span", d(2026, 6, 15), d(2026, 6, 26), 11},
		{"across fall DST boundary", d(2026, 11, 1), d(2026, 11, 3), 2}, // clocks fall back Nov 1, 2026
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := civilDays(tt.from, tt.to); got != tt.want {
				t.Errorf("civilDays() = %d, want %d", got, tt.want)
			}
		})
	}
}

// mcpTestConfig is a minimal config for exercising the /mcp handler.
func mcpTestConfig(t *testing.T) config {
	t.Helper()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return config{loc: ny, aheadDays: 30, cacheTTL: 15 * time.Minute, authToken: "secret"}
}

// fixedConfig serves one config to newMCPHandler, like a server whose config
// file never changes.
func fixedConfig(cfg config) func() *config {
	return func() *config { return &cfg }
}

// storeWithDays returns a ready store holding one event per day for n days
// starting today (local), refreshed just now.
func storeWithDays(cfg config, n int) *store {
	now := time.Now().In(cfg.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, cfg.loc)
	resp := Response{Generated: now.Format(time.RFC3339)}
	for i := 0; i < n; i++ {
		d := today.AddDate(0, 0, i).Format("2006-01-02")
		ev := Event{Name: "Standup", Start: d + "T09:00:00-04:00", Date: d, Source: "Work"}
		resp.Days = append(resp.Days, Day{Label: d, Date: d, Events: []Event{ev}})
		resp.Events = append(resp.Events, ev)
	}
	resp.Count = len(resp.Events)
	st := &store{}
	st.set(resp, nil)
	return st
}

const mcpInitBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`

func mcpInitRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(mcpInitBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	return r
}

func TestMCPAuth(t *testing.T) {
	cfg := mcpTestConfig(t)
	h := requireTunnelAuth(fixedConfig(cfg), newMCPHandler(fixedConfig(cfg), storeWithDays(cfg, 3)))

	t.Run("tunnel without bearer is 401", func(t *testing.T) {
		r := mcpInitRequest()
		r.Header.Set("Cf-Ray", "abc123-EWR")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if got := w.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("WWW-Authenticate = %q, want Bearer", got)
		}
	})

	t.Run("tunnel with valid bearer initializes", func(t *testing.T) {
		r := mcpInitRequest()
		r.Header.Set("Cf-Ray", "abc123-EWR")
		r.Header.Set("Authorization", "Bearer secret")
		// Mirror cloudflared: loopback origin, public Host. The SDK's DNS-rebinding
		// guard would 403 this unless DisableLocalhostProtection is set.
		r.Host = "calmerge.example.com"
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
			&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8076}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"calmerge"`) {
			t.Errorf("initialize response missing server info: %s", w.Body.String())
		}
	})

	t.Run("internal without Cf headers is allowed", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, mcpInitRequest())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
	})

	// require_auth: the token is needed even without Cloudflare's headers.
	strict := cfg
	strict.requireAuth = true
	hs := requireTunnelAuth(fixedConfig(strict), newMCPHandler(fixedConfig(strict), storeWithDays(strict, 3)))

	t.Run("require_auth: internal without bearer is 401", func(t *testing.T) {
		w := httptest.NewRecorder()
		hs.ServeHTTP(w, mcpInitRequest())
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})

	t.Run("require_auth: internal with bearer initializes", func(t *testing.T) {
		r := mcpInitRequest()
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		hs.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("require_auth with no AUTH_TOKEN rejects everyone", func(t *testing.T) {
		open := strict
		open.authToken = ""
		r := mcpInitRequest()
		r.Header.Set("Authorization", "Bearer ")
		w := httptest.NewRecorder()
		requireTunnelAuth(fixedConfig(open), http.NotFoundHandler()).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})
}

// callEvents invokes get_calendar_events over a real Streamable HTTP client
// and decodes the JSON text payload.
func callEvents(t *testing.T, cfg config, st *store, args map[string]any) (mcpEvents, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(newMCPHandler(fixedConfig(cfg), st))
	defer srv.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_calendar_events", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("unexpected tool result: %#v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	var out mcpEvents
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatal(err)
	}
	return out, raw
}

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestMCPGetCalendarEvents(t *testing.T) {
	t.Run("tool metadata", func(t *testing.T) {
		cfg := mcpTestConfig(t)
		srv := httptest.NewServer(newMCPHandler(fixedConfig(cfg), storeWithDays(cfg, 1)))
		defer srv.Close()
		ctx := context.Background()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		lt, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		tools := map[string]*mcp.Tool{}
		for _, tl := range lt.Tools {
			tools[tl.Name] = tl
		}
		if len(tools) != 2 || tools["get_calendar_events"] == nil || tools["set_event_entity"] == nil {
			t.Fatalf("tools = %#v, want get_calendar_events and set_event_entity", lt.Tools)
		}
		if a := tools["get_calendar_events"].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("get_calendar_events readOnlyHint not set: %#v", a)
		}
		if a := tools["set_event_entity"].Annotations; a == nil || a.ReadOnlyHint {
			t.Errorf("set_event_entity must not claim to be read-only: %#v", a)
		}
	})

	clampTests := []struct {
		name     string
		args     map[string]any
		wantDays int
		capped   bool
	}{
		{"default is 3", nil, 3, false},
		{"zero falls back to default", map[string]any{"days": 0}, 3, false},
		{"one", map[string]any{"days": 1}, 1, false},
		{"fourteen", map[string]any{"days": 14}, 14, false},
		{"past two weeks", map[string]any{"days": 28}, 28, false},
		{"whole lookahead", map[string]any{"days": 31}, 31, false},
		{"over lookahead clamps", map[string]any{"days": 60}, 31, true},
	}
	for _, tt := range clampTests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mcpTestConfig(t)
			out, raw := callEvents(t, cfg, storeWithDays(cfg, 40), tt.args)
			if len(out.Days) != tt.wantDays || out.Count != tt.wantDays {
				t.Errorf("days=%d count=%d, want %d", len(out.Days), out.Count, tt.wantDays)
			}
			if got := hasWarning(out.Warnings, "lookahead is 30 days"); got != tt.capped {
				t.Errorf("lookahead warning = %v, want %v (warnings %q)", got, tt.capped, out.Warnings)
			}
			if _, ok := raw["events"]; ok {
				t.Errorf("flat events[] list should be dropped from the MCP payload")
			}
			if out.Warming || out.Updated == "" {
				t.Errorf("warming=%v updated=%q, want false/non-empty", out.Warming, out.Updated)
			}
		})
	}

	t.Run("cache warming", func(t *testing.T) {
		cfg := mcpTestConfig(t)
		st := &store{resp: Response{Generated: time.Now().Format(time.RFC3339), Days: []Day{}}}
		out, _ := callEvents(t, cfg, st, nil)
		if !out.Warming || out.Updated != "" {
			t.Errorf("warming=%v updated=%q, want true/empty", out.Warming, out.Updated)
		}
		if out.Days == nil {
			t.Errorf("days should be [] not null while warming")
		}
		if !hasWarning(out.Warnings, "warming") {
			t.Errorf("missing warming warning: %q", out.Warnings)
		}
	})

	t.Run("stale data", func(t *testing.T) {
		cfg := mcpTestConfig(t)
		st := storeWithDays(cfg, 3)
		st.updated = time.Now().Add(-2*cfg.cacheTTL - time.Minute)
		out, _ := callEvents(t, cfg, st, nil)
		if !hasWarning(out.Warnings, "stale") {
			t.Errorf("missing stale warning: %q", out.Warnings)
		}

		// Just inside 2x TTL is not stale.
		st.updated = time.Now().Add(-2*cfg.cacheTTL + time.Minute)
		out, _ = callEvents(t, cfg, st, nil)
		if hasWarning(out.Warnings, "stale") {
			t.Errorf("unexpected stale warning: %q", out.Warnings)
		}
	})

	t.Run("feed errors", func(t *testing.T) {
		cfg := mcpTestConfig(t)
		st := storeWithDays(cfg, 3)
		st.resp.Errors = []string{"Work: HTTP 500"}
		out, _ := callEvents(t, cfg, st, nil)
		if !reflect.DeepEqual(out.Errors, []string{"Work: HTTP 500"}) {
			t.Errorf("errors = %q", out.Errors)
		}
		if !hasWarning(out.Warnings, "1 feed(s) failed") {
			t.Errorf("missing feed-error warning: %q", out.Warnings)
		}
	})

	t.Run("past lookahead", func(t *testing.T) {
		cfg := mcpTestConfig(t)
		cfg.aheadDays = 5
		out, _ := callEvents(t, cfg, storeWithDays(cfg, 20), map[string]any{"days": 10})
		if len(out.Days) != 6 {
			t.Errorf("days = %d, want 6 (lookahead 5 + today)", len(out.Days))
		}
		if !hasWarning(out.Warnings, "lookahead is 5 days") {
			t.Errorf("missing lookahead warning: %q", out.Warnings)
		}
	})
}

// TestRemapWindowsTZ: every Windows zone name maps to its own IANA zone, even
// when one name is the start of another ("Pacific Standard Time" and "Pacific
// Standard Time (Mexico)").
func TestRemapWindowsTZ(t *testing.T) {
	tests := []struct{ in, want string }{
		{"DTSTART;TZID=Eastern Standard Time:20260601T090000", "DTSTART;TZID=America/New_York:20260601T090000"},
		{"TZID:Pacific Standard Time", "TZID:America/Los_Angeles"},
		{"TZID:Pacific Standard Time (Mexico)", "TZID:America/Tijuana"},
		{"DTSTART;TZID=Central Standard Time (Mexico):20260601T090000", "DTSTART;TZID=America/Mexico_City:20260601T090000"},
		{"TZID:US Eastern Standard Time", "TZID:America/Indiana/Indianapolis"},
		{"TZID:America/Chicago", "TZID:America/Chicago"},
	}
	for _, tt := range tests {
		if got := remapWindowsTZ(tt.in); got != tt.want {
			t.Errorf("remapWindowsTZ(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestFetchFeedErrorsHideURL: feed URLs are secrets and fetch errors are
// served in the errors list, so a failed fetch must never echo the URL.
func TestFetchFeedErrorsHideURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	secret := srv.URL + "/published/s3cr3t-token/calendar.ics"
	srv.Close() // nothing listens any more: the fetch fails to connect
	now := time.Now()
	for _, u := range []string{secret, "https://example.com/s3cr3t-token/\x7f.ics"} {
		_, err := fetchFeed(&http.Client{Timeout: 2 * time.Second}, Feed{Name: "Work", URL: u}, now, now.AddDate(0, 0, 7), time.UTC, now, true, true)
		if err == nil {
			t.Fatalf("fetchFeed(%q) succeeded, want an error", u)
		}
		if strings.Contains(err.Error(), "s3cr3t-token") {
			t.Errorf("error leaks the feed URL: %v", err)
		}
	}
}

// TestFetchFeedSizeCap: a feed past maxFeedBytes is refused instead of read
// into memory.
func TestFetchFeedSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for i := 0; i <= maxFeedBytes>>20; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	now := time.Now()
	_, err := fetchFeed(srv.Client(), Feed{Name: "Work", URL: srv.URL}, now, now.AddDate(0, 0, 7), time.UTC, now, true, true)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want a size-cap error", err)
	}
}

// TestBuildKeepsLastGoodFeed: a feed that fails after a good fetch keeps its
// events (with the failure and their age in errors) instead of vanishing
// until the next refresh, while a feed that never loaded is just reported.
func TestBuildKeepsLastGoodFeed(t *testing.T) {
	old := feedRetryDelay
	feedRetryDelay = 0
	defer func() { feedRetryDelay = old }()

	day := time.Now().UTC().AddDate(0, 0, 1).Format("20060102")
	ics := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VEVENT",
		"UID:standup-1",
		"DTSTAMP:20260601T000000Z",
		"SUMMARY:Standup",
		"DTSTART:" + day + "T140000Z",
		"DTEND:" + day + "T143000Z",
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n")
	var down atomic.Bool
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if down.Load() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(ics))
	}))
	defer srv.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	defer dead.Close()

	cfg := config{loc: time.UTC, aheadDays: 7, feeds: []Feed{
		{Name: "Work", URL: srv.URL},
		{Name: "Family", URL: dead.URL},
	}}
	fc := &feedCache{}

	first := build(cfg, srv.Client(), fc)
	if first.Count != 1 || len(first.Errors) != 1 || first.Errors[0] != "Family: HTTP 404" {
		t.Fatalf("first build: %d events, errors %q; want 1 event and only Family failing", first.Count, first.Errors)
	}

	down.Store(true)
	hits.Store(0)
	second := build(cfg, srv.Client(), fc)
	if second.Count != 1 || second.Events[0].Name != "Standup" {
		t.Fatalf("second build has %d events, want the Standup kept from the last good fetch", second.Count)
	}
	if hits.Load() != 2 {
		t.Errorf("failed feed was fetched %d times, want 2 (one retry)", hits.Load())
	}
	var work string
	for _, e := range second.Errors {
		if strings.HasPrefix(e, "Work: ") {
			work = e
		}
	}
	if !strings.Contains(work, "HTTP 503") || !strings.Contains(work, "showing its events as of") {
		t.Errorf("errors = %q, want Work's failure and the age of what's shown", second.Errors)
	}

	// With nothing remembered the feed simply drops out.
	if got := build(cfg, srv.Client(), nil); got.Count != 0 || len(got.Errors) != 2 {
		t.Errorf("no cache: %d events, errors %q; want 0 events and both feeds failing", got.Count, got.Errors)
	}
}

// TestFeedCacheGet: remembered events are brought up to date before they
// stand in: finished meetings turn past, days before the window go, and a
// copy that's too old isn't used at all.
func TestFeedCacheGet(t *testing.T) {
	at := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	f := Feed{Name: "Work", URL: "https://example.com/w.ics"}
	fc := &feedCache{}
	fc.put(f, []Event{
		{Name: "Yesterday", Date: "2026-05-31", Start: "2026-05-31T09:00:00Z", End: "2026-05-31T10:00:00Z"},
		{Name: "Morning", Date: "2026-06-01", Start: "2026-06-01T09:00:00Z", End: "2026-06-01T10:00:00Z"},
		{Name: "Lunch", Date: "2026-06-01", Start: "2026-06-01T12:00:00Z", End: "2026-06-01T13:00:00Z"},
		{Name: "Offsite", Date: "2026-06-01", Start: "2026-06-01T00:00:00Z", AllDay: true},
	}, at)

	now := at.Add(4*time.Hour + 30*time.Minute) // 12:30
	evs, got, ok := fc.get(f, now, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if !ok || !got.Equal(at) || len(evs) != 3 {
		t.Fatalf("get = %d events, at %s, ok %v; want 3 from %s", len(evs), got, ok, at)
	}
	if !evs[0].Past || evs[0].Ongoing {
		t.Errorf("Morning past=%v ongoing=%v, want past", evs[0].Past, evs[0].Ongoing)
	}
	if evs[1].Past || !evs[1].Ongoing {
		t.Errorf("Lunch past=%v ongoing=%v, want ongoing", evs[1].Past, evs[1].Ongoing)
	}
	if evs[2].Past || evs[2].Ongoing {
		t.Errorf("all-day Offsite past=%v ongoing=%v, want neither", evs[2].Past, evs[2].Ongoing)
	}
	if _, _, ok := fc.get(f, at.Add(maxStaleFeed+time.Minute), at); ok {
		t.Error("a copy older than maxStaleFeed was still used")
	}
	fc.keep(nil)
	if _, _, ok := fc.get(f, now, at); ok {
		t.Error("a feed dropped from the config was still remembered")
	}
}
