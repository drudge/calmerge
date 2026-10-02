package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entity is one business/client an event can belong to. Configured via the
// ENTITIES env var as a JSON array. Name them the way your own planning docs
// do, so an event's entity joins straight to its workstream there.
type Entity struct {
	Name       string   `json:"name" toml:"name"`
	Parent     string   `json:"parent,omitempty" toml:"parent,omitempty"` // rollup, e.g. the holding company a client belongs to
	Color      string   `json:"color,omitempty" toml:"color,omitempty"`
	URL        string   `json:"url,omitempty" toml:"url,omitempty"`               // e.g. the entity's page in your wiki or project tool
	Keywords   []string `json:"keywords,omitempty" toml:"keywords,omitempty"`     // whole-word, case/punctuation-insensitive; title hits count most
	Domains    []string `json:"domains,omitempty" toml:"domains,omitempty"`       // attendee/organizer email domains (subdomains match too)
	Categories []string `json:"categories,omitempty" toml:"categories,omitempty"` // Outlook category names; the entity name always counts
	Feeds      []string `json:"feeds,omitempty" toml:"feeds,omitempty"`           // feeds whose otherwise-unplaced events fall back to this entity
}

// EntityInfo is the public view of an Entity, listed once per response so each
// event only has to carry the name.
type EntityInfo struct {
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"`
	Color  string `json:"color,omitempty"`
	URL    string `json:"url,omitempty"`
}

// How an event's entity was decided, strongest first.
const (
	viaManual   = "manual"   // the user corrected this series (set_event_entity)
	viaCategory = "category" // an ICS CATEGORIES value (Outlook's manual categorize)
	viaRule     = "rule"     // keyword / domain rules from ENTITIES
	viaLearned  = "learned"  // naive Bayes trained on the category/rule-labeled events
	viaFeed     = "feed"     // nothing else placed it; the feed's default entity
)

// Rule weights. A title keyword or the organizer's domain is enough on its own
// (minRuleScore); agenda/location keywords and other attendees' domains only
// nudge.
const (
	wTitle       = 3
	wOrgDomain   = 3
	wBody        = 1
	wDomain      = 1
	minRuleScore = 2

	// minLearnedConf is the posterior the learned model needs before it labels
	// an event the rules couldn't place, and minKnownFeatures is how many of the
	// event's features (feed aside) it must have seen in training. One shared
	// word isn't enough to go on.
	minLearnedConf   = 0.85
	minKnownFeatures = 2

	// nbAlpha is the additive smoothing. Well under 1 because the training set
	// is a few dozen events; add-one would wash out every signal.
	nbAlpha = 0.1
)

// signals are the classifier inputs gathered from the raw ICS event. They are
// captured regardless of INCLUDE_AGENDA / INCLUDE_ATTENDEES, since those only
// trim the JSON payload.
type signals struct {
	title      string
	body       string // cleaned DESCRIPTION
	location   string
	categories []string
	orgDomain  string   // organizer's email domain
	domains    []string // every attendee's email domain, deduped
	people     []string // every attendee, by lowercased email (or name when there's no email)
	organizer  string
	tenant     string // Microsoft tenant id from a Teams join link
	feed       string
}

// compiledEntity is an Entity with its lookups built once.
type compiledEntity struct {
	Entity
	keywords []keyword
	cats     map[string]bool
	feeds    map[string]bool
}

// keyword is one configured keyword: norm is matched (see normText), text is
// shown when explaining a match.
type keyword struct {
	norm, text string
}

type classifier struct {
	mu       sync.Mutex // one classify at a time: refreshes and corrections both run it
	entities []compiledEntity
	fixes    *corrections     // user corrections by seriesId; may be nil
	memory   *lessons         // firm labels remembered across refreshes; may be nil
	clock    func() time.Time // for tests; nil means time.Now
}

func parseEntities(raw string) ([]Entity, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var ents []Entity
	if err := json.Unmarshal([]byte(raw), &ents); err != nil {
		return nil, fmt.Errorf("ENTITIES is not valid JSON: %w", err)
	}
	return ents, prepareEntities(ents)
}

// prepareEntities validates entities (from ENTITIES or the config file) and
// normalizes their colors in place.
func prepareEntities(ents []Entity) error {
	seen := map[string]bool{}
	for i, e := range ents {
		name := strings.ToLower(strings.TrimSpace(e.Name))
		if name == "" {
			return fmt.Errorf("entity #%d has no name", i+1)
		}
		if seen[name] {
			return fmt.Errorf("entity %q is listed twice", e.Name)
		}
		seen[name] = true
		ents[i].Color = normalizeColor(e.Color)
	}
	return nil
}

// newClassifier compiles the entity rules. Returns nil when no entities are
// configured, which turns classification off.
func newClassifier(ents []Entity) *classifier {
	if len(ents) == 0 {
		return nil
	}
	c := &classifier{}
	for _, e := range ents {
		ce := compiledEntity{Entity: e, cats: map[string]bool{}, feeds: map[string]bool{}}
		for _, k := range e.Keywords {
			if n := normText(k); n != " " {
				ce.keywords = append(ce.keywords, keyword{norm: n, text: strings.TrimSpace(k)})
			}
		}
		ce.cats[strings.ToLower(strings.TrimSpace(e.Name))] = true
		for _, cat := range e.Categories {
			ce.cats[strings.ToLower(strings.TrimSpace(cat))] = true
		}
		for _, f := range e.Feeds {
			ce.feeds[strings.ToLower(strings.TrimSpace(f))] = true
		}
		c.entities = append(c.entities, ce)
	}
	return c
}

// normText lowercases s and turns every run of non-letters/digits into a
// single space, padded at both ends. Matching a normalized keyword as a
// substring of normalized text is then a whole-word match that ignores case
// and punctuation: "Acme Rocket Co" hits `Acme "Rocket" Co`, and "ARC" skips "arcade".
func normText(s string) string {
	return " " + strings.Join(tokenSplitRe.Split(strings.ToLower(s), -1), " ") + " "
}

// Entities lists the configured entities for the response payload.
func (c *classifier) Entities() []EntityInfo {
	if c == nil {
		return nil
	}
	out := make([]EntityInfo, len(c.entities))
	for i, e := range c.entities {
		out[i] = EntityInfo{Name: e.Name, Parent: e.Parent, Color: e.Color, URL: e.URL}
	}
	return out
}

// byFeed returns the entity a feed falls back to, if any.
func (c *classifier) byFeed(s signals) (int, bool) {
	f := strings.ToLower(strings.TrimSpace(s.feed))
	for i, e := range c.entities {
		if e.feeds[f] {
			return i, true
		}
	}
	return 0, false
}

// lookup finds a configured entity by name, case-insensitively.
func (c *classifier) lookup(name string) (int, bool) {
	for i, e := range c.entities {
		if strings.EqualFold(e.Name, strings.TrimSpace(name)) {
			return i, true
		}
	}
	return 0, false
}

// byCorrection returns the entity the user pinned this series to. A pin to an
// entity that's since been removed from ENTITIES is ignored.
func (c *classifier) byCorrection(seriesID string) (int, bool) {
	name, ok := c.fixes.get(seriesID)
	if !ok {
		return 0, false
	}
	return c.lookup(name)
}

// byCategory returns the entity an Outlook category maps to, if any, and why.
func (c *classifier) byCategory(s signals) (int, string, bool) {
	for _, cat := range s.categories {
		key := strings.ToLower(strings.TrimSpace(cat))
		for i, e := range c.entities {
			if e.cats[key] {
				return i, fmt.Sprintf("Outlook category %q", cat), true
			}
		}
	}
	return 0, "", false
}

// byRules scores every entity and returns the clear winner, with the matches
// that put it there: at least minRuleScore and strictly ahead of the
// runner-up. Ties stay unlabeled so the learned model (or nobody) decides.
func (c *classifier) byRules(s signals) (int, string, bool) {
	title, body, loc := normText(s.title), normText(s.body), normText(s.location)
	best, bestScore, second := -1, 0, 0
	var bestWhy []string
	for i, e := range c.entities {
		score := 0
		var why []string
		for _, k := range e.keywords {
			switch {
			case strings.Contains(title, k.norm):
				score += wTitle
				why = append(why, fmt.Sprintf("title says %q", k.text))
			case strings.Contains(body, k.norm), strings.Contains(loc, k.norm):
				score += wBody
				why = append(why, fmt.Sprintf("agenda/location mentions %q", k.text))
			}
		}
		for _, d := range e.Domains {
			d = strings.ToLower(strings.TrimSpace(d))
			if d == "" {
				continue
			}
			if domainMatch(s.orgDomain, d) {
				score += wOrgDomain
				why = append(why, "organizer is @"+s.orgDomain)
			}
			for _, ad := range s.domains {
				if ad != s.orgDomain && domainMatch(ad, d) {
					score += wDomain
					why = append(why, "attendee from @"+ad)
				}
			}
		}
		switch {
		case score > bestScore:
			best, second, bestScore, bestWhy = i, bestScore, score, why
		case score > second:
			second = score
		}
	}
	if best < 0 || bestScore < minRuleScore || bestScore == second {
		return 0, "", false
	}
	return best, strings.Join(bestWhy, "; "), true
}

// domainMatch reports whether host is want or a subdomain of it.
func domainMatch(host, want string) bool {
	return host != "" && (host == want || strings.HasSuffix(host, "."+want))
}

// classify labels every event in place. The user's corrections come first,
// then categories and rules. Those firm labels train a naive Bayes model,
// along with every firm label remembered from past refreshes (the lessons
// file), and the model labels what's left when it is confident. The feed
// default catches the rest. Training takes one sample per series so a daily
// standup doesn't drown out everything else.
func (c *classifier) classify(evs []Event) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	label := func(ev *Event, i int, via, why string) {
		ev.Entity = c.entities[i].Name
		ev.EntityColor = c.entities[i].Color
		ev.EntityVia = via
		ev.EntityWhy = why
	}
	today := time.Now()
	if c.clock != nil {
		today = c.clock()
	}

	var pending []int
	nb := newNaiveBayes()
	trained := map[string]bool{}  // series already used as a training sample
	inWindow := map[string]bool{} // series seen this refresh
	for k := range evs {
		ev := &evs[k]
		if ev.SeriesId != "" {
			inWindow[ev.SeriesId] = true
		}
		i, ok := c.byCorrection(ev.SeriesId)
		via, why := viaManual, "you set this"
		if !ok {
			i, why, ok = c.byCategory(ev.sig)
			via = viaCategory
		}
		if !ok {
			i, why, ok = c.byRules(ev.sig)
			via = viaRule
		}
		if !ok {
			pending = append(pending, k)
			continue
		}
		label(ev, i, via, why)
		key := ev.SeriesId
		if key == "" {
			key = ev.Name + "\x00" + ev.Start
		}
		if trained[key] {
			continue
		}
		trained[key] = true
		f := features(ev.sig)
		nb.train(i, f)
		if ev.SeriesId != "" {
			c.memory.put(ev.SeriesId, lesson{
				Entity:   c.entities[i].Name,
				Via:      via,
				Name:     ev.Name,
				LastSeen: today.Format("2006-01-02"),
				Features: f,
			})
		}
	}

	// A series in the window with no firm label now (its correction was
	// removed, a keyword was dropped) stops teaching: the window beats memory.
	for id := range inWindow {
		if !trained[id] {
			c.memory.drop(id)
		}
	}
	// Everything remembered from series no longer in the window keeps teaching.
	// Lessons for entities since removed from ENTITIES are skipped.
	c.memory.each(func(id string, ls lesson) {
		if inWindow[id] {
			return
		}
		if i, ok := c.lookup(ls.Entity); ok {
			nb.train(i, ls.Features)
		}
	})
	c.memory.prune(today)
	if err := c.memory.saveIfChanged(); err != nil {
		log.Printf("classifier: %v", err)
	}

	for _, k := range pending {
		ev := &evs[k]
		guess, p, clues, guessed := nb.predict(features(ev.sig))
		if guessed && p >= minLearnedConf {
			label(ev, guess, viaLearned, fmt.Sprintf("model, %d%% sure: %s", pct(p), strings.Join(clues, ", ")))
			continue
		}
		// Not confident: say what the model leaned toward, so a person
		// reviewing it has somewhere to start.
		lean := "no clues matched past meetings"
		if guessed {
			lean = fmt.Sprintf("model leaned %s (%d%%, below %d%%)", c.entities[guess].Name, pct(p), pct(minLearnedConf))
		}
		if i, ok := c.byFeed(ev.sig); ok {
			label(ev, i, viaFeed, fmt.Sprintf("default for the %s calendar; %s", ev.sig.feed, lean))
		} else {
			ev.EntityWhy = lean
		}
	}
}

// pct renders a probability as a whole percentage, rounding down so 0.849
// never shows as a passing 85%.
func pct(p float64) int { return int(p * 100) }

// tenantRe pulls the Microsoft tenant id out of a (decoded) Teams join link.
// It identifies the organizer's company, which makes it a strong feature.
var tenantRe = regexp.MustCompile(`"Tid"\s*:\s*"([0-9a-fA-F-]{36})"`)

func teamsTenant(meetURL string) string {
	if meetURL == "" {
		return ""
	}
	dec, err := url.QueryUnescape(meetURL)
	if err != nil {
		dec = meetURL
	}
	if m := tenantRe.FindStringSubmatch(dec); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

func emailDomain(email string) string {
	_, d, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !ok {
		return ""
	}
	return d
}

// buildSignals gathers classifier inputs for one source event.
func buildSignals(feed, title, body, location string, categories []string, organizer string, attendees []Attendee, meetURL string) signals {
	s := signals{
		title:     title,
		body:      body,
		location:  location,
		organizer: strings.ToLower(strings.TrimSpace(organizer)),
		tenant:    teamsTenant(meetURL),
		feed:      feed,
	}
	for _, c := range categories {
		if c = strings.TrimSpace(c); c != "" {
			s.categories = append(s.categories, c)
		}
	}
	seen := map[string]bool{}
	for _, a := range attendees {
		who := strings.ToLower(strings.TrimSpace(a.Email))
		if who == "" {
			who = strings.ToLower(strings.TrimSpace(a.Name))
		}
		if who != "" && !seen["p:"+who] {
			seen["p:"+who] = true
			s.people = append(s.people, who)
		}
		d := emailDomain(a.Email)
		if d == "" {
			continue
		}
		if a.Role == "organizer" && s.orgDomain == "" {
			s.orgDomain = d
		}
		if !seen[d] {
			seen[d] = true
			s.domains = append(s.domains, d)
		}
	}
	return s
}

// stopwords are dropped from title/body tokens; they carry no entity signal.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "to": true, "of": true,
	"a": true, "an": true, "in": true, "on": true, "at": true, "is": true,
	"be": true, "we": true, "will": true, "this": true, "that": true, "it": true,
	"our": true, "you": true, "your": true, "are": true, "as": true, "or": true,
	"by": true, "from": true, "all": true, "any": true, "can": true, "so": true,
	"meeting": true, "call": true, "thanks": true, "thank": true, "please": true,
	"hi": true, "hello": true, "re": true, "fw": true, "fwd": true,
}

var tokenSplitRe = regexp.MustCompile(`[^\pL\pN]+`)

// maxBodyTokens caps how much agenda text feeds the model, so a long pasted
// email doesn't swamp the title and people signals.
const maxBodyTokens = 60

func words(text string, limit int) []string {
	var out []string
	for _, w := range tokenSplitRe.Split(strings.ToLower(text), -1) {
		if len(w) < 2 || stopwords[w] || isDigits(w) {
			continue
		}
		out = append(out, w)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// features turns signals into the prefixed token set the model sees. Prefixes
// keep "globex" in a title distinct from "globex" in an agenda.
func features(s signals) []string {
	var f []string
	for _, w := range words(s.title, 0) {
		f = append(f, "t:"+w)
	}
	for _, w := range words(s.body, maxBodyTokens) {
		f = append(f, "b:"+w)
	}
	for _, w := range words(s.location, 0) {
		f = append(f, "l:"+w)
	}
	for _, d := range s.domains {
		f = append(f, "d:"+d)
	}
	// Individual people: who's in the room says a lot about which client it is,
	// even when their domain is shared (your own company) or unconfigured.
	for _, p := range s.people {
		f = append(f, "p:"+p)
	}
	if s.orgDomain != "" {
		f = append(f, "od:"+s.orgDomain)
	}
	if s.organizer != "" {
		f = append(f, "o:"+s.organizer)
	}
	if s.tenant != "" {
		f = append(f, "tid:"+s.tenant)
	}
	if s.feed != "" {
		f = append(f, "f:"+strings.ToLower(s.feed))
	}
	return f
}

// naiveBayes is a tiny naive Bayes over binary features: each token counts once
// per document, with light additive smoothing. Plenty for a few dozen labeled
// events.
type naiveBayes struct {
	docs   map[int]int            // class -> document count
	counts map[int]map[string]int // class -> token -> documents containing it
	vocab  map[string]bool
	total  int
}

func newNaiveBayes() *naiveBayes {
	return &naiveBayes{docs: map[int]int{}, counts: map[int]map[string]int{}, vocab: map[string]bool{}}
}

func uniq(toks []string) []string {
	seen := make(map[string]bool, len(toks))
	out := toks[:0:0]
	for _, t := range toks {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func (m *naiveBayes) train(class int, toks []string) {
	if m.counts[class] == nil {
		m.counts[class] = map[string]int{}
	}
	for _, t := range uniq(toks) {
		m.counts[class][t]++
		m.vocab[t] = true
	}
	m.docs[class]++
	m.total++
}

// predict returns the most likely class, its posterior, and the (up to 3)
// features that most favored it over the runner-up, in plain words. ok is
// false when there's too little to go on: fewer than two classes trained, or
// fewer than minKnownFeatures of the event's tokens (feed aside) seen in
// training.
func (m *naiveBayes) predict(toks []string) (class int, p float64, clues []string, ok bool) {
	if len(m.docs) < 2 {
		return 0, 0, nil, false
	}
	var known []string
	informative := 0
	for _, t := range uniq(toks) {
		if m.vocab[t] {
			known = append(known, t)
			if !strings.HasPrefix(t, "f:") {
				informative++
			}
		}
	}
	if informative < minKnownFeatures {
		return 0, 0, nil, false
	}

	classes := make([]int, 0, len(m.docs))
	for c := range m.docs {
		classes = append(classes, c)
	}
	sort.Ints(classes) // deterministic tie-breaks

	like := func(c int, t string) float64 {
		n := float64(m.docs[c])
		return math.Log((float64(m.counts[c][t]) + nbAlpha) / (n + 2*nbAlpha))
	}
	logp := make([]float64, len(classes))
	for i, c := range classes {
		lp := math.Log(float64(m.docs[c]) / float64(m.total))
		for _, t := range known {
			lp += like(c, t)
		}
		logp[i] = lp
	}

	// Softmax over the log scores for a posterior.
	best, second := 0, -1
	for i, v := range logp {
		switch {
		case v > logp[best]:
			best, second = i, best
		case i != best && (second < 0 || v > logp[second]):
			second = i
		}
	}
	var sum float64
	for _, v := range logp {
		sum += math.Exp(v - logp[best])
	}

	// Clues: the features that most favor the winner over the runner-up.
	type clue struct {
		t string
		w float64
	}
	var cs []clue
	for _, t := range known {
		if w := like(classes[best], t) - like(classes[second], t); w > 0 {
			cs = append(cs, clue{t, w})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].w != cs[j].w {
			return cs[i].w > cs[j].w
		}
		return cs[i].t < cs[j].t
	})
	for i := 0; i < len(cs) && i < 3; i++ {
		clues = append(clues, featureLabel(cs[i].t))
	}
	return classes[best], 1 / sum, clues, true
}

// featureLabel turns a model feature back into words for explanations.
func featureLabel(f string) string {
	kind, v, _ := strings.Cut(f, ":")
	switch kind {
	case "t":
		return fmt.Sprintf("%q in the title", v)
	case "b":
		return fmt.Sprintf("%q in the agenda", v)
	case "l":
		return fmt.Sprintf("%q in the location", v)
	case "d":
		return "attendee @" + v
	case "od":
		return "organizer @" + v
	case "p":
		return v
	case "o":
		return "organized by " + v
	case "tid":
		if len(v) > 8 {
			v = v[:8]
		}
		return "Teams tenant " + v
	case "f":
		return v + " calendar"
	}
	return f
}
