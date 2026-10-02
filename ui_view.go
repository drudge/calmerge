package main

import (
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// View models for the review UI templates. Everything the templates show is
// computed here, so the templates stay free of logic.

const (
	agendaSpanDays = 7  // an agenda page covers a week
	weekHourPx     = 88 // week grid: pixels per hour
	weekMinBlockPx = 36 // a 15-minute meeting still gets a full line of title
	weekIndentPx   = 44 // how far a meeting drawn over a longer one is pushed right
	minGapMinutes  = 15 // shorter gaps between meetings aren't worth a divider
)

var uiFuncs = template.FuncMap{
	"mul":    func(a, b int) int { return a * b },
	"plural": plural,
	// skelWidths are the title widths of skeleton rows: varied, so the
	// placeholder reads like a real list.
	"skelWidths": func() []int { return []int{300, 220, 380, 260, 330, 240} },
}

// pageQuery is everything a view URL can say.
type pageQuery struct {
	Kind   string // agenda | week
	From   string // YYYY-MM-DD; empty = today
	Filter string // "" | "guesses"
	Client string
	Sel    string // selected seriesId
	On     string // date of the selected instance
	Open   bool   // agenda: today's finished meetings unfolded
}

func parseQuery(kind string, q url.Values) pageQuery {
	return pageQuery{
		Kind: kind, From: q.Get("from"), Filter: q.Get("filter"),
		Client: q.Get("client"), Sel: q.Get("sel"), On: q.Get("on"),
		Open: q.Get("earlier") == "1",
	}
}

// url renders the query, applying edits to a copy first.
func (q pageQuery) url(edit func(*pageQuery)) string {
	if edit != nil {
		edit(&q)
	}
	v := url.Values{}
	for k, s := range map[string]string{"from": q.From, "filter": q.Filter, "client": q.Client, "sel": q.Sel, "on": q.On} {
		if s != "" {
			v.Set(k, s)
		}
	}
	if q.Open && q.Kind == "agenda" {
		v.Set("earlier", "1")
	}
	u := "/ui/" + q.Kind
	if len(v) > 0 {
		u += "?" + v.Encode()
	}
	return u
}

type entityOpt struct {
	Name     string
	Color    template.CSS
	Selected bool
}

type statusView struct {
	Warming, Syncing, JustNow bool
	Updated                   string
	Stamp                     string
	PollEvery                 string
	FeedErrors                int
}

type pageView struct {
	Kind              string
	IsAgenda, IsWeek  bool
	Self              string
	AgendaURL         string
	WeekURL           string
	Status            statusView
	Warming           bool
	Tagging           bool
	GuessCount        int
	GuessesOn         bool // the guess queue: every upcoming guess, not one page of dates
	AllURL            string
	GuessesURL        string
	GuessesToggleURL  string // phones: one chip that turns the filter on and off
	ClientClearURL    string
	Client            string
	Clients           []clientOpt
	RangeLabel        string
	PrevURL, NextURL  string
	TodayURL          string
	IsCurrent         bool
	Agenda            *agendaView
	Week              *weekView
	Panel             *panelView
	Corrections       []correctionRow
	Toast             *toastView
	EmptyFilterResult bool
}

type clientOpt struct {
	Name, URL string
	Color     template.CSS
	Selected  bool
}

// eventRow is one meeting instance as the lists and grid show it.
type eventRow struct {
	ID, SeriesID, Date string
	Time, Dur, Title   string
	AllDay             bool
	AllDayNote         string
	Cal                string
	CalColor           template.CSS
	MeetURL, MeetKind  string
	People             string
	HasAgenda          bool
	LinkLabel, LinkURL string
	MapLabel, MapURL   string
	Tagged             bool
	OOO                bool   // your own time off (all-day OOO/PTO/vacation)
	AwayWho            string // someone else's "Name - Vacation Alert"
	OOONote            string // "Oct 7 – 9 · day 1 of 3 · back Mon Oct 12"
	Entity             string
	EntityColor        template.CSS
	Via, ViaLabel      string
	Guess, You         bool
	Why                string
	ConfirmVals        string
	SelectURL          string
	Selected           bool
	Just               bool
	Past               bool
	Delay              int
}

type agendaItem struct {
	Gap            bool
	GapTimes       string
	GapLen         string
	Row            eventRow
	FirstInSection bool
}

type agendaDay struct {
	Label, Date string
	OOO         []eventRow
	Away        []eventRow
	Count       string
	Items       []agendaItem
}

type earlierView struct {
	Open             bool
	OpenURL, FoldURL string
	Label            string
	Count            int
	Titles           string
	Span             string
	Guesses          string
	Dots             []template.CSS
	Items            []agendaItem
}

type agendaView struct {
	Earlier *earlierView
	Days    []agendaDay
}

type weekBlock struct {
	Row       eventRow
	Top       int
	Height    int
	Left      template.CSS
	Lines     int
	ShowTime  bool
	TimeLabel string
}

type weekCol struct {
	DOW, DateLabel, Date string
	IsToday              bool
	OOO                  bool
	Blocks               []weekBlock
	HasNow               bool
	NowTop               int
}

type hourMark struct {
	Label string
	Top   int
}

type allDayBar struct {
	Row          eventRow
	Col, Span    int
	Lane         int
	GridColumn   template.CSS
	GridRow      template.CSS
	ContinuesPre bool
}

type weekView struct {
	Cols        []weekCol
	Hours       []hourMark
	Height      int
	ColTemplate template.CSS
	AllDay      []allDayBar
	AllDayLanes int
}

type peopleLine struct {
	Who, Domain string
	Organizer   bool
}

type panelView struct {
	Row            eventRow
	Mon, Day, DOW  string
	TimeRange      string
	Repeat         string
	People         []peopleLine
	PeopleCount    int
	Agenda         string
	AgendaShort    string
	AgendaLong     bool
	WhyChips       []string
	Confidence     string
	Options        []entityOpt
	PinVals        string
	CanRemove      bool
	RemoveVals     string
	DocURL         string
	DocName        string
	DocColor       template.CSS
	CloseURL       string
	TaggingEnabled bool
}

type correctionRow struct {
	Name, Entity string
	Color        template.CSS
	RemoveVals   string
	When         string
}

type toastView struct {
	Error       bool
	Title, Body string
	UndoVals    string
	SeriesID    string
	View        string
}

var hexColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

// cssColor passes a configured color through to a style attribute only if
// it's a plain hex color; anything else becomes neutral grey.
func cssColor(c string) template.CSS {
	if hexColorRe.MatchString(c) {
		return template.CSS(c)
	}
	return template.CSS("#8f96a1")
}

func (u *uiServer) statusView() statusView {
	cfg := u.cur()
	resp, _, ready, updated := u.st.get()
	version, refreshing := u.st.stamp()
	sv := statusView{
		Warming:    !ready,
		Syncing:    refreshing,
		Stamp:      strconv.FormatUint(version, 10),
		FeedErrors: len(resp.Errors),
		PollEvery:  "30s",
	}
	if ready {
		sv.Updated = updated.In(cfg.loc).Format("3:04 PM")
		sv.JustNow = u.clock().Sub(updated) < 15*time.Second
	}
	if sv.Warming || sv.Syncing {
		sv.PollEvery = "1s"
	}
	return sv
}

// buildPage assembles a whole view. just names a series the user changed a
// moment ago, which gets the settle animation.
func (u *uiServer) buildPage(q pageQuery, just string) pageView {
	cfg := u.cur()
	loc := cfg.loc
	resp, _, ready, _ := u.st.get()
	now := u.clock().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	from := today
	if t, err := time.ParseInLocation("2006-01-02", q.From, loc); err == nil {
		from = t
	}
	if q.Filter != "guesses" {
		q.Filter = ""
	}
	if q.Filter == "guesses" {
		// Guesses is a review queue over the whole feed window, so it has no
		// page of dates to be on and always lays out as an agenda.
		q.Kind, q.From, q.Open = "agenda", "", false
		from = today
	}

	v := pageView{
		Kind: q.Kind, IsAgenda: q.Kind == "agenda", IsWeek: q.Kind == "week",
		Self:      q.url(nil),
		AgendaURL: q.url(func(p *pageQuery) { p.Kind = "agenda" }),
		WeekURL:   q.url(func(p *pageQuery) { p.Kind = "week" }),
		Status:    u.statusView(),
		Warming:   !ready,
		Tagging:   cfg.classifier != nil,
		GuessesOn: q.Filter == "guesses",
		Client:    q.Client,
	}
	v.AllURL = q.url(func(p *pageQuery) { p.Filter, p.Client = "", "" })
	v.GuessesURL = q.url(func(p *pageQuery) { p.Kind, p.Filter, p.From, p.Sel, p.On = "agenda", "guesses", "", "", "" })
	v.GuessesToggleURL = v.GuessesURL
	if q.Filter == "guesses" {
		v.GuessesToggleURL = q.url(func(p *pageQuery) { p.Filter = "" })
		v.WeekURL = q.url(func(p *pageQuery) { p.Kind, p.Filter = "week", "" })
	}
	v.ClientClearURL = q.url(func(p *pageQuery) { p.Client = "" })

	feedColor := map[string]template.CSS{}
	for _, f := range cfg.feeds {
		feedColor[f.Name] = cssColor(f.Color)
	}
	entColor := map[string]template.CSS{}
	entURL := map[string]string{}
	for _, e := range cfg.classifier.Entities() {
		entColor[e.Name] = cssColor(e.Color)
		entURL[e.Name] = e.URL
	}
	for _, e := range cfg.classifier.Entities() {
		name := e.Name
		v.Clients = append(v.Clients, clientOpt{
			Name: name, Color: entColor[name], Selected: name == q.Client,
			URL: q.url(func(p *pageQuery) { p.Client = name }),
		})
	}

	// Count guesses in the whole window (for the chip), then filter.
	seenGuess := map[string]bool{}
	for _, ev := range resp.Events {
		if isGuess(ev) && !ev.Past && !seenGuess[ev.SeriesId] && cfg.classifier != nil {
			seenGuess[ev.SeriesId] = true
		}
	}
	v.GuessCount = len(seenGuess)

	mk := func(ev Event) eventRow {
		return u.row(cfg, ev, q, v.Self, just, feedColor, entColor)
	}
	queued := map[string]bool{}
	keep := func(ev Event) bool {
		if q.Client != "" && ev.Entity != q.Client {
			return false
		}
		if q.Filter == "guesses" {
			// One row per series, its next meeting: fixing it fixes them all,
			// and it matches how the chip counts.
			if !isGuess(ev) || ev.Past || queued[ev.SeriesId] {
				return false
			}
			queued[ev.SeriesId] = true
		}
		return true
	}

	switch q.Kind {
	case "week":
		start := from.AddDate(0, 0, -((int(from.Weekday()) + 6) % 7)) // Monday
		v.Week = buildWeek(resp.Events, start, now, loc, keep, mk)
		end := start.AddDate(0, 0, len(v.Week.Cols)-1)
		v.RangeLabel = rangeLabel(start, end)
		v.PrevURL = q.url(func(p *pageQuery) { p.From = start.AddDate(0, 0, -7).Format("2006-01-02"); p.Sel, p.On = "", "" })
		v.NextURL = q.url(func(p *pageQuery) { p.From = start.AddDate(0, 0, 7).Format("2006-01-02"); p.Sel, p.On = "", "" })
		v.IsCurrent = !today.Before(start) && today.Before(start.AddDate(0, 0, 7))
	default:
		until := from.AddDate(0, 0, agendaSpanDays)
		if v.GuessesOn {
			until = time.Time{} // no end: the queue runs to the end of the feeds
		}
		v.Agenda = buildAgenda(resp.Events, from, until, now, loc, keep, mk, q.Filter == "" && q.Client == "")
		if e := v.Agenda.Earlier; e != nil {
			e.Open = q.Open
			e.OpenURL = q.url(func(p *pageQuery) { p.Open = true })
			e.FoldURL = q.url(func(p *pageQuery) { p.Open = false })
		}
		v.RangeLabel = rangeLabel(from, from.AddDate(0, 0, agendaSpanDays-1))
		if v.GuessesOn {
			v.RangeLabel = "Upcoming guesses"
		}
		v.PrevURL = q.url(func(p *pageQuery) {
			p.From = from.AddDate(0, 0, -agendaSpanDays).Format("2006-01-02")
			p.Sel, p.On = "", ""
		})
		v.NextURL = q.url(func(p *pageQuery) {
			p.From = from.AddDate(0, 0, agendaSpanDays).Format("2006-01-02")
			p.Sel, p.On = "", ""
		})
		v.IsCurrent = from.Equal(today)
		v.EmptyFilterResult = ready && v.Agenda.Earlier == nil && len(v.Agenda.Days) == 0
	}
	v.TodayURL = q.url(func(p *pageQuery) { p.From, p.Sel, p.On = "", "", "" })

	if q.Sel != "" {
		v.Panel = u.panel(cfg, resp, q, v.Self, just, feedColor, entColor, entURL)
	}
	if v.Panel == nil && cfg.classifier != nil {
		for _, c := range cfg.classifier.fixes.all() {
			when := ""
			if t, err := time.Parse(time.RFC3339, c.Updated); err == nil {
				when = t.In(loc).Format("Jan 2")
			}
			v.Corrections = append(v.Corrections, correctionRow{
				Name: c.Name, Entity: c.Entity, Color: entColor[c.Entity], When: when,
				RemoveVals: hxVals(map[string]string{"seriesId": c.SeriesID, "entity": "none", "view": v.Self}),
			})
		}
	}
	return v
}

func isGuess(ev Event) bool {
	if own, who := oooKind(ev); own || who != "" {
		return false // time off isn't a meeting to tag
	}
	return ev.EntityVia == viaLearned || ev.EntityVia == viaFeed || ev.EntityVia == ""
}

var (
	oooRe      = regexp.MustCompile(`(?i)^\s*(ooo|pto|out of (the )?office|vacation|holiday|day off|off)\b`)
	awayWhoRe  = regexp.MustCompile(`(?i)^\s*(.+?)\s*[-–—:]\s*(vacation( alert)?|ooo|out of (the )?office|pto)\s*$`)
	weekendDay = map[time.Weekday]bool{time.Saturday: true, time.Sunday: true}
)

// oooKind spots all-day time off: your own ("OOO", "PTO", "Vacation") or
// someone else's ("Jordan - Vacation Alert").
func oooKind(ev Event) (own bool, who string) {
	if !ev.AllDay {
		return false, ""
	}
	if m := awayWhoRe.FindStringSubmatch(ev.Name); m != nil {
		return false, m[1]
	}
	return oooRe.MatchString(ev.Name), ""
}

// oooNote describes a time-off span: dates, which day this is, and the next
// weekday back.
func oooNote(ev Event, loc *time.Location) string {
	start, err1 := time.ParseInLocation("2006-01-02", ev.StartDate, loc)
	end, err2 := time.ParseInLocation("2006-01-02", ev.EndDate, loc)
	if err1 != nil || err2 != nil {
		return ""
	}
	note := rangeLabel(start, end)
	if start.Equal(end) {
		note = start.Format("Mon Jan 2")
	}
	if ev.SpanDays > 1 {
		note += fmt.Sprintf(" · day %d of %d", ev.DayIndex, ev.SpanDays)
	}
	back := end.AddDate(0, 0, 1)
	for weekendDay[back.Weekday()] {
		back = back.AddDate(0, 0, 1)
	}
	return note + " · back " + back.Format("Mon Jan 2")
}

func rangeLabel(a, b time.Time) string {
	if a.Month() == b.Month() {
		return a.Format("Jan 2") + " – " + b.Format("2")
	}
	return a.Format("Jan 2") + " – " + b.Format("Jan 2")
}

func eventTimes(ev Event, loc *time.Location) (start, end time.Time, ok bool) {
	start, err := time.Parse(time.RFC3339, ev.Start)
	if err != nil {
		return
	}
	start = start.In(loc)
	end = start
	if e, err := time.Parse(time.RFC3339, ev.End); err == nil {
		end = e.In(loc)
	}
	return start, end, true
}

// prettyDur spaces calmerge's compact durations: "1h30m" -> "1h 30m".
func prettyDur(d string) string {
	if i := strings.Index(d, "h"); i > 0 && i < len(d)-1 {
		return d[:i+1] + " " + d[i+1:]
	}
	return d
}

func clockLabel(t time.Time) string { return t.Format("3:04") }

func (u *uiServer) row(cfg *config, ev Event, q pageQuery, self, just string, feedColor, entColor map[string]template.CSS) eventRow {
	r := eventRow{
		ID:       "ev-" + ev.SeriesId + "-" + ev.Date,
		SeriesID: ev.SeriesId, Date: ev.Date, Title: ev.Name,
		AllDay: ev.AllDay, Cal: ev.Source, CalColor: feedColor[ev.Source],
		MeetURL: ev.MeetURL, MeetKind: ev.MeetKind,
		HasAgenda: ev.Agenda != "", Past: ev.Past,
		Tagged: cfg.classifier != nil,
	}
	if r.CalColor == "" {
		r.CalColor = cssColor(ev.Color)
	}
	if r.MeetKind == "Meet" {
		r.MeetKind = "Google Meet"
	}
	if st, _, ok := eventTimes(ev, cfg.loc); ok && !ev.AllDay {
		r.Time = clockLabel(st)
		r.Dur = prettyDur(ev.Duration)
	}
	if ev.AllDay {
		r.Time = "All day"
		if ev.MultiDay {
			r.AllDayNote = fmt.Sprintf("day %d of %d", ev.DayIndex, ev.SpanDays)
		}
	}
	switch ev.LocKind {
	case "link":
		r.LinkLabel, r.LinkURL = ev.Location, ev.Address
	case "map":
		r.MapLabel, r.MapURL = ev.Location, ev.MapURL
	}
	if n := len(ev.Attendees); n > 0 {
		r.People = fmt.Sprintf("%d people", n)
		if n == 1 {
			r.People = "1 person"
		}
		if ev.Organizer != "" {
			r.People += " · " + ev.Organizer
		}
	}
	if own, who := oooKind(ev); own || who != "" {
		r.OOO, r.AwayWho = own, who
		r.OOONote = oooNote(ev, cfg.loc)
		r.Tagged = false // not a meeting: no client, nothing to confirm
	}
	if r.Tagged {
		r.Entity, r.Via = ev.Entity, ev.EntityVia
		r.EntityColor = entColor[ev.Entity]
		if r.EntityColor == "" {
			r.EntityColor = "#8f96a1"
		}
		r.Guess = isGuess(ev)
		r.You = ev.EntityVia == viaManual
		r.Why = ev.EntityWhy
		r.ViaLabel = viaLabel(ev.EntityVia, ev.EntityWhy)
		if r.Guess && ev.Entity != "" {
			r.ConfirmVals = hxVals(map[string]string{"seriesId": ev.SeriesId, "entity": ev.Entity, "view": self})
		}
	}
	r.SelectURL = q.url(func(p *pageQuery) { p.Sel, p.On = ev.SeriesId, ev.Date })
	r.Selected = q.Sel == ev.SeriesId && (q.On == "" || q.On == ev.Date)
	r.Just = just != "" && just == ev.SeriesId
	return r
}

var modelPctRe = regexp.MustCompile(`^model, (\d+)% sure`)

func viaLabel(via, why string) string {
	switch via {
	case viaManual:
		return "You"
	case viaCategory:
		return "Outlook"
	case viaRule:
		return "Rule"
	case viaLearned:
		if m := modelPctRe.FindStringSubmatch(why); m != nil {
			return "Model " + m[1] + "%"
		}
		return "Model"
	case viaFeed:
		return "Default"
	}
	return "Unplaced"
}

// withGaps inserts free-time dividers between a day's timed meetings,
// counting overlapping meetings as busy until the last one ends.
func withGaps(rows []eventRow, times [][2]time.Time, gaps bool) []agendaItem {
	var out []agendaItem
	var busyUntil time.Time
	for i, r := range rows {
		st, en := times[i][0], times[i][1]
		if gaps && !r.AllDay && !busyUntil.IsZero() && st.Sub(busyUntil) >= minGapMinutes*time.Minute {
			out = append(out, agendaItem{Gap: true, GapTimes: clockLabel(busyUntil) + " – " + clockLabel(st), GapLen: "free " + spanLabel(st.Sub(busyUntil))})
		}
		out = append(out, agendaItem{Row: r})
		if !r.AllDay && en.After(busyUntil) {
			busyUntil = en
		}
	}
	for i := range out {
		out[i].FirstInSection = i == 0
	}
	return out
}

func spanLabel(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	h, mm := m/60, m%60
	switch {
	case h > 0 && mm > 0:
		return fmt.Sprintf("%dh %dm", h, mm)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", mm)
}

// buildAgenda lays out the days from from up to (not including) until, or
// with no end when until is zero (the guess queue). Today's finished meetings
// fold into one "earlier" box; gaps adds the free-time dividers.
func buildAgenda(evs []Event, from, until, now time.Time, loc *time.Location, keep func(Event) bool, mk func(Event) eventRow, gaps bool) *agendaView {
	av := &agendaView{}
	todayKey := now.Format("2006-01-02")
	type dayAcc struct {
		rows      []eventRow
		times     [][2]time.Time
		ooo, away []eventRow
	}
	days := map[string]*dayAcc{}
	var order []string
	var earlierRows []eventRow
	var earlierTimes [][2]time.Time
	delay := 0
	for _, ev := range evs {
		d, err := time.ParseInLocation("2006-01-02", ev.Date, loc)
		if err != nil || d.Before(from) || (!until.IsZero() && !d.Before(until)) || !keep(ev) {
			continue
		}
		st, en, _ := eventTimes(ev, loc)
		r := mk(ev)
		if ev.Date == todayKey && ev.Past && !ev.AllDay {
			// Staggered so unfolding "today" cascades the rows in.
			r.Delay = min(len(earlierRows)*40+120, 600)
			earlierRows = append(earlierRows, r)
			earlierTimes = append(earlierTimes, [2]time.Time{st, en})
			continue
		}
		r.Delay = min(delay, 600)
		delay += 35
		acc, ok := days[ev.Date]
		if !ok {
			acc = &dayAcc{}
			days[ev.Date] = acc
			order = append(order, ev.Date)
		}
		switch {
		case r.OOO:
			acc.ooo = append(acc.ooo, r)
			continue
		case r.AwayWho != "":
			acc.away = append(acc.away, r)
			continue
		}
		acc.rows = append(acc.rows, r)
		acc.times = append(acc.times, [2]time.Time{st, en})
	}
	if len(earlierRows) > 0 {
		ev := &earlierView{Label: dayLabel(now, now), Count: len(earlierRows)}
		var titles []string
		guesses := 0
		for i, r := range earlierRows {
			if i < 3 {
				titles = append(titles, r.Title)
			}
			if r.Guess {
				guesses++
			}
			ev.Dots = append(ev.Dots, r.EntityColor)
		}
		ev.Titles = strings.Join(titles, ", ")
		if len(earlierRows) > 3 {
			ev.Titles += fmt.Sprintf(" +%d", len(earlierRows)-3)
		}
		ev.Span = earlierTimes[0][0].Format("3:04 PM") + " – " + latest(earlierTimes).Format("3:04 PM")
		if guesses > 0 {
			ev.Guesses = fmt.Sprintf("%d %s", guesses, plural(guesses, "guess", "guesses"))
		}
		ev.Items = withGaps(earlierRows, earlierTimes, gaps)
		av.Earlier = ev
	}
	for _, key := range order {
		d, _ := time.ParseInLocation("2006-01-02", key, loc)
		acc := days[key]
		n := 0
		for _, r := range acc.rows {
			if !r.AllDay {
				n++
			}
		}
		count := fmt.Sprintf("%d %s", n, plural(n, "meeting", "meetings"))
		switch {
		case n == 0 && len(acc.ooo) > 0:
			count = "out of office"
		case len(acc.ooo) > 0:
			count += " · out of office"
		case n == 0:
			count = "no meetings"
		}
		av.Days = append(av.Days, agendaDay{
			Label: dayLabel(d, now), Date: key, OOO: acc.ooo, Away: acc.away,
			Count: count,
			Items: withGaps(acc.rows, acc.times, gaps),
		})
	}
	return av
}

func latest(ts [][2]time.Time) time.Time {
	var l time.Time
	for _, t := range ts {
		if t[1].After(l) {
			l = t[1]
		}
	}
	return l
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func dayLabel(d, now time.Time) string {
	label := d.Format("Mon Jan 2")
	switch d.Format("2006-01-02") {
	case now.Format("2006-01-02"):
		return "Today · " + label
	case now.AddDate(0, 0, 1).Format("2006-01-02"):
		return "Tomorrow · " + label
	}
	return label
}

func buildWeek(evs []Event, monday, now time.Time, loc *time.Location, keep func(Event) bool, mk func(Event) eventRow) *weekView {
	wv := &weekView{}
	days := 5
	type timed struct {
		ev     Event
		st, en time.Time
	}
	byDay := map[string][]timed{}
	allDay := map[string][]Event{} // by series
	var allDayOrder []string
	lo, hi := 8*60, 18*60
	for _, ev := range evs {
		d, err := time.ParseInLocation("2006-01-02", ev.Date, loc)
		if err != nil || d.Before(monday) || !d.Before(monday.AddDate(0, 0, 7)) || !keep(ev) {
			continue
		}
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			days = 7
		}
		if ev.AllDay {
			if _, ok := allDay[ev.SeriesId]; !ok {
				allDayOrder = append(allDayOrder, ev.SeriesId)
			}
			allDay[ev.SeriesId] = append(allDay[ev.SeriesId], ev)
			continue
		}
		st, en, ok := eventTimes(ev, loc)
		if !ok {
			continue
		}
		if !en.After(st) {
			en = st.Add(15 * time.Minute)
		}
		byDay[ev.Date] = append(byDay[ev.Date], timed{ev, st, en})
		sm := st.Hour()*60 + st.Minute()
		em := sm + int(en.Sub(st)/time.Minute)
		lo = min(lo, sm/60*60)
		hi = max(hi, min((em+59)/60*60, 24*60))
	}
	for h := lo; h < hi; h += 60 {
		t := time.Date(2000, 1, 1, h/60, 0, 0, 0, time.UTC)
		wv.Hours = append(wv.Hours, hourMark{Label: strings.ToLower(t.Format("3pm")), Top: (h - lo) * weekHourPx / 60})
	}
	wv.Height = (hi - lo) * weekHourPx / 60
	wv.ColTemplate = template.CSS(fmt.Sprintf("repeat(%d, minmax(0, 1fr))", days))

	colIdx := map[string]int{}
	for i := 0; i < days; i++ {
		d := monday.AddDate(0, 0, i)
		key := d.Format("2006-01-02")
		colIdx[key] = i
		col := weekCol{DOW: d.Format("Mon"), DateLabel: d.Format("1/2"), Date: key, IsToday: key == now.Format("2006-01-02")}
		items := byDay[key]
		sort.SliceStable(items, func(a, b int) bool { return items[a].st.Before(items[b].st) })
		for j, it := range items {
			// Indent by how many earlier meetings are still running, so the
			// longer meeting underneath keeps its title visible.
			level := 0
			for _, prev := range items[:j] {
				if prev.en.After(it.st) {
					level++
				}
			}
			level = min(level, 2)
			sm := it.st.Hour()*60 + it.st.Minute()
			top := (sm - lo) * weekHourPx / 60
			h := max(int(it.en.Sub(it.st)/time.Minute)*weekHourPx/60-2, weekMinBlockPx)
			showTime := h >= 60
			lines := max(1, (h-8-map[bool]int{true: 15, false: 0}[showTime])/15)
			r := mk(it.ev)
			col.Blocks = append(col.Blocks, weekBlock{
				Row: r, Top: top, Height: h, Lines: lines, ShowTime: showTime,
				TimeLabel: r.Time + " (" + r.Dur + ")",
				Left:      template.CSS(fmt.Sprintf("left: %dpx; width: calc(100%% - %dpx)", level*weekIndentPx, level*weekIndentPx)),
			})
		}
		if col.IsToday {
			nm := now.Hour()*60 + now.Minute()
			if nm >= lo && nm <= hi {
				col.HasNow, col.NowTop = true, (nm-lo)*weekHourPx/60
			}
		}
		wv.Cols = append(wv.Cols, col)
	}

	// All-day lane: one bar per series across the days it covers.
	laneEnds := []int{}
	for _, id := range allDayOrder {
		list := allDay[id]
		first, last := days, -1
		for _, ev := range list {
			if i, ok := colIdx[ev.Date]; ok {
				first, last = min(first, i), max(last, i)
			}
		}
		if last < 0 {
			continue
		}
		lane := 0
		for lane < len(laneEnds) && laneEnds[lane] >= first {
			lane++
		}
		if lane == len(laneEnds) {
			laneEnds = append(laneEnds, last)
		} else {
			laneEnds[lane] = last
		}
		b := allDayBar{Row: mk(list[0]), Col: first, Span: last - first + 1, Lane: lane, ContinuesPre: list[0].DayIndex > 1 && first == 0}
		if b.Row.OOO {
			for i := first; i <= last; i++ {
				wv.Cols[i].OOO = true
			}
		}
		b.GridColumn = template.CSS(fmt.Sprintf("%d / span %d", first+1, b.Span))
		b.GridRow = template.CSS(strconv.Itoa(lane + 1))
		wv.AllDay = append(wv.AllDay, b)
	}
	wv.AllDayLanes = len(laneEnds)
	return wv
}

func (u *uiServer) panel(cfg *config, resp Response, q pageQuery, self, just string, feedColor, entColor map[string]template.CSS, entURL map[string]string) *panelView {
	var ev *Event
	count := 0
	seenDates := map[string]bool{}
	for i := range resp.Events {
		e := &resp.Events[i]
		if e.SeriesId != q.Sel {
			continue
		}
		if !seenDates[e.Date] {
			seenDates[e.Date] = true
			count++
		}
		if ev == nil || e.Date == q.On {
			ev = e
		}
	}
	if ev == nil {
		return nil
	}
	p := &panelView{Row: u.row(cfg, *ev, q, self, just, feedColor, entColor)}
	p.TaggingEnabled = p.Row.Tagged // off for time off, which isn't a meeting
	p.CloseURL = q.url(func(x *pageQuery) { x.Sel, x.On = "", "" })
	if d, err := time.ParseInLocation("2006-01-02", ev.Date, cfg.loc); err == nil {
		p.Mon, p.Day, p.DOW = strings.ToUpper(d.Format("Jan")), d.Format("2"), d.Format("Mon")
	}
	if st, en, ok := eventTimes(*ev, cfg.loc); ok && !ev.AllDay {
		if st.Format("PM") == en.Format("PM") {
			p.TimeRange = st.Format("3:04") + " – " + en.Format("3:04 PM")
		} else {
			p.TimeRange = st.Format("3:04 PM") + " – " + en.Format("3:04 PM")
		}
	} else if ev.AllDay {
		p.TimeRange = "All day"
		if ev.MultiDay {
			p.TimeRange = fmt.Sprintf("All day · %s – %s", ev.StartDate, ev.EndDate)
		}
	}
	if count > 1 {
		p.Repeat = fmt.Sprintf("Repeats · %d in the next %d days", count, cfg.aheadDays)
	} else {
		p.Repeat = "One-time meeting"
	}

	// People, grouped by company domain, organizer first.
	type group struct {
		domain string
		names  []string
		org    bool
	}
	var groups []*group
	byDomain := map[string]*group{}
	for _, a := range ev.Attendees {
		dom := emailDomain(a.Email)
		name := a.Name
		if name == "" {
			name, _, _ = strings.Cut(a.Email, "@")
		}
		g := byDomain[dom]
		if g == nil {
			g = &group{domain: dom}
			byDomain[dom] = g
			groups = append(groups, g)
		}
		if a.Role == "organizer" {
			g.org = true
			g.names = append([]string{name}, g.names...)
		} else {
			g.names = append(g.names, name)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].org && !groups[j].org })
	for _, g := range groups {
		who := strings.Join(g.names, ", ")
		if len(g.names) > 3 {
			who = fmt.Sprintf("%d people", len(g.names))
		}
		p.People = append(p.People, peopleLine{Who: who, Domain: g.domain, Organizer: g.org})
	}
	p.PeopleCount = len(ev.Attendees)

	p.Agenda = tidyAgenda(ev.Agenda)
	if len(p.Agenda) > 280 {
		p.AgendaLong = true
		cut := strings.LastIndex(p.Agenda[:280], " ")
		if cut < 200 {
			cut = 280
		}
		p.AgendaShort = strings.TrimSpace(p.Agenda[:cut]) + "…"
	}

	if p.TaggingEnabled {
		why := ev.EntityWhy
		if m := modelPctRe.FindStringSubmatch(why); m != nil {
			p.Confidence = m[1] + "%"
			_, why, _ = strings.Cut(why, ": ")
			p.WhyChips = strings.Split(why, ", ")
		} else if why != "" {
			p.WhyChips = strings.Split(why, "; ")
		}
		for _, e := range cfg.classifier.Entities() {
			p.Options = append(p.Options, entityOpt{Name: e.Name, Color: entColor[e.Name], Selected: e.Name == ev.Entity})
		}
		p.PinVals = hxVals(map[string]string{"seriesId": ev.SeriesId, "view": self})
		if p.Row.You {
			p.CanRemove = true
			p.RemoveVals = hxVals(map[string]string{"seriesId": ev.SeriesId, "entity": "none", "view": self})
		}
		if u := entURL[ev.Entity]; u != "" {
			p.DocURL, p.DocName, p.DocColor = u, ev.Entity, entColor[ev.Entity]
		}
	}
	return p
}

// tidyAgenda folds the lone "*" lines Outlook leaves for bullets into the
// line they belong to: "*\nCatch up" -> "• Catch up".
func tidyAgenda(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	bullet := ""
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "*" || t == "•" || t == "-" || t == "o" {
			// Keep the indent of nested bullets.
			bullet = ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))] + "• "
			continue
		}
		if bullet != "" && t != "" {
			out = append(out, bullet+t)
			bullet = ""
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
