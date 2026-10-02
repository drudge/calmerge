package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// correction pins one event series to an entity. The user sets these (usually
// through the MCP set_event_entity tool) when the classifier gets one wrong.
type correction struct {
	Entity  string `json:"entity"`
	Name    string `json:"name,omitempty"` // event title when it was set, so the file reads sensibly
	Updated string `json:"updated"`
}

// corrections is the saved seriesId -> correction map, kept in a small JSON
// file so fixes survive restarts and redeploys.
type corrections struct {
	mu   sync.RWMutex
	path string
	m    map[string]correction
}

// loadCorrections reads the corrections file. A missing file is fine (nothing
// corrected yet); a corrupt one is an error, so a later save can't silently
// wipe the user's fixes.
func loadCorrections(path string) (*corrections, error) {
	c := &corrections{path: path, m: map[string]correction{}}
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading corrections %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &c.m); err != nil {
		return nil, fmt.Errorf("corrections %s is not valid JSON: %w", path, err)
	}
	if c.m == nil {
		c.m = map[string]correction{}
	}
	return c, nil
}

// get returns the corrected entity name for a series.
func (c *corrections) get(seriesID string) (string, bool) {
	if c == nil || seriesID == "" {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	fix, ok := c.m[seriesID]
	return fix.Entity, ok
}

// entry returns the full correction for a series.
func (c *corrections) entry(seriesID string) (correction, bool) {
	if c == nil || seriesID == "" {
		return correction{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	fix, ok := c.m[seriesID]
	return fix, ok
}

// all returns every correction keyed by series, newest first.
func (c *corrections) all() []correctionEntry {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]correctionEntry, 0, len(c.m))
	for id, fix := range c.m {
		out = append(out, correctionEntry{SeriesID: id, correction: fix})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated != out[j].Updated {
			return out[i].Updated > out[j].Updated
		}
		return out[i].SeriesID < out[j].SeriesID
	})
	return out
}

// correctionEntry is a correction with the series it pins.
type correctionEntry struct {
	SeriesID string
	correction
}

func (c *corrections) len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}

// set pins seriesID to entity, or removes the pin when entity is empty, then
// saves the file. On a save error the change is rolled back so memory never
// claims a fix that won't survive a restart.
func (c *corrections) set(seriesID, entity, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, had := c.m[seriesID]
	if entity == "" {
		delete(c.m, seriesID)
	} else {
		c.m[seriesID] = correction{Entity: entity, Name: name, Updated: time.Now().UTC().Format(time.RFC3339)}
	}
	if err := c.save(); err != nil {
		if had {
			c.m[seriesID] = old
		} else {
			delete(c.m, seriesID)
		}
		return err
	}
	return nil
}

// save writes the map to disk. Caller holds mu.
func (c *corrections) save() error {
	if c.path == "" {
		return nil
	}
	if err := writeJSONAtomic(c.path, c.m); err != nil {
		return fmt.Errorf("saving corrections: %w", err)
	}
	return nil
}

// writeJSONAtomic writes v as indented JSON via a temp file and rename, so a
// crash mid-write never leaves a half-written file behind.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// correctionResult says what a correction changed.
type correctionResult struct {
	SeriesID    string `json:"seriesId"`
	Name        string `json:"name"`
	Entity      string `json:"entity,omitempty"`      // empty when the correction was removed
	Previous    string `json:"previous,omitempty"`    // what the event was tagged before
	PreviousVia string `json:"previousVia,omitempty"` // and how
}

// applyCorrection pins seriesID to entity (a configured entity name, any case;
// "" or "none" removes the pin), saves it, and re-tags the cached events so
// the change is visible at once. Setting needs the series in the current
// window; removing works on any saved correction. Errors are written for the
// person making the change.
func applyCorrection(c *classifier, st *store, loc *time.Location, seriesID, entity string) (correctionResult, error) {
	if c == nil {
		return correctionResult{}, fmt.Errorf("entity tagging is off: no entities are configured")
	}
	id := strings.TrimSpace(seriesID)
	resp, _, _, _ := st.get()
	var ev *Event
	for i := range resp.Events {
		if resp.Events[i].SeriesId == id {
			ev = &resp.Events[i]
			break
		}
	}

	entity = strings.TrimSpace(entity)
	if strings.EqualFold(entity, "none") {
		entity = ""
	}
	name := ""
	res := correctionResult{SeriesID: id}
	switch {
	case ev != nil:
		name = ev.Name
		res.Previous, res.PreviousVia = ev.Entity, ev.EntityVia
	case entity == "":
		fix, ok := c.fixes.entry(id)
		if !ok {
			return res, fmt.Errorf("no correction saved for seriesId %q", id)
		}
		name = fix.Name
		res.Previous, res.PreviousVia = fix.Entity, viaManual
	default:
		return res, fmt.Errorf("no event with seriesId %q in the current calendar window", id)
	}
	if entity != "" {
		i, ok := c.lookup(entity)
		if !ok {
			names := make([]string, len(c.entities))
			for j, e := range c.entities {
				names[j] = e.Name
			}
			return res, fmt.Errorf("unknown entity %q; use one of: %s", entity, strings.Join(names, ", "))
		}
		entity = c.entities[i].Name
	}
	if err := c.fixes.set(id, entity, name); err != nil {
		return res, fmt.Errorf("could not save the correction: %v", err)
	}
	st.reclassify(c, loc)
	res.Name, res.Entity = name, entity
	return res, nil
}
