// Package journal is the append-only Taildrives Lifecycle Journal.
//
// Every share add/remove, every category-affecting operation, every
// destructive action (delete) writes one event. Entries land in
// ~/.config/taildrives/journal/YYYY-MM-DD.jsonl as one JSON object per
// line — easy to grep, easy to ship around, no parsing surprises.
package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one journal record.
type Entry struct {
	Timestamp time.Time `json:"ts"`
	Host      string    `json:"host"`     // device this happened on
	Actor     string    `json:"actor"`    // operator who triggered it (loginName)
	Action    string    `json:"action"`   // e.g. "share.add", "share.remove", "file.delete"
	Target    string    `json:"target"`   // path / share name / device name acted on
	Detail    string    `json:"detail,omitempty"`
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
}

// dir returns ~/.config/taildrives/journal (XDG-aware), creating it on demand.
func dir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	d := filepath.Join(base, "taildrives", "journal")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// fileFor returns the journal path for the given day.
func fileFor(t time.Time) string {
	return filepath.Join(dir(), t.UTC().Format("2006-01-02")+".jsonl")
}

// Append writes one entry to today's journal file. Safe to call from any
// goroutine — uses O_APPEND so concurrent writers don't tear lines.
func Append(e Entry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.Host == "" {
		e.Host, _ = os.Hostname()
	}
	f, err := os.OpenFile(fileFor(e.Timestamp), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return enc.Encode(e)
}

// Log is a convenience for the common success-or-error pattern.
func Log(actor, action, target, detail string, err error) {
	e := Entry{
		Actor:   actor,
		Action:  action,
		Target:  target,
		Detail:  detail,
		Success: err == nil,
	}
	if err != nil {
		e.Error = err.Error()
	}
	_ = Append(e)
}

// Read returns all entries from the given day's file, newest first.
func Read(day time.Time) ([]Entry, error) {
	data, err := os.ReadFile(fileFor(day))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	// newest first
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out, nil
}

// ReadRecent returns the last N days of entries (today + N-1 prior days),
// newest first, capped at maxEntries.
func ReadRecent(days, maxEntries int) ([]Entry, error) {
	var all []Entry
	for i := 0; i < days; i++ {
		day := time.Now().UTC().AddDate(0, 0, -i)
		entries, _ := Read(day)
		all = append(all, entries...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp.After(all[j].Timestamp) })
	if maxEntries > 0 && len(all) > maxEntries {
		all = all[:maxEntries]
	}
	return all, nil
}
