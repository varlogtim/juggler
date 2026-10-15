package app

import (
	"strings"
	"testing"
	"time"

	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/sway"
)

// lotTree builds a tree with one root per name, parked in a lot workspace,
// each holding an opencode window.
func lotTree(names ...string) *sway.Node {
	lot := &sway.Node{ID: 10, Type: "con", Layout: "tabbed", Marks: []string{layout.LotMark}}
	for i, n := range names {
		id := int64(100 + 10*i)
		lot.Nodes = append(lot.Nodes, &sway.Node{ID: id, Type: "con", Layout: "splith", Marks: []string{layout.RootMark(n)}, Nodes: []*sway.Node{
			{ID: id + 1, Type: "con", Layout: "stacked", Marks: []string{layout.LeftMark(n)}, Nodes: []*sway.Node{{ID: id + 2, Type: "con", AppID: layout.OpencodeAppID(n), PID: 1}}},
			{ID: id + 3, Type: "con", Layout: "stacked", Marks: []string{layout.RightMark(n)}, Nodes: []*sway.Node{{ID: id + 4, Type: "con", AppID: "jug-term:" + n, PID: 1}}},
		}})
	}
	return &sway.Node{Type: "root", Nodes: []*sway.Node{{ID: 1, Type: "output", Nodes: []*sway.Node{
		{ID: 2, Type: "workspace", Name: "WS+", Num: -1, Nodes: []*sway.Node{lot}},
	}}}}
}

func TestHibernateDue(t *testing.T) {
	a := newApp(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.Local)
	day := 24 * time.Hour
	mk := func(id, desc string) *store.Workstream {
		w, err := a.Create(CreateOptions{Desc: desc, Jira: id})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	closeTicket := func(w *store.Workstream, at time.Time) {
		r := w.IdentityRef()
		r.SetClosed(true, at)
		a.Store.Save(w)
	}
	due := mk("T-1", "ticket closed long ago") // finished 9d, shown 10d ago -> due
	closeTicket(due, now.Add(-9*day))
	fresh := mk("T-2", "closed an hour ago") // finished too recently
	closeTicket(fresh, now.Add(-time.Hour))
	recent := mk("T-3", "shown this morning") // finished 9d, but looked at since
	closeTicket(recent, now.Add(-9*day))
	open := mk("T-4", "still open") // not finished
	completed := mk("T-5", "completed by hand 3d ago")
	completed.Completed = now.Add(-3 * day)
	a.Store.Save(completed)
	displayed := mk("T-6", "the one on screen") // finished, but displayed
	closeTicket(displayed, now.Add(-9*day))
	noWindows := mk("T-7", "finished, no windows") // nothing to give back
	closeTicket(noWindows, now.Add(-9*day))
	asleep := mk("T-8", "already hibernated")
	closeTicket(asleep, now.Add(-9*day))
	legacy := mk("T-9", "closed before ClosedAt existed") // Closed without ClosedAt: Updated stands in
	lr := legacy.IdentityRef()
	lr.Closed, lr.Updated = true, now.Add(-5*day)
	a.Store.Save(legacy)

	st := &store.State{Displayed: displayed.Name(), Streams: map[string]*store.StreamState{
		due.Name():    {Shown: now.Add(-10 * day).Format(time.RFC3339)},
		recent.Name(): {Shown: now.Add(-12 * time.Hour).Format(time.RFC3339)},
		asleep.Name(): {Hibernated: now.Add(-day).Format(time.RFC3339)},
	}}
	tree := lotTree(due.Name(), fresh.Name(), recent.Name(), open.Name(), completed.Name(), displayed.Name(), asleep.Name(), legacy.Name())
	all, _ := a.Store.List()

	got := map[string]string{}
	for _, c := range hibernateDue(all, st, tree, day, now) {
		got[c.w.ID] = c.why
	}
	want := map[string]bool{"T-1": true, "T-5": true, "T-9": true}
	for id := range got {
		if !want[id] {
			t.Errorf("%s should not be due: %s", id, got[id])
		}
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("%s should be due; got %v", id, got)
		}
	}
	if !strings.Contains(got["T-1"], "finished 9d ago") || !strings.Contains(got["T-1"], "not shown for 10d") {
		t.Errorf("T-1 why: %q", got["T-1"])
	}
	if !strings.Contains(got["T-5"], "never shown") {
		t.Errorf("T-5 why: %q", got["T-5"])
	}
	// a longer grace period keeps the 3-day-old completion and the 5-day-old legacy close
	if cs := hibernateDue(all, st, tree, 7*day, now); len(cs) != 1 || cs[0].w.ID != "T-1" {
		t.Errorf("7d grace: %+v", cs)
	}
}

func TestFinishedSinceAndClosedAt(t *testing.T) {
	now := time.Now()
	w := &store.Workstream{ID: "X-1", Refs: []store.Ref{{Type: "jira", Key: "X-1"}}}
	if !w.FinishedSince().IsZero() {
		t.Fatal("open workstream has a finished time")
	}
	r := &w.Refs[0]
	if !r.SetClosed(true, now.Add(-2*time.Hour)) || r.SetClosed(true, now) || r.ClosedAt != now.Add(-2*time.Hour) {
		t.Fatalf("SetClosed edge: %+v", r)
	}
	if w.FinishedSince() != now.Add(-2*time.Hour) {
		t.Fatalf("finished since: %v", w.FinishedSince())
	}
	w.Completed = now.Add(-5 * time.Hour) // completed before the ticket closed: the earlier one counts
	if w.FinishedSince() != w.Completed {
		t.Fatalf("earliest wins: %v", w.FinishedSince())
	}
	if !r.SetClosed(false, now) || !r.ClosedAt.IsZero() {
		t.Fatalf("reopen clears ClosedAt: %+v", r)
	}
	if got := store.Age(30 * time.Hour); got != "30h" {
		t.Fatal(got)
	}
	if got := store.Age(9 * 24 * time.Hour); got != "9d" {
		t.Fatal(got)
	}
}
