package app

import (
	"errors"
	"testing"
	"time"

	"github.com/varlogtim/juggler/internal/store"
)

func str(s string) *string { return &s }

func TestGroupsUnionOrderAndCounts(t *testing.T) {
	a := newApp(t)
	w1, _ := a.Create(CreateOptions{Desc: "one", Jira: "P-1", Groups: []string{"s20"}})
	w2, _ := a.Create(CreateOptions{Desc: "two", Jira: "P-2", Groups: []string{"s20", "proj-x"}})
	w3, _ := a.Create(CreateOptions{Desc: "three"}) // ungrouped
	_ = w3
	today := time.Now().Format(store.DateLayout)
	if _, err := a.SetGroup("s20", GroupPatch{Kind: str("sprint"), Start: str("2026-01-01"), End: str(today)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetGroup("s21", GroupPatch{Kind: str("sprint"), Start: str("2099-01-01"), End: str("2099-01-14")}); err != nil {
		t.Fatal(err) // registered, empty
	}
	gs, err := a.Groups(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 || gs[0].Name != "s20" || gs[1].Name != "proj-x" {
		t.Fatalf("groups(false): %+v", names(gs))
	}
	if gs[0].Count != 2 || !gs[0].Registered || !gs[0].Current || gs[0].DaysLeft == nil || *gs[0].DaysLeft != 1 {
		t.Fatalf("s20: %+v", gs[0])
	}
	if gs[1].Registered || gs[1].Count != 1 || gs[1].Members[0] != w2.Name() {
		t.Fatalf("proj-x: %+v", gs[1])
	}
	all, _ := a.Groups(true)
	if len(all) != 3 || all[1].Name != "s21" || all[1].Count != 0 || all[1].Current {
		t.Fatalf("groups(true): %+v", names(all))
	}
	if g, err := a.Group("proj-x"); err != nil || g.Count != 1 {
		t.Fatalf("Group: %+v %v", g, err)
	}
	if _, err := a.Group("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// tag / untag / set
	if err := a.Tag(w1, "proj-x", "proj-x"); err != nil {
		t.Fatal(err)
	}
	w1b, _ := a.Resolve("P-1")
	if len(w1b.Groups) != 2 {
		t.Fatalf("tag dedupe: %v", w1b.Groups)
	}
	if err := a.Untag(w1b, "s20"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetGroups(w2, []string{"backlog"}); err != nil {
		t.Fatal(err)
	}
	gs, _ = a.Groups(false)
	if len(gs) != 2 || gs[0].Name != "backlog" && gs[1].Name != "backlog" {
		t.Fatalf("after retag: %v", names(gs))
	}
	if err := a.Tag(w1b, "bad/name"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid tag accepted: %v", err)
	}
	// remove: registry only vs untag
	if n, err := a.RemoveGroup("s21", false); err != nil || n != 0 {
		t.Fatalf("remove empty registered: %d %v", n, err)
	}
	if n, err := a.RemoveGroup("proj-x", true); err != nil || n != 1 {
		t.Fatalf("remove+untag: %d %v", n, err)
	}
	if _, err := a.RemoveGroup("proj-x", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	if _, err := a.SetGroup("x", GroupPatch{Start: str("2026-02-01"), End: str("2026-01-01")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad dates accepted: %v", err)
	}
	in := a.InfoOf(w2, nil, nil)
	if len(in.Groups) != 1 || in.Groups[0] != "backlog" {
		t.Fatalf("info groups: %v", in.Groups)
	}
}

func names(gs []GroupInfo) []string {
	var n []string
	for _, g := range gs {
		n = append(n, g.Name)
	}
	return n
}
