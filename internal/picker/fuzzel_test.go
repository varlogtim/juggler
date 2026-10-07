package picker

import (
	"strings"
	"testing"
	"time"

	"github.com/varlogtim/juggler/internal/store"
)

func entry(id, desc string, displayed, live bool, shown, created time.Time) Entry {
	return Entry{
		W:         &store.Workstream{ID: id, Desc: desc, Category: "work", Created: created, Dir: "/nonexistent/" + id},
		Displayed: displayed,
		Live:      live,
		Shown:     shown,
	}
}

func TestOrderingAndRows(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	es := []Entry{
		entry("C", "old idle", false, false, time.Time{}, t0.Add(-48*time.Hour)),
		entry("B", "parked recent", false, true, t0.Add(2*time.Hour), t0),
		entry("D", "new idle", false, false, time.Time{}, t0.Add(1*time.Hour)),
		entry("A", "displayed", true, false, t0.Add(1*time.Hour), t0),
		entry("E", "parked older", false, true, t0.Add(1*time.Hour), t0),
	}
	got := Ordered(es)
	var ids []string
	for _, e := range got {
		ids = append(ids, e.W.ID)
	}
	want := "A B E D C" // displayed, parked by shown desc, rest by created desc
	if strings.Join(ids, " ") != want {
		t.Fatalf("order %v, want %s", ids, want)
	}

	lines := Lines(es)
	if len(lines) != len(es)+1 || lines[len(lines)-1] != NewMarker {
		t.Fatalf("lines: %q", lines)
	}
	if !strings.HasPrefix(lines[0], "● A") || !strings.HasPrefix(lines[1], "◐ B") || !strings.HasPrefix(lines[3], "○ D") {
		t.Fatalf("glyphs: %q", lines[:4])
	}
	// the description column is aligned: every row has the same prefix width
	// up to the description
	col := strings.Index(lines[0], "displayed")
	for _, l := range lines[1:4] {
		if !strings.Contains(l[col:], " ") && len(l) < col {
			t.Errorf("row not aligned: %q", l)
		}
	}
}

func TestFinishedHiddenUnlessLive(t *testing.T) {
	t0 := time.Now()
	closed := func(id string, live bool) Entry {
		e := entry(id, "x", false, live, t0, t0)
		e.W.Refs = []store.Ref{{Type: "jira", Key: id, URL: "https://x/" + id, Closed: true}}
		return e
	}
	completed := entry("C", "x", false, false, t0, t0)
	completed.W.Completed = t0
	es := []Entry{entry("A", "open", false, false, t0, t0), closed("B", false), closed("L", true), completed}
	lines := Lines(es)
	if len(lines) != 3 { // A, L (live), + new
		t.Fatalf("lines: %q", lines)
	}
	if !strings.HasPrefix(lines[0], "✓ L") || !strings.HasPrefix(lines[1], "○ A") {
		t.Fatalf("order/glyphs: %q", lines)
	}
	if o := Ordered(es); len(o) != 2 {
		t.Fatalf("ordered: %d", len(o))
	}
}

func TestRowShowsRefs(t *testing.T) {
	e := entry("AISW-1", "x", false, false, time.Time{}, time.Now())
	e.W.Refs = []store.Ref{
		{Type: "jira", Key: "AISW-1", Status: "Blocked"},
		{Type: "pr", Key: "org/repo#897", Status: "open"},
	}
	r := row(e)
	if r[2] != "Blocked" || r[3] != "PR#897 open" || r[5] != "work" {
		t.Fatalf("row = %q", r)
	}
}
