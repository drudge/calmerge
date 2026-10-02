package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// loadSeed fills the store from a saved /events payload instead of fetching
// feeds, for working on the UI against real-shaped data without the feed URLs
// (run with -seed file.json). The classifier inputs aren't in the JSON, so
// they're rebuilt from the fields that are, then everything is re-tagged with
// the current entities.
func loadSeed(path string, cfg config, st *store) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var resp Response
	if err := json.Unmarshal(b, &resp); err != nil {
		return fmt.Errorf("seed %s: %w", path, err)
	}
	for i := range resp.Events {
		e := &resp.Events[i]
		e.sig = buildSignals(e.Source, e.Name, e.Agenda, e.Address, e.Categories, e.Organizer, e.Attendees, e.MeetURL)
		e.Entity, e.EntityColor, e.EntityVia, e.EntityWhy = "", "", "", ""
	}
	cfg.classifier.classify(resp.Events)
	resp.Days = groupDays(resp.Events, time.Now().In(cfg.loc), cfg.loc)
	resp.Entities = cfg.classifier.Entities()
	out, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	st.set(resp, out)
	return nil
}
