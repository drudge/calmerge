package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func memClassifier(t *testing.T, path string, day time.Time) *classifier {
	t.Helper()
	c := newClassifier([]Entity{
		{Name: "Globex Records", Keywords: []string{"Globex"}},
		{Name: "Hooli", Keywords: []string{"Hooli"}},
		{Name: "Northwind Holdings", Feeds: []string{"Work"}},
	})
	fixes, _ := loadCorrections("")
	mem, err := loadLessons(path, 365)
	if err != nil {
		t.Fatal(err)
	}
	c.fixes, c.memory = fixes, mem
	c.clock = func() time.Time { return day }
	return c
}

func memEvent(name string, att ...Attendee) Event {
	return Event{Name: name, SeriesId: seriesID(name), sig: buildSignals("Work", name, "", "", nil, "", att, "")}
}

// TestLessonsOutliveTheWindow: what a meeting taught in September still
// counts in November, after that meeting has left the fetch window.
func TestLessonsOutliveTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lessons.json")
	anne := Attendee{Email: "anne@initech.example"}
	jacob := Attendee{Email: "jacob@globexrecords.example"}
	sam := Attendee{Email: "scarter@hooli.example"}

	sept := memClassifier(t, path, time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC))
	sept.classify([]Event{
		memEvent("Globex VIP kickoff", anne, jacob),
		memEvent("Hooli weekly", sam),
	})
	if sept.memory.len() != 2 {
		t.Fatalf("remembered %d lessons, want 2", sept.memory.len())
	}

	// Weeks later, fresh process, neither meeting is on the calendar any more.
	nov := memClassifier(t, path, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	evs := []Event{memEvent("Loyalty contract chat", anne, jacob)}
	nov.classify(evs)
	if evs[0].Entity != "Globex Records" || evs[0].EntityVia != viaLearned {
		t.Errorf("=> %q via %q, want Globex Records via learned from memory", evs[0].Entity, evs[0].EntityVia)
	}
}

// TestLessonsWindowBeatsMemory: removing a correction on a meeting that's still
// on the calendar stops it teaching, instead of the old label living on.
func TestLessonsWindowBeatsMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lessons.json")
	c := memClassifier(t, path, time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC))
	evs := []Event{memEvent("Offsite fun times")}
	_ = c.fixes.set(evs[0].SeriesId, "Globex Records", evs[0].Name)
	c.classify(evs)
	if c.memory.len() != 1 {
		t.Fatalf("correction not remembered")
	}

	_ = c.fixes.set(evs[0].SeriesId, "", "")
	c.classify(evs)
	if c.memory.len() != 0 {
		t.Errorf("removed correction still remembered")
	}
	if evs[0].EntityVia != viaFeed {
		t.Errorf("via %q, want feed after the correction is gone", evs[0].EntityVia)
	}
}

func TestLessonsPruneAndQuietSaves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lessons.json")
	day := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	c := memClassifier(t, path, day)
	c.memory.put("old", lesson{Entity: "Hooli", Via: viaRule, LastSeen: "2025-01-01", Features: []string{"t:x"}})
	c.memory.put("recent", lesson{Entity: "Hooli", Via: viaRule, LastSeen: "2026-06-01", Features: []string{"t:y"}})
	c.classify(nil)
	if c.memory.len() != 1 {
		t.Errorf("after prune: %d lessons, want 1 (the year-old one dropped)", c.memory.len())
	}

	// A refresh that changes nothing doesn't rewrite the file.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	past := st.ModTime().Add(-time.Hour)
	_ = os.Chtimes(path, past, past)
	c.classify(nil)
	if st, _ := os.Stat(path); !st.ModTime().Equal(past) {
		t.Error("quiet refresh rewrote the lessons file")
	}
}

func TestLessonsCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lessons.json")
	_ = os.WriteFile(path, []byte("nope"), 0o644)
	if _, err := loadLessons(path, 365); err == nil {
		t.Error("corrupt lessons file should fail loudly")
	}
}
