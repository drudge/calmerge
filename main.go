// calmerge: merges multiple ICS calendar feeds into a single, source-tagged,
// chronologically-sorted JSON feed for a Glance custom-api widget.
package main

import (
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apognu/gocal"
)

type Feed struct {
	Name  string `json:"name" toml:"name"`
	URL   string `json:"url" toml:"url"`
	Color string `json:"color,omitempty" toml:"color,omitempty"`
	User  string `json:"user,omitempty" toml:"user,omitempty"`
	Pass  string `json:"pass,omitempty" toml:"pass,omitempty"`
}

// Attendee is a single normalized invitee on an event.
type Attendee struct {
	Name   string `json:"name,omitempty"`   // CN
	Email  string `json:"email,omitempty"`  // from mailto: value
	Status string `json:"status,omitempty"` // accepted | declined | tentative | needs-action
	Role   string `json:"role,omitempty"`   // organizer | required | optional
}

type Event struct {
	Name     string `json:"name"`
	Start    string `json:"start"`              // RFC3339
	End      string `json:"end,omitempty"`      // RFC3339
	Date     string `json:"date"`               // YYYY-MM-DD (local)
	Duration string `json:"duration,omitempty"` // human, e.g. "30m", "1h", "1h30m"
	AllDay   bool   `json:"allDay"`
	Ongoing  bool   `json:"ongoing"`
	Past     bool   `json:"past,omitempty"`
	Location string `json:"location,omitempty"` // short label (venue / first line)
	Address  string `json:"address,omitempty"`  // full address for tooltip + maps
	MapURL   string `json:"mapUrl,omitempty"`
	MeetURL  string `json:"meetUrl,omitempty"`
	MeetKind string `json:"meetKind,omitempty"` // Teams | Meet | Zoom
	LocKind  string `json:"locKind,omitempty"`  // map | link

	Organizer string     `json:"organizer,omitempty"` // organizer display name (or email)
	Attendees []Attendee `json:"attendees,omitempty"`
	Agenda    string     `json:"agenda,omitempty"` // cleaned DESCRIPTION text

	// Busy state and RSVP, so readers can leave out meetings you aren't going
	// to. Published Outlook feeds strip ATTENDEE, so there showAs and
	// transparent are the only signals; myResponse needs self_emails and a
	// feed that keeps ATTENDEE (Google, iCloud).
	Status      string `json:"status,omitempty"`      // confirmed | tentative (STATUS; cancelled events are skipped)
	ShowAs      string `json:"showAs,omitempty"`      // free | tentative | busy | oof | workingelsewhere (Outlook)
	Transparent bool   `json:"transparent,omitempty"` // TRANSP:TRANSPARENT: doesn't block time
	MyResponse  string `json:"myResponse,omitempty"`  // accepted | declined | tentative | needs-action

	// Span / grouping. seriesId is set on every event; the rest only on all-day
	// instances (where one source event expands to one entry per day).
	SeriesId  string `json:"seriesId,omitempty"`  // stable id shared by all instances of one source event
	StartDate string `json:"startDate,omitempty"` // true inclusive span start, "2006-01-02", NOT window-clipped
	EndDate   string `json:"endDate,omitempty"`   // true inclusive span end (DTEND is exclusive; we subtract a day)
	MultiDay  bool   `json:"multiDay,omitempty"`  // span > 1 day
	SpanDays  int    `json:"spanDays,omitempty"`  // inclusive day count of the span
	DayIndex  int    `json:"dayIndex,omitempty"`  // 1-based position of this instance within the true span

	Source string `json:"source"`
	Color  string `json:"color"`

	// Business/entity this event belongs to, when ENTITIES is configured.
	Categories  []string `json:"categories,omitempty"`  // raw ICS CATEGORIES (Outlook categories)
	Entity      string   `json:"entity,omitempty"`      // e.g. "Globex Records"; empty when unsure
	EntityColor string   `json:"entityColor,omitempty"` // the entity's configured color
	EntityVia   string   `json:"entityVia,omitempty"`   // manual | category | rule | learned | feed
	EntityWhy   string   `json:"entityWhy,omitempty"`   // plain-words reason (matched rule, model clues, or its unsure lean)

	sig signals // classifier inputs; not serialized
}

type Day struct {
	Label  string  `json:"label"` // e.g. "Today \u00b7 Fri, May 29"
	Date   string  `json:"date"`  // YYYY-MM-DD
	Events []Event `json:"events"`
}

type Response struct {
	Generated string       `json:"generated"`
	Count     int          `json:"count"`
	Days      []Day        `json:"days"`
	Events    []Event      `json:"events"`             // flat list retained for convenience/debugging
	Entities  []EntityInfo `json:"entities,omitempty"` // configured entities, when classification is on
	Errors    []string     `json:"errors,omitempty"`
}

type config struct {
	feeds        []Feed
	listen       string
	loc          *time.Location
	lookbackDays int
	aheadDays    int
	cacheTTL     time.Duration
	httpTimeout  time.Duration
	authToken    string

	includeAttendees bool
	includeAgenda    bool
	requireAuth      bool // token on every /events and /mcp request, not just tunnel traffic

	selfEmails   []string // lowercased addresses that are "me", for myResponse
	skipDeclined bool     // drop events whose myResponse is declined

	classifier *classifier // nil when ENTITIES is unset
}

// defaultPalette colors feeds that don't set their own color, by position in
// the feed list (wrapping around), so every calendar is told apart out of the
// box.
var defaultPalette = []string{
	"#5aa2f0", // blue
	"#57c46a", // green
	"#cf7fd1", // pink-purple
	"#e0a84a", // amber
	"#4fc1c9", // teal
	"#e0736b", // coral
}

// normalizeColor accepts hex colors with or without a leading "#" (or "0x"),
// which lets colors be set in env files where "#" would otherwise be parsed as
// the start of a comment and truncate the value. Non-hex values (named colors,
// rgb(), etc.) are left untouched.
func normalizeColor(c string) string {
	c = strings.TrimSpace(c)
	if c == "" {
		return c
	}
	c = strings.TrimPrefix(c, "0x")
	c = strings.TrimPrefix(c, "0X")
	if strings.HasPrefix(c, "#") {
		return c
	}
	if isHexColor(c) {
		return "#" + c
	}
	return c
}

func isHexColor(s string) bool {
	switch len(s) {
	case 3, 4, 6, 8:
	default:
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// multiPartTLDs are public suffixes made of two labels, so the registered
// domain is the three-label form (e.g. example.co.uk, not co.uk).
var multiPartTLDs = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true,
	"com.au": true, "net.au": true, "org.au": true,
	"co.nz": true, "co.jp": true, "com.br": true, "co.in": true,
	"gc.ca": true,
}

// apexDomain reduces a hostname to its registered/apex domain so link labels
// stay short (acme.harvestapp.com -> harvestapp.com).
func apexDomain(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	labels := strings.Split(host, ".")
	n := len(labels)
	if n <= 2 {
		return host
	}
	last2 := labels[n-2] + "." + labels[n-1]
	if multiPartTLDs[last2] {
		return labels[n-3] + "." + last2
	}
	return last2
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// winToIANA maps Microsoft Windows time zone names (used by Outlook/O365 ICS
// feeds) to IANA names that Go's time package can resolve.
var winToIANA = map[string]string{
	"Dateline Standard Time":          "Etc/GMT+12",
	"Hawaiian Standard Time":          "Pacific/Honolulu",
	"Alaskan Standard Time":           "America/Anchorage",
	"Pacific Standard Time":           "America/Los_Angeles",
	"Pacific Standard Time (Mexico)":  "America/Tijuana",
	"US Mountain Standard Time":       "America/Phoenix",
	"Mountain Standard Time":          "America/Denver",
	"Mountain Standard Time (Mexico)": "America/Chihuahua",
	"Central Standard Time":           "America/Chicago",
	"Central Standard Time (Mexico)":  "America/Mexico_City",
	"Canada Central Standard Time":    "America/Regina",
	"Eastern Standard Time":           "America/New_York",
	"Eastern Standard Time (Mexico)":  "America/Cancun",
	"US Eastern Standard Time":        "America/Indiana/Indianapolis",
	"Atlantic Standard Time":          "America/Halifax",
	"Newfoundland Standard Time":      "America/St_Johns",
	"SA Pacific Standard Time":        "America/Bogota",
	"SA Eastern Standard Time":        "America/Cayenne",
	"E. South America Standard Time":  "America/Sao_Paulo",
	"GMT Standard Time":               "Europe/London",
	"Greenwich Standard Time":         "Atlantic/Reykjavik",
	"W. Europe Standard Time":         "Europe/Berlin",
	"Central Europe Standard Time":    "Europe/Budapest",
	"Romance Standard Time":           "Europe/Paris",
	"Central European Standard Time":  "Europe/Warsaw",
	"W. Central Africa Standard Time": "Africa/Lagos",
	"GTB Standard Time":               "Europe/Bucharest",
	"E. Europe Standard Time":         "Europe/Chisinau",
	"South Africa Standard Time":      "Africa/Johannesburg",
	"Israel Standard Time":            "Asia/Jerusalem",
	"Arabic Standard Time":            "Asia/Baghdad",
	"Arab Standard Time":              "Asia/Riyadh",
	"Russian Standard Time":           "Europe/Moscow",
	"Arabian Standard Time":           "Asia/Dubai",
	"India Standard Time":             "Asia/Kolkata",
	"Bangladesh Standard Time":        "Asia/Dhaka",
	"SE Asia Standard Time":           "Asia/Bangkok",
	"China Standard Time":             "Asia/Shanghai",
	"Singapore Standard Time":         "Asia/Singapore",
	"W. Australia Standard Time":      "Australia/Perth",
	"Tokyo Standard Time":             "Asia/Tokyo",
	"Korea Standard Time":             "Asia/Seoul",
	"Cen. Australia Standard Time":    "Australia/Adelaide",
	"AUS Eastern Standard Time":       "Australia/Sydney",
	"New Zealand Standard Time":       "Pacific/Auckland",
	"UTC":                             "UTC",
}

// winTZReplacer does every winToIANA rewrite in one pass over the feed. Longer
// names go first because a Replacer tries its pairs in order: that way
// "Pacific Standard Time (Mexico)" is never cut short by "Pacific Standard
// Time" and left with a dangling " (Mexico)".
var winTZReplacer = func() *strings.Replacer {
	names := make([]string, 0, len(winToIANA))
	for win, iana := range winToIANA {
		if win != iana {
			names = append(names, win)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
	pairs := make([]string, 0, 4*len(names))
	for _, win := range names {
		pairs = append(pairs, "TZID:"+win, "TZID:"+winToIANA[win], "TZID="+win, "TZID="+winToIANA[win])
	}
	return strings.NewReplacer(pairs...)
}()

// remapWindowsTZ rewrites Windows TZID names to IANA equivalents in both the
// VTIMEZONE definitions (`TZID:Name`) and the property params (`TZID=Name`).
func remapWindowsTZ(ics string) string {
	return winTZReplacer.Replace(ics)
}

// meetRe matches a join URL for the major conferencing providers.
var meetRe = regexp.MustCompile(`https?://(?:teams\.microsoft\.com/l/meetup-join/[^\s<>"']+|[a-z0-9.-]*\.?meet\.google\.com/[a-z0-9-]+|meet\.google\.com/[a-z0-9-]+|[a-z0-9.-]*zoom\.us/(?:j|my|w)/[^\s<>"']+)`)

// extractMeet finds the first conferencing join URL across the event's location,
// URL property, and description, and labels which provider it is.
func extractMeet(location, url, description string) (string, string) {
	for _, src := range []string{location, url, description} {
		if m := meetRe.FindString(src); m != "" {
			switch {
			case strings.Contains(m, "teams.microsoft.com"):
				return m, "Teams"
			case strings.Contains(m, "meet.google.com"):
				return m, "Meet"
			case strings.Contains(m, "zoom.us"):
				return m, "Zoom"
			default:
				return m, "Join"
			}
		}
	}
	return "", ""
}

// parseLocation cleans a raw ICS LOCATION into a short display label, a full
// single-line address, and a Google Maps link. Returns empty strings for noise
// locations (conferencing labels and bare join URLs). Apple stores venue name
// and street address separated by an escaped newline (\n), which gocal leaves
// literal, so we unescape and split it here.
func parseLocation(raw string) (label, full, mapURL, kind string) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", "", "", ""
	}
	l := strings.ToLower(t)

	// Conferencing URLs are captured separately as the meet link, so drop them here.
	switch {
	case strings.Contains(l, "teams.microsoft.com"),
		strings.Contains(l, "microsoft teams"),
		strings.Contains(l, "zoom.us"),
		strings.Contains(l, "meet.google.com"):
		return "", "", "", ""
	}

	// A plain URL location (e.g. LOCATION:https://www.harvest.com) is a real link
	// the user wants. Label with the host, link straight to the URL.
	if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
		if u, err := url.Parse(t); err == nil && u.Hostname() != "" {
			return apexDomain(u.Hostname()), t, t, "link"
		}
		return t, t, t, "link"
	}

	// Unescape ICS escapes gocal left in (newlines), then split venue / address.
	t = strings.ReplaceAll(t, `\n`, "\n")
	t = strings.ReplaceAll(t, `\N`, "\n")
	t = strings.ReplaceAll(t, `\\`, `\`)

	var parts []string
	for _, ln := range strings.Split(t, "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "", "", "", ""
	}
	label = parts[0]
	full = strings.Join(parts, ", ")
	mapURL = "https://www.google.com/maps/search/?api=1&query=" + url.QueryEscape(full)
	return label, full, mapURL, "map"
}

// humanizeDuration renders a duration compactly: "30m", "1h", "1h30m", "2d3h".
func humanizeDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	h, m := mins/60, mins%60
	if h >= 24 {
		days, rem := h/24, h%24
		if rem == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, rem)
	}
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// stripMailto removes a leading, case-insensitive "mailto:" from an ICS CAL-ADDRESS.
func stripMailto(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 7 && strings.EqualFold(v[:7], "mailto:") {
		v = strings.TrimSpace(v[7:])
	}
	return v
}

// parseAttendees normalizes gocal attendees into a clean, deduped invitee list:
// CN as the name, the mailto: value as the email, and PARTSTAT lowercased as the
// RSVP status. Entries with neither a name nor an email are dropped.
func parseAttendees(in []gocal.Attendee) []Attendee {
	if len(in) == 0 {
		return nil
	}
	out := make([]Attendee, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, a := range in {
		name := strings.TrimSpace(a.Cn)
		email := stripMailto(a.Value)
		if name == "" && email == "" {
			continue
		}
		if email != "" {
			key := strings.ToLower(email)
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, Attendee{
			Name:   name,
			Email:  email,
			Status: strings.ToLower(strings.TrimSpace(a.Status)),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// organizerName returns the organizer's display name, falling back to the bare
// email address when no CN is present.
func organizerName(o *gocal.Organizer) string {
	if o == nil {
		return ""
	}
	if cn := strings.TrimSpace(o.Cn); cn != "" {
		return cn
	}
	return stripMailto(o.Value)
}

// guestsHdrRe / guestLineRe / guestEndRe parse the "Guests" roster that Google
// Calendar embeds as text in the DESCRIPTION of forwarded invites (published
// Outlook feeds strip the structured ATTENDEE properties, but this text block
// survives). A guest line looks like:  Sam Carter<mailto:scarter@example.com> - organizer
var (
	guestsHdrRe  = regexp.MustCompile(`(?i)^\s*guests\s*$`)
	guestLineRe  = regexp.MustCompile(`(?i)^\s*(.*?)<mailto:([^>]+)>\s*(?:-\s*(organizer|optional|required))?\s*$`)
	guestEndRe   = regexp.MustCompile(`(?i)^\s*(view all guest info|reply for )`)
	invitedByRe  = regexp.MustCompile(`(?i)invited by (.+?) to attend an event named`)
	descUnescape = strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`)
)

// parseDescriptionGuests extracts the organizer and the invitee roster from a
// Google-style "Guests" block in the DESCRIPTION. It operates on the raw text
// (no HTML stripping) so the literal <mailto:...> markers survive. Returns an
// empty organizer and nil attendees when no such block is present.
func parseDescriptionGuests(raw string) (organizer string, attendees []Attendee) {
	if !strings.Contains(strings.ToLower(raw), "<mailto:") {
		return "", nil
	}
	lines := strings.Split(descUnescape.Replace(raw), "\n")

	// Organizer fallback: the "You have been invited by <Name>" preamble line.
	if m := invitedByRe.FindStringSubmatch(raw); m != nil {
		organizer = strings.TrimSpace(m[1])
	}

	inGuests := false
	for _, ln := range lines {
		if guestsHdrRe.MatchString(ln) {
			inGuests = true
			continue
		}
		if !inGuests {
			continue
		}
		if guestEndRe.MatchString(ln) {
			break
		}
		m := guestLineRe.FindStringSubmatch(ln)
		if m == nil {
			// A blank line inside the block is skipped; anything else ends it.
			if strings.TrimSpace(ln) == "" {
				continue
			}
			break
		}
		name := strings.TrimSpace(m[1])
		email := strings.TrimSpace(m[2])
		role := strings.ToLower(m[3])
		if role == "" {
			role = "required"
		}
		// Google writes the address as the display text when there's no name.
		if strings.EqualFold(name, email) {
			name = ""
		}
		attendees = append(attendees, Attendee{Name: name, Email: email, Role: role})
		if role == "organizer" && email != "" {
			if name != "" {
				organizer = name
			} else if organizer == "" {
				organizer = email
			}
		}
	}
	return organizer, attendees
}

// mergeAttendees folds extra attendees (e.g. parsed from the DESCRIPTION) into a
// base list (e.g. from structured ATTENDEE properties), deduped by email. It
// never clobbers: existing non-empty fields win, and extras only fill blanks or
// append people not already present.
func mergeAttendees(base, extra []Attendee) []Attendee {
	idx := make(map[string]int, len(base))
	for i, a := range base {
		if a.Email != "" {
			idx[strings.ToLower(a.Email)] = i
		}
	}
	for _, e := range extra {
		key := strings.ToLower(e.Email)
		if key != "" {
			if i, ok := idx[key]; ok {
				if base[i].Name == "" {
					base[i].Name = e.Name
				}
				if base[i].Status == "" {
					base[i].Status = e.Status
				}
				if base[i].Role == "" {
					base[i].Role = e.Role
				}
				continue
			}
		}
		base = append(base, e)
		if key != "" {
			idx[key] = len(base) - 1
		}
	}
	return base
}

// seriesID derives a stable, opaque id from an ICS UID so every instance of one
// source event — each day of a multi-day all-day event, each occurrence of a
// recurring series — shares the same value. Empty UID yields "".
func seriesID(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return ""
	}
	sum := sha1.Sum([]byte(uid))
	return hex.EncodeToString(sum[:])[:12]
}

// civilDays returns the whole number of days between two local-midnight times.
// Rounding to whole days absorbs the ±1h drift when a DST boundary falls inside
// the span.
func civilDays(from, to time.Time) int {
	return int(to.Sub(from).Round(24*time.Hour) / (24 * time.Hour))
}

// meetLineRe matches a line that is nothing but a conferencing join URL (with
// optional surrounding markup/punctuation), so the agenda cleaner can drop it.
var meetLineRe = regexp.MustCompile(`(?i)^[\s<>"'|>-]*` + meetRe.String() + `[\s<>"'|]*$`)

// descNoiseRe matches a line that is pure conferencing boilerplate (Teams / Zoom
// / Google Meet join instructions, dial-ins, meeting IDs, passcodes). These are
// noise here because the join URL is already surfaced as meetUrl/meetKind.
var descNoiseRe = regexp.MustCompile(`(?i)^\s*(` + strings.Join([]string{
	`microsoft teams meeting`,
	`microsoft teams need help`,
	`join on your computer`,
	`click here to join the meeting`,
	`meeting id\s*:`,
	`passcode\s*:`,
	`phone conference id\s*:`,
	`video conference id\s*:`,
	`join with a video conferencing device`,
	`download teams`,
	`join on the web`,
	`learn more`,
	`meeting options`,
	`or call in \(audio only\)`,
	`find a local number`,
	`reset pin`,
	`join zoom meeting`,
	`.*is inviting you to a scheduled zoom meeting`,
	`one tap mobile`,
	`dial by your location`,
	`join with google meet`,
	`or dial\s*:`,
	`more phone numbers`,
	`learn more about meet`,
	`\(\$tel:`,
	// Newer Teams footer (Outlook 2024+ format).
	`need help\?`,
	`system reference\s*<`,
	`for organizers\s*:`,
	`join the meeting now`,
	`dial in by phone\s*$`,
	`join on a video conferencing device`,
	`tenant key\s*:`,
	`video id\s*:`,
	`more info\s*(<|$)`,
	// Google Calendar sync header and its meeting/phone block.
	`this email keeps the event up to date`,
	`set up inbox filters`,
	`meeting link\s*$`,
	`join by phone\s*$`,
	`meet\.google\.com/[a-z0-9-]+\s*$`,
	`\([a-z]{2}\)\s*\+\d[\d\s().-]*$`,
	`pin\s*:\s*[\d#\s]+$`,
	// Image placeholders Outlook leaves for inline pictures (logos, signatures).
	`\[(https?://[^\]]*|[^\]]*\b(logo|image)\b[^\]]*)\]\s*$`,
}, `|`) + `)`)

// htmlTagRe strips HTML tags from descriptions that ship as HTML (some Outlook
// feeds do). gmeetRe and teamsRuleRe identify the delimiter lines that bracket
// the Google Meet and Teams boilerplate blocks respectively.
var (
	htmlTagRe   = regexp.MustCompile(`<[^>]+>`)
	gmeetRe     = regexp.MustCompile(`^\s*-::~`)
	teamsRuleRe = regexp.MustCompile(`^\s*_{5,}\s*$`)
)

// googleTailRe marks the start of the Google Calendar invite footer that
// forwarded invites append to the DESCRIPTION (the "invited by … to attend an
// event named" preamble, the Guests roster, or the "Invitation from Google
// Calendar" sign-off). Everything from that point on is boilerplate — the
// attendees are extracted separately by parseDescriptionGuests — so the agenda
// cleaner truncates there.
var googleTailRe = regexp.MustCompile(`(?i)(invited by .+ to attend an event named|^\s*guests\s*$|^\s*invitation from google calendar)`)

// disclaimerRe marks the start of a corporate email disclaimer ("This email
// message is for the sole use of the intended recipient(s) ..."). Mail servers
// stamp these at the very bottom, so the cleaner truncates there, which also
// catches disclaimers that wrap onto several lines.
var disclaimerRe = regexp.MustCompile(`(?i)^\W*(this (e-?mail|message|communication|transmission)\b.*\b(confidential|privileged|sole use|intended recipient)|confidentiality notice|disclaimer\s*:)`)

// googleMarkRe spots a line that only a Google Calendar invite would carry.
// Once one is seen, a bare "When" line is the start of the schedule tail
// ("When / Weekly from 8:30am ...") and the cleaner truncates there too. The
// guard keeps a lone "When" in an ordinary agenda from cutting it short.
var (
	googleMarkRe = regexp.MustCompile(`(?i)^\s*(this email keeps the event up to date|meeting link\s*$|join by phone\s*$|join with google meet)`)
	googleWhenRe = regexp.MustCompile(`(?i)^\s*when\s*$`)
)

// htmlEntities decodes the handful of entities that show up in calendar
// descriptions once tags are stripped.
var htmlEntities = strings.NewReplacer(
	"&nbsp;", " ",
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&#39;", "'",
)

// cleanDescription turns a raw ICS DESCRIPTION into readable agenda text: it
// unescapes ICS escapes, de-tags HTML, removes the delimited Teams/Google Meet
// boilerplate blocks and any stray conferencing lines, then collapses blank runs.
// Returns "" when nothing meaningful remains.
func cleanDescription(raw string) string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return ""
	}

	// Unescape ICS escapes gocal left literal.
	t = strings.ReplaceAll(t, `\n`, "\n")
	t = strings.ReplaceAll(t, `\N`, "\n")
	t = strings.ReplaceAll(t, `\,`, ",")
	t = strings.ReplaceAll(t, `\;`, ";")
	t = strings.ReplaceAll(t, `\\`, `\`)

	// Some feeds ship HTML descriptions; flatten them to text.
	if strings.Contains(t, "</") || strings.Contains(t, "/>") || strings.Contains(t, "&lt;") || strings.Contains(t, "&nbsp;") {
		t = htmlTagRe.ReplaceAllString(t, "")
		t = htmlEntities.Replace(t)
	}

	lines := strings.Split(t, "\n")
	kept := make([]string, 0, len(lines))
	skipGMeet := false // inside a Google Meet -::~ ... -::~ block
	teamsRule := 0     // length of the rule that opened a Teams block; 0 = not in one
	googleInvite := false
	for _, ln := range lines {
		trimmed := strings.TrimRight(ln, " \t\r")

		// Google invite footer (guest list, RSVP links, dial-ins) runs to the end
		// of the description; drop it and everything after.
		if googleTailRe.MatchString(trimmed) || disclaimerRe.MatchString(trimmed) {
			break
		}
		if googleMarkRe.MatchString(trimmed) {
			googleInvite = true
		}
		if googleInvite && googleWhenRe.MatchString(trimmed) {
			break
		}

		// Google Meet block: delimiter lines bracket it; drop them and everything between.
		if gmeetRe.MatchString(trimmed) {
			skipGMeet = !skipGMeet
			continue
		}
		if skipGMeet {
			continue
		}

		// Teams block: a horizontal underscore rule brackets the appended block.
		// Newer footers also put shorter rules inside the block (between the join
		// info and the dial-in / "Need help?" section), so only a rule at least as
		// long as the opener closes it. With no closing rule it runs to the end.
		if teamsRuleRe.MatchString(trimmed) {
			n := len(strings.TrimSpace(trimmed))
			switch {
			case teamsRule == 0:
				teamsRule = n
			case n >= teamsRule:
				teamsRule = 0
			}
			continue
		}
		if teamsRule > 0 {
			continue
		}

		if descNoiseRe.MatchString(trimmed) || meetLineRe.MatchString(trimmed) {
			continue
		}
		kept = append(kept, trimmed)
	}

	// Collapse runs of blank lines and trim.
	var b strings.Builder
	blank := true // leading blanks suppressed
	for _, ln := range kept {
		if strings.TrimSpace(ln) == "" {
			if blank {
				continue
			}
			blank = true
			b.WriteByte('\n')
			continue
		}
		blank = false
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// stripTitleEcho drops a leading line that just repeats the event name. Google
// invites open the DESCRIPTION with the title, so after the boilerplate is gone
// the agenda is often the title alone, or the title followed by the real body.
func stripTitleEcho(agenda, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return agenda
	}
	first, rest, _ := strings.Cut(agenda, "\n")
	if !strings.EqualFold(strings.TrimSpace(first), name) {
		return agenda
	}
	return strings.TrimSpace(rest)
}

// parseDateLocal turns a raw "YYYYMMDD" all-day value into local midnight.
// Falls back to the parser's time (date-portion only) if the raw value is empty.
func parseDateLocal(raw string, fallback *time.Time, loc *time.Location) time.Time {
	if d, err := time.ParseInLocation("20060102", raw, loc); err == nil {
		return d
	}
	if fallback != nil {
		f := fallback.UTC()
		return time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc)
	}
	return time.Time{}
}

// store holds the most recent merged payload. Requests are served from here
// instantly; the upstream fetch happens on a background timer, never on the
// request path, so response headers always return immediately.
type store struct {
	mu      sync.RWMutex
	resp    Response
	payload []byte
	updated time.Time
	ready   bool

	version    uint64 // bumps on every refresh and re-tag, so pages know when they're stale
	refreshing bool   // a feed refresh is running right now

	kick chan struct{} // asks the refresher to run now (e.g. the UI's refresh button)
}

// stamp returns the data version and whether a refresh is running.
func (s *store) stamp() (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version, s.refreshing
}

func (s *store) setRefreshing(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshing = on
}

// reclassify re-tags the cached events without fetching any feed, so a
// correction shows up in milliseconds instead of after a full refresh. The
// write lock keeps a concurrent refresh from being overwritten with older data.
func (s *store) reclassify(c *classifier, loc *time.Location) {
	if c == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return
	}
	evs := make([]Event, len(s.resp.Events))
	copy(evs, s.resp.Events)
	c.classify(evs)
	resp := s.resp
	resp.Events = evs
	resp.Days = groupDays(evs, time.Now().In(loc), loc)
	resp.Entities = c.Entities()
	b, err := json.Marshal(resp)
	if err != nil {
		log.Printf("reclassify marshal error: %v", err)
		return
	}
	s.resp, s.payload = resp, b
	s.version++
}

// refreshSoon asks the background refresher to run without waiting for the
// next tick. Never blocks; a kick already pending covers this one.
func (s *store) refreshSoon() {
	if s.kick == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *store) get() (Response, []byte, bool, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resp, s.payload, s.ready, s.updated
}

func (s *store) set(resp Response, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resp = resp
	s.payload = b
	s.updated = time.Now()
	s.ready = true
	s.version++
}

// authOK reports whether the request carries a valid "Authorization: Bearer
// <token>" matching the configured token. An empty configured token never
// matches, so tunnel traffic is rejected until AUTH_TOKEN is set.
func authOK(r *http.Request, token string) bool {
	return bearerOK(r.Header, token)
}

// bearerOK is authOK for a bare header set (MCP tool handlers get headers, not
// the request).
func bearerOK(hdr http.Header, token string) bool {
	if token == "" {
		return false
	}
	const prefix = "Bearer "
	h := hdr.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// requireTunnelAuth enforces the bearer token on tunnel traffic, or on every
// request when require_auth is on. Cloudflare stamps every tunnel-proxied
// request with Cf-Connecting-Ip (and Cf-Ray); internal Docker requests
// (Glance) never carry them, which is why they pass without a token by
// default. That only holds while nothing else can reach the port, so
// require_auth is there for any other setup (another reverse proxy, a
// published port). Fail closed: when a token is needed, a missing/empty
// AUTH_TOKEN rejects the request.
func requireTunnelAuth(cur func() *config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := cur()
		viaTunnel := r.Header.Get("Cf-Connecting-Ip") != "" || r.Header.Get("Cf-Ray") != ""
		if (viaTunnel || c.requireAuth) && !authOK(r, c.authToken) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusWriter remembers the status a handler answered with. It passes
// Flush through and unwraps, so /mcp event streams keep working.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logFailures logs every request the API turns away (401 bad token, 404
// expired MCP session, ...). Successful requests stay quiet. Without this a
// client saying "the calendar wasn't available" leaves no trace here to say
// why. Only the method, path and a few yes/no facts are logged, never
// headers or tokens.
func logFailures(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status >= 400 {
			log.Printf("%s %s -> %d (tunnel=%t bearer=%t mcp-session=%t)", r.Method, r.URL.Path, sw.status,
				r.Header.Get("Cf-Connecting-Ip") != "" || r.Header.Get("Cf-Ray") != "",
				r.Header.Get("Authorization") != "", r.Header.Get("Mcp-Session-Id") != "")
		}
	})
}

// parseDays interprets the ?days=N query value as a positive day count, capped
// at the configured lookahead window. Returns ok=false for empty/invalid values
// so the caller falls back to the full payload.
func parseDays(raw string, maxAhead int) (int, bool) {
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, false
	}
	if limit := maxAhead + 1; n > limit {
		n = limit
	}
	return n, true
}

// filterDays returns a copy of resp limited to day-groups whose date falls in
// [today, today+n-1] (local tz), with the flat Events list and Count rebuilt
// from the kept days. Lookback/past days are excluded when this is applied.
func filterDays(resp Response, n int, loc *time.Location) Response {
	now := time.Now().In(loc)
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	until := from.AddDate(0, 0, n) // exclusive

	days := make([]Day, 0, len(resp.Days))
	events := make([]Event, 0, len(resp.Events))
	for _, d := range resp.Days {
		t, err := time.ParseInLocation("2006-01-02", d.Date, loc)
		if err != nil || t.Before(from) || !t.Before(until) {
			continue
		}
		days = append(days, d)
		events = append(events, d.Events...)
	}

	resp.Days = days
	resp.Events = events
	resp.Count = len(events)
	return resp
}

func main() {
	printCfg := flag.Bool("print-config", false, "print the effective config (file + env) as TOML and exit; AUTH_TOKEN and LISTEN are left out")
	envFile := flag.String("env-file", "", "load KEY=VALUE lines from this file into the environment first (read the way Docker reads an env file)")
	seed := flag.String("seed", "", "dev: serve a saved /events payload instead of fetching feeds (no background refresh)")
	flag.Parse()
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			log.Fatalf("env file: %v", err)
		}
	}

	cfgPath := envStr("CONFIG_FILE", "/config/calmerge.toml")
	if *printCfg {
		cfg, err := loadConfig(cfgPath, nil)
		if err != nil {
			log.Fatalf("config error: %v", err)
		}
		if err := printConfig(os.Stdout, cfg); err != nil {
			log.Fatalf("print config: %v", err)
		}
		return
	}
	cfgSum := fileSum(cfgPath) // before loading, so an edit in between still reloads
	cfg, err := loadConfig(cfgPath, nil)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		log.Printf("config file %s (hot-reloaded; env vars fill whatever it leaves out)", cfgPath)
	} else {
		log.Printf("no config file at %s: using env vars (it's picked up if created later)", cfgPath)
	}
	log.Printf("calmerge starting on %s | %d feeds | window -%dd..+%dd | refresh %s",
		cfg.listen, len(cfg.feeds), cfg.lookbackDays, cfg.aheadDays, cfg.cacheTTL)
	if cfg.classifier != nil {
		log.Printf("entity classifier on: %d entities, %d corrections, %d remembered lessons",
			len(cfg.classifier.entities), cfg.classifier.fixes.len(), cfg.classifier.memory.len())
	}
	switch {
	case cfg.requireAuth && cfg.authToken == "":
		log.Printf("require_auth is on but AUTH_TOKEN is not set: every /events and /mcp request will be rejected with 401")
	case cfg.requireAuth:
		log.Printf("require_auth is on: every /events and /mcp request must present the bearer token")
	case cfg.authToken == "":
		log.Printf("AUTH_TOKEN is not set: tunnel traffic (Cloudflare-proxied) will be rejected with 401; only internal requests are served")
	default:
		log.Printf("AUTH_TOKEN is set: tunnel traffic must present a matching bearer token")
	}

	// The live config. The watcher swaps in a new one when the file changes;
	// everything reads through cur so edits apply without a restart.
	var cur atomic.Pointer[config]
	cur.Store(&cfg)
	st := &store{kick: make(chan struct{}, 1)}
	go watchConfig(context.Background(), cfgPath, cfgSum, configPollInterval, &cur, st)

	// Seed a valid empty payload so /events answers instantly before the first
	// refresh completes, rather than returning an empty body.
	seedResp := Response{
		Generated: time.Now().In(cfg.loc).Format(time.RFC3339),
		Days:      []Day{},
		Events:    []Event{},
	}
	if seed, err := json.Marshal(seedResp); err == nil {
		st.resp = seedResp
		st.payload = seed
	}

	if *seed != "" {
		if err := loadSeed(*seed, cfg, st); err != nil {
			log.Fatalf("seed: %v", err)
		}
		log.Printf("dev: serving %s, feeds are not fetched", *seed)
	}

	// Background refresher: fetch + merge on a ticker (or when kicked by the
	// refresh button or a config reload), push into the store.
	go func() {
		if *seed != "" {
			for range st.kick { // nothing to fetch; act out a refresh for the UI
				st.setRefreshing(true)
				time.Sleep(1500 * time.Millisecond)
				st.setRefreshing(false)
			}
			return
		}
		feedsSeen := &feedCache{}
		refresh := func() {
			c := cur.Load()
			start := time.Now()
			st.setRefreshing(true)
			defer st.setRefreshing(false)
			resp := build(*c, &http.Client{Timeout: c.httpTimeout}, feedsSeen)
			b, err := json.Marshal(resp)
			if err != nil {
				log.Printf("refresh marshal error: %v", err)
				return
			}
			st.set(resp, b)
			log.Printf("refreshed in %s: %d events / %d days (%d feed errors)",
				time.Since(start).Round(time.Millisecond), resp.Count, len(resp.Days), len(resp.Errors))
		}
		refresh() // prime immediately on startup
		interval := cur.Load().cacheTTL
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
			case <-st.kick:
			}
			refresh()
			if next := cur.Load().cacheTTL; next != interval {
				interval = next
				t.Reset(interval)
			}
		}
	}()

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	http.Handle("/events", logFailures(requireTunnelAuth(cur.Load, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := cur.Load()
		resp, body, ready, updated := st.get()

		// ?days=N trims the payload to the next N calendar days (today inclusive),
		// keeping the briefing call lean. Without a valid param we serve the cached
		// bytes directly (fast path, no re-marshal).
		if n, ok := parseDays(r.URL.Query().Get("days"), c.aheadDays); ok {
			resp = filterDays(resp, n, c.loc)
			if b, err := json.Marshal(resp); err == nil {
				body = b
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if ready {
			w.Header().Set("X-Updated", updated.Format(time.RFC3339))
		} else {
			w.Header().Set("X-Warming", "true")
		}
		_, _ = w.Write(body)
	}))))

	http.Handle("/mcp", logFailures(requireTunnelAuth(cur.Load, newMCPHandler(cur.Load, st))))
	newUIServer(cur.Load, st).routes(http.DefaultServeMux)

	// No write timeout: /mcp holds event streams open. The header and idle
	// timeouts are what stop a slow or stalled client from pinning a
	// connection forever.
	srv := &http.Server{
		Addr:              cfg.listen,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Fatal(srv.ListenAndServe())
}

// groupDays buckets start-sorted events into day groups, labelling today and
// tomorrow.
func groupDays(all []Event, now time.Time, loc *time.Location) []Day {
	today := now.Format("2006-01-02")
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")

	var days []Day
	idx := map[string]int{}
	for _, e := range all {
		i, ok := idx[e.Date]
		if !ok {
			t, _ := time.ParseInLocation("2006-01-02", e.Date, loc)
			label := t.Format("Mon, Jan 2")
			switch e.Date {
			case today:
				label = "Today \u00b7 " + label
			case tomorrow:
				label = "Tomorrow \u00b7 " + label
			}
			days = append(days, Day{Label: label, Date: e.Date})
			i = len(days) - 1
			idx[e.Date] = i
		}
		days[i].Events = append(days[i].Events, e)
	}
	return days
}

// A failed feed fetch is tried once more after feedRetryDelay, since most
// failures are a blip on the calendar host. If that fails too, the feed's
// last good events stand in for up to maxStaleFeed, so one bad fetch doesn't
// empty a calendar until the next refresh.
var feedRetryDelay = 2 * time.Second

const maxStaleFeed = 24 * time.Hour

// feedCache remembers each feed's last good events. A nil cache keeps
// nothing: a failed feed then just drops out of the payload.
type feedCache struct {
	mu sync.Mutex
	m  map[string]cachedFeed // by feed name + URL
}

type cachedFeed struct {
	events []Event
	at     time.Time
}

func feedKey(f Feed) string { return f.Name + "\x00" + f.URL }

// put saves a feed's freshly fetched events.
func (fc *feedCache) put(f Feed, evs []Event, at time.Time) {
	if fc == nil {
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.m == nil {
		fc.m = map[string]cachedFeed{}
	}
	fc.m[feedKey(f)] = cachedFeed{events: evs, at: at}
}

// get returns a feed's last good events, brought up to date for now: days
// that have left the window (it starts at windowStart) are dropped and the
// past/ongoing flags are worked out again. ok is false when there is nothing
// saved or it is older than maxStaleFeed.
func (fc *feedCache) get(f Feed, now, windowStart time.Time) (evs []Event, at time.Time, ok bool) {
	if fc == nil {
		return nil, time.Time{}, false
	}
	fc.mu.Lock()
	c, found := fc.m[feedKey(f)]
	fc.mu.Unlock()
	if !found || now.Sub(c.at) > maxStaleFeed {
		return nil, time.Time{}, false
	}
	first := windowStart.Format("2006-01-02")
	evs = make([]Event, 0, len(c.events))
	for _, ev := range c.events {
		if ev.Date < first {
			continue
		}
		setProgress(&ev, now)
		evs = append(evs, ev)
	}
	return evs, c.at, true
}

// keep forgets feeds that are no longer configured.
func (fc *feedCache) keep(feeds []Feed) {
	if fc == nil {
		return
	}
	want := make(map[string]bool, len(feeds))
	for _, f := range feeds {
		want[feedKey(f)] = true
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for k := range fc.m {
		if !want[k] {
			delete(fc.m, k)
		}
	}
}

// setProgress sets a timed event's past/ongoing flags for now, from its start
// and end. All-day entries carry neither flag.
func setProgress(ev *Event, now time.Time) {
	if ev.AllDay {
		return
	}
	st, err := time.Parse(time.RFC3339, ev.Start)
	if err != nil {
		return
	}
	en, err := time.Parse(time.RFC3339, ev.End)
	hasEnd := ev.End != "" && err == nil
	ev.Ongoing = st.Before(now) && hasEnd && en.After(now)
	if hasEnd {
		ev.Past = !en.After(now) // ended at or before now
	} else {
		ev.Past = st.Before(now)
	}
}

func build(cfg config, cl *http.Client, fc *feedCache) Response {
	now := time.Now().In(cfg.loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, cfg.loc).
		AddDate(0, 0, -cfg.lookbackDays)
	end := start.AddDate(0, 0, cfg.lookbackDays+cfg.aheadDays)

	type result struct {
		events []Event
		err    string
	}
	results := make([]result, len(cfg.feeds))
	var wg sync.WaitGroup

	for i, f := range cfg.feeds {
		wg.Add(1)
		go func(i int, f Feed) {
			defer wg.Done()
			evs, err := fetchFeed(cl, f, start, end, cfg.loc, now, cfg.feedOptions())
			if err != nil {
				time.Sleep(feedRetryDelay)
				evs, err = fetchFeed(cl, f, start, end, cfg.loc, now, cfg.feedOptions())
			}
			if err == nil {
				fc.put(f, evs, now)
				results[i] = result{events: evs}
				return
			}
			if old, at, ok := fc.get(f, now, start); ok {
				results[i] = result{events: old, err: fmt.Sprintf("%s: %v; showing its events as of %s", f.Name, err, at.Format("Jan 2 3:04pm"))}
				return
			}
			results[i] = result{err: fmt.Sprintf("%s: %v", f.Name, err)}
		}(i, f)
	}
	wg.Wait()
	fc.keep(cfg.feeds)

	var all []Event
	var errs []string
	for _, r := range results {
		if r.err != "" {
			errs = append(errs, r.err)
		}
		all = append(all, r.events...)
	}
	cfg.classifier.classify(all)

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Start == all[j].Start {
			return all[i].Source < all[j].Source
		}
		return all[i].Start < all[j].Start
	})

	return Response{
		Generated: now.Format(time.RFC3339),
		Count:     len(all),
		Days:      groupDays(all, now, cfg.loc),
		Events:    all,
		Entities:  cfg.classifier.Entities(),
		Errors:    errs,
	}
}

// maxFeedBytes caps how much of one feed is read. Real calendars are a few MB
// at most; the cap keeps a broken or hostile URL from filling memory.
const maxFeedBytes = 32 << 20

// isCancelled reports whether a feed still carries a meeting that was called
// off. Cancelled meetings aren't deleted from the attendee's calendar: Google
// sends STATUS:CANCELLED (on the whole event, or on one RECURRENCE-ID override
// that gocal swaps in for that occurrence), and Outlook keeps the item until
// someone clicks "Remove from calendar", titled "Canceled: <subject>".
func isCancelled(e gocal.Event) bool {
	if strings.EqualFold(strings.TrimSpace(e.Status), "CANCELLED") {
		return true
	}
	title := strings.ToLower(strings.TrimSpace(e.Summary))
	return strings.HasPrefix(title, "canceled:") || strings.HasPrefix(title, "cancelled:")
}

// hideURL drops the URL that net/http and net/url put in their errors. Feed
// URLs are secrets (the address is the password), and fetch errors are served
// in the errors list of /events and /mcp, so only the cause may pass through.
func hideURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s failed: %w", strings.ToLower(ue.Op), ue.Err)
	}
	return err
}

// feedOptions are the config settings that shape how a feed is parsed.
type feedOptions struct {
	includeAttendees bool
	includeAgenda    bool
	self             map[string]bool // lowercased addresses that are "me"
	skipDeclined     bool
}

// feedOptions collects the parse settings from c.
func (c config) feedOptions() feedOptions {
	o := feedOptions{
		includeAttendees: c.includeAttendees,
		includeAgenda:    c.includeAgenda,
		skipDeclined:     c.skipDeclined,
	}
	if len(c.selfEmails) > 0 {
		o.self = make(map[string]bool, len(c.selfEmails))
		for _, e := range c.selfEmails {
			o.self[e] = true
		}
	}
	return o
}

// transpRE finds TRANSP property lines. gocal drops the standard properties it
// doesn't model, TRANSP among them, but keeps every X- property, so TRANSP is
// renamed to transpKey before parsing.
var transpRE = regexp.MustCompile(`(?im)^TRANSP([;:])`)

const transpKey = "X-CALMERGE-TRANSP"

// myResponse is the calendar owner's RSVP: the PARTSTAT of the first
// structured attendee whose address is in self. Only structured ATTENDEE
// lines carry PARTSTAT, so the DESCRIPTION roster can't answer this.
func myResponse(atts []Attendee, self map[string]bool) string {
	for _, a := range atts {
		if a.Email != "" && self[strings.ToLower(a.Email)] {
			return a.Status
		}
	}
	return ""
}

func fetchFeed(cl *http.Client, f Feed, start, end time.Time, loc *time.Location, now time.Time, opt feedOptions) ([]Event, error) {
	req, err := http.NewRequest(http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, hideURL(err)
	}
	if f.User != "" || f.Pass != "" {
		req.SetBasicAuth(f.User, f.Pass)
	}
	req.Header.Set("User-Agent", "calmerge/1.0")
	res, err := cl.Do(req)
	if err != nil {
		return nil, hideURL(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxFeedBytes+1))
	if err != nil {
		return nil, hideURL(err)
	}
	if len(raw) > maxFeedBytes {
		return nil, fmt.Errorf("feed is larger than %d MB", maxFeedBytes>>20)
	}

	// Microsoft/O365 feeds use Windows zone names (e.g. "Eastern Standard Time")
	// that gocal can't resolve, so it silently falls back to UTC and the times
	// come out hours off. Rewrite known Windows TZIDs to IANA names first.
	ics := transpRE.ReplaceAllString(remapWindowsTZ(string(raw)), transpKey+"$1")
	p := gocal.NewParser(strings.NewReader(ics))
	p.Start, p.End = &start, &end
	if err := p.Parse(); err != nil {
		return nil, err
	}

	out := make([]Event, 0, len(p.Events))
	for _, e := range p.Events {
		if e.Start == nil || isCancelled(e) {
			continue
		}
		allDay := strings.EqualFold(e.RawStart.Params["VALUE"], "DATE")

		// All-day (VALUE=DATE) values are floating dates, not instants. Pin them
		// to local midnight of the given calendar date instead of converting the
		// UTC-midnight gocal produces, which would shift the event a day earlier.
		var st, en time.Time
		hasEnd := e.End != nil
		if allDay {
			st = parseDateLocal(e.RawStart.Value, e.Start, loc)
			if hasEnd {
				en = parseDateLocal(e.RawEnd.Value, e.End, loc)
			}
		} else {
			st = e.Start.In(loc)
			if hasEnd {
				en = e.End.In(loc)
			}
		}
		meetURL, meetKind := extractMeet(e.Location, e.URL, e.Description)
		locLabel, locFull, mapURL, locKind := parseLocation(e.Location)

		// Fields shared by every entry this event produces.
		base := Event{
			Name:     strings.TrimSpace(e.Summary),
			Location: locLabel,
			Address:  locFull,
			MapURL:   mapURL,
			LocKind:  locKind,
			MeetURL:  meetURL,
			MeetKind: meetKind,
			SeriesId: seriesID(e.Uid),
			Source:   f.Name,
			Color:    f.Color,
		}
		// Structured ATTENDEE properties (real RSVP status) are the base; the
		// Google "Guests" roster in the DESCRIPTION (name + role, common on
		// published Outlook feeds that strip ATTENDEE) fills gaps and adds
		// anyone missing, without clobbering the structured data.
		structured := parseAttendees(e.Attendees)
		base.MyResponse = myResponse(structured, opt.self)
		if opt.skipDeclined && base.MyResponse == "declined" {
			continue
		}
		base.Status = strings.ToLower(strings.TrimSpace(e.Status))
		base.ShowAs = strings.ToLower(strings.TrimSpace(e.CustomAttributes["X-MICROSOFT-CDO-BUSYSTATUS"]))
		base.Transparent = strings.EqualFold(strings.TrimSpace(e.CustomAttributes[transpKey]), "TRANSPARENT")

		descOrg, guests := parseDescriptionGuests(e.Description)
		attendees := mergeAttendees(structured, guests)
		organizer := organizerName(e.Organizer)
		if organizer == "" {
			organizer = descOrg
		}
		agenda := stripTitleEcho(cleanDescription(e.Description), base.Name)
		if opt.includeAttendees {
			base.Attendees = attendees
			base.Organizer = organizer
		}
		if opt.includeAgenda {
			base.Agenda = agenda
		}
		// The classifier sees everything, even fields trimmed from the payload.
		base.sig = buildSignals(f.Name, base.Name, agenda, locFull, e.Categories, organizer, attendees, meetURL)
		base.Categories = base.sig.categories

		if allDay {
			// A VALUE=DATE event can span several days (DTEND is exclusive: a
			// single-day event has end = start+1 day). Emit one entry per day it
			// covers so a multi-day event renders on every day it spans, and clamp
			// to the visible window [start, end] so past days drop off (and an
			// over-long span can't blow up the loop).
			last := st.AddDate(0, 0, 1)
			if hasEnd && en.After(st) {
				last = en
			}
			// True span, derived from the source event before any window clipping.
			// DTEND is exclusive, so the inclusive last day is last - 1 day.
			base.StartDate = st.Format("2006-01-02")
			base.EndDate = last.AddDate(0, 0, -1).Format("2006-01-02")
			base.SpanDays = civilDays(st, last)
			base.MultiDay = base.SpanDays > 1
			from := st
			if start.After(from) {
				from = start
			}
			for d := from; d.Before(last) && !d.After(end); d = d.AddDate(0, 0, 1) {
				ev := base
				ev.Start = d.Format(time.RFC3339)
				ev.Date = d.Format("2006-01-02")
				ev.AllDay = true
				ev.DayIndex = civilDays(st, d) + 1
				out = append(out, ev)
			}
			continue
		}

		// Timed event: a single entry pinned to its start instant.
		ev := base
		ev.Start = st.Format(time.RFC3339)
		ev.Date = st.Format("2006-01-02")
		if hasEnd {
			ev.Duration = humanizeDuration(en.Sub(st))
			ev.End = en.Format(time.RFC3339)
		}
		setProgress(&ev, now)
		out = append(out, ev)
	}
	return out, nil
}
