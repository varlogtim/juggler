package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Groups put workstreams into buckets — a sprint, a project, a date range,
// anything. Two halves, deliberately split:
//
//   - MEMBERSHIP is a tag on the workstream (Workstream.Groups, in its
//     workstream.toml). That is the truth for "which workstreams are in group
//     X": a group with no tagged workstreams simply has no members, removing
//     a workstream leaves nothing dangling, and renaming one moves its tags
//     along. Nothing lists members anywhere else.
//   - METADATA — what a group knows about itself (when it runs, what kind of
//     thing it is) — lives once, in <root>/groups.toml. Copying a sprint's
//     dates onto every workstream in it would mean N edits when the sprint
//     shifts. A group may be tagged but unregistered (no dates, sorted last)
//     or registered but empty (a sprint created ahead of time; not shown in
//     the main views).
//
// Groups are ordered by end date, then start, then name.

// Group is one registry entry.
type Group struct {
	Name  string `toml:"name" json:"name"`
	Kind  string `toml:"kind,omitempty" json:"kind,omitempty"` // sprint | project | bucket | … (free text)
	Desc  string `toml:"desc,omitempty" json:"desc,omitempty"`
	Start string `toml:"start,omitempty" json:"start,omitempty"` // YYYY-MM-DD
	End   string `toml:"end,omitempty" json:"end,omitempty"`     // YYYY-MM-DD
}

// GroupsFile is the registry file name, under the store root.
const GroupsFile = "groups.toml"

// DateLayout is the date format used in the registry and the API.
const DateLayout = "2006-01-02"

// ErrInvalidGroup marks validation failures.
var ErrInvalidGroup = errors.New("invalid group")

// NormalizeGroupName trims a group name and rejects what cannot be used as
// a tag, a file-friendly value and a URL path segment.
func NormalizeGroupName(name string) (string, error) {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return "", fmt.Errorf("%w: name is empty", ErrInvalidGroup)
	case strings.ContainsAny(n, "/\\\x00\n\r\t"):
		return "", fmt.Errorf("%w: name %q may not contain slashes or control characters", ErrInvalidGroup, n)
	case len(n) > 100:
		return "", fmt.Errorf("%w: name is longer than 100 characters", ErrInvalidGroup)
	}
	return n, nil
}

// Validate checks the entry.
func (g Group) Validate() error {
	if _, err := NormalizeGroupName(g.Name); err != nil {
		return err
	}
	var start, end time.Time
	var err error
	if g.Start != "" {
		if start, err = time.ParseInLocation(DateLayout, g.Start, time.Local); err != nil {
			return fmt.Errorf("%w: start %q is not YYYY-MM-DD", ErrInvalidGroup, g.Start)
		}
	}
	if g.End != "" {
		if end, err = time.ParseInLocation(DateLayout, g.End, time.Local); err != nil {
			return fmt.Errorf("%w: end %q is not YYYY-MM-DD", ErrInvalidGroup, g.End)
		}
	}
	if g.Start != "" && g.End != "" && end.Before(start) {
		return fmt.Errorf("%w: end %s is before start %s", ErrInvalidGroup, g.End, g.Start)
	}
	return nil
}

// StartTime / EndTime parse the dates (ok=false when unset or invalid).
func (g Group) StartTime() (time.Time, bool) { return parseDate(g.Start) }
func (g Group) EndTime() (time.Time, bool)   { return parseDate(g.End) }

func parseDate(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(DateLayout, s, time.Local)
	return t, err == nil
}

// IsCurrent reports whether now falls within [start, end] (whole days,
// inclusive). Both dates must be set.
func (g Group) IsCurrent(now time.Time) bool {
	start, ok1 := g.StartTime()
	end, ok2 := g.EndTime()
	if !ok1 || !ok2 {
		return false
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	return !day.Before(start) && !day.After(end)
}

// Less is the group ordering: by end date (dated first), then start, then
// name.
func (g Group) Less(o Group) bool {
	ge, gok := g.EndTime()
	oe, ook := o.EndTime()
	switch {
	case gok && !ook:
		return true
	case !gok && ook:
		return false
	case gok && ook && !ge.Equal(oe):
		return ge.Before(oe)
	}
	gs, gsok := g.StartTime()
	os_, osok := o.StartTime()
	switch {
	case gsok && !osok:
		return true
	case !gsok && osok:
		return false
	case gsok && osok && !gs.Equal(os_):
		return gs.Before(os_)
	}
	return strings.ToLower(g.Name) < strings.ToLower(o.Name)
}

// SortGroups orders gs in place (see Less).
func SortGroups(gs []Group) { sort.SliceStable(gs, func(i, j int) bool { return gs[i].Less(gs[j]) }) }

type groupsFile struct {
	Group []Group `toml:"group"`
}

// LoadGroups reads the registry; a missing file is an empty registry.
func (s Store) LoadGroups() ([]Group, error) {
	var f groupsFile
	if _, err := toml.DecodeFile(filepath.Join(s.Root, GroupsFile), &f); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", GroupsFile, err)
	}
	SortGroups(f.Group)
	return f.Group, nil
}

// SaveGroups writes the registry (sorted, atomically).
func (s Store) SaveGroups(gs []Group) error {
	for _, g := range gs {
		if err := g.Validate(); err != nil {
			return err
		}
	}
	gs = append([]Group(nil), gs...)
	SortGroups(gs)
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# juggler groups — metadata only; membership is the `groups` tag in each workstream.toml.\n")
	buf.WriteString("# Ordered by end date. See `jug group --help`.\n\n")
	if err := toml.NewEncoder(&buf).Encode(groupsFile{Group: gs}); err != nil {
		return err
	}
	tmp := filepath.Join(s.Root, GroupsFile+".tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Root, GroupsFile))
}

// NormalizeGroups trims, drops empties and duplicates, keeps order.
func NormalizeGroups(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// InGroup reports whether w is tagged with name.
func (w *Workstream) InGroup(name string) bool {
	for _, g := range w.Groups {
		if g == name {
			return true
		}
	}
	return false
}
