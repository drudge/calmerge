package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"
)

// lesson is one confidently-labeled event series, kept so it keeps teaching
// the model after it scrolls out of the fetch window. That's what lets the
// classifier remember months of ongoing work instead of only the next 30 days.
type lesson struct {
	Entity   string   `json:"entity"`
	Via      string   `json:"via"`  // manual | category | rule
	Name     string   `json:"name"` // event title, for humans reading the file
	LastSeen string   `json:"lastSeen"`
	Features []string `json:"features"`
}

// lessons is the saved seriesId -> lesson map. Only the refresher touches it,
// but the lock keeps it safe to read from elsewhere later.
type lessons struct {
	mu     sync.Mutex
	path   string
	maxAge int // days a lesson survives without being seen again; 0 = forever
	m      map[string]lesson
	dirty  bool
}

// loadLessons reads the lessons file. Missing is fine; corrupt is an error so
// the next save can't wipe the history.
func loadLessons(path string, maxAgeDays int) (*lessons, error) {
	l := &lessons{path: path, maxAge: maxAgeDays, m: map[string]lesson{}}
	if path == "" {
		return l, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading lessons %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &l.m); err != nil {
		return nil, fmt.Errorf("lessons %s is not valid JSON: %w", path, err)
	}
	if l.m == nil {
		l.m = map[string]lesson{}
	}
	return l, nil
}

func (l *lessons) len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// put records what a series in the current window taught this refresh.
func (l *lessons) put(key string, ls lesson) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	old, ok := l.m[key]
	if ok && old.Entity == ls.Entity && old.Via == ls.Via && old.Name == ls.Name &&
		old.LastSeen == ls.LastSeen && slices.Equal(old.Features, ls.Features) {
		return
	}
	l.m[key] = ls
	l.dirty = true
}

// drop forgets a series. Used when a series is back in the window but nothing
// firm labels it any more (say, its correction was removed): what the window
// says now beats what was remembered.
func (l *lessons) drop(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.m[key]; ok {
		delete(l.m, key)
		l.dirty = true
	}
}

// each calls fn for every remembered lesson.
func (l *lessons) each(fn func(key string, ls lesson)) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, ls := range l.m {
		fn(k, ls)
	}
}

// prune forgets lessons not seen within maxAge days of today.
func (l *lessons) prune(today time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxAge <= 0 {
		return
	}
	cutoff := today.AddDate(0, 0, -l.maxAge).Format("2006-01-02")
	for k, ls := range l.m {
		if ls.LastSeen < cutoff {
			delete(l.m, k)
			l.dirty = true
		}
	}
}

// saveIfChanged writes the file only when something changed, so a quiet
// refresh doesn't touch the disk.
func (l *lessons) saveIfChanged() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.dirty || l.path == "" {
		return nil
	}
	if err := writeJSONAtomic(l.path, l.m); err != nil {
		return fmt.Errorf("saving lessons: %w", err)
	}
	l.dirty = false
	return nil
}
