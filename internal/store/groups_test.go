package store

import (
	"errors"
	"testing"
	"time"
)

func TestGroupValidateAndNames(t *testing.T) {
	ok := []Group{{Name: "backlog"}, {Name: "PCFS-S20-26.09.23-Nebula", Start: "2026-09-23", End: "2026-10-06"}, {Name: "Team Nebula Sprint 3", End: "2026-12-01"}}
	for _, g := range ok {
		if err := g.Validate(); err != nil {
			t.Errorf("%+v: %v", g, err)
		}
	}
	bad := []Group{{Name: ""}, {Name: " "}, {Name: "a/b"}, {Name: "x", Start: "2026-1-1"}, {Name: "x", End: "yesterday"}, {Name: "x", Start: "2026-10-07", End: "2026-10-06"}}
	for _, g := range bad {
		if err := g.Validate(); !errors.Is(err, ErrInvalidGroup) {
			t.Errorf("%+v should be invalid, got %v", g, err)
		}
	}
	if n, _ := NormalizeGroupName("  sprint 1 "); n != "sprint 1" {
		t.Errorf("trim: %q", n)
	}
}

func TestGroupOrderAndCurrent(t *testing.T) {
	gs := []Group{
		{Name: "zeta-undated"},
		{Name: "backlog"},
		{Name: "s21", Start: "2026-10-07", End: "2026-10-20"},
		{Name: "s20", Start: "2026-09-23", End: "2026-10-06"},
		{Name: "proj-ends-same-day", Start: "2026-01-01", End: "2026-10-20"},
	}
	SortGroups(gs)
	var names []string
	for _, g := range gs {
		names = append(names, g.Name)
	}
	want := "s20 proj-ends-same-day s21 backlog zeta-undated" // dated by end (ties: earlier start first), then undated by name
	if got := joinNames(names); got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
	now := time.Date(2026, 10, 5, 23, 59, 0, 0, time.Local)
	if !gs[0].IsCurrent(now) || gs[2].IsCurrent(now) || gs[3].IsCurrent(now) {
		t.Fatalf("current flags wrong: s20=%v s21=%v backlog=%v", gs[0].IsCurrent(now), gs[2].IsCurrent(now), gs[3].IsCurrent(now))
	}
	if !gs[0].IsCurrent(time.Date(2026, 10, 6, 0, 0, 1, 0, time.Local)) {
		t.Fatal("end date should be inclusive")
	}
}

func joinNames(n []string) string {
	s := ""
	for i, x := range n {
		if i > 0 {
			s += " "
		}
		s += x
	}
	return s
}

func TestGroupsRegistryRoundTrip(t *testing.T) {
	s := Store{Root: t.TempDir()}
	gs, err := s.LoadGroups()
	if err != nil || len(gs) != 0 {
		t.Fatalf("empty registry: %v %v", gs, err)
	}
	in := []Group{{Name: "backlog", Kind: "bucket"}, {Name: "s20", Kind: "sprint", Start: "2026-09-23", End: "2026-10-06", Desc: "Team Nebula"}}
	if err := s.SaveGroups(in); err != nil {
		t.Fatal(err)
	}
	gs, err = s.LoadGroups()
	if err != nil || len(gs) != 2 || gs[0].Name != "s20" || gs[1].Name != "backlog" || gs[0].Desc != "Team Nebula" {
		t.Fatalf("round trip: %+v %v", gs, err)
	}
	if err := s.SaveGroups([]Group{{Name: "bad/name"}}); err == nil {
		t.Fatal("invalid group saved")
	}
	// the registry file must not be mistaken for a workstream
	if all, err := s.List(); err != nil || len(all) != 0 {
		t.Fatalf("List saw the registry: %v %v", all, err)
	}
}

func TestWorkstreamGroupsNormalized(t *testing.T) {
	s := Store{Root: t.TempDir()}
	w := &Workstream{ID: "x-1", Category: "work", Desc: "g", Created: time.Now(), Groups: []string{" s20 ", "", "s20", "backlog"}}
	if err := s.Save(w); err != nil {
		t.Fatal(err)
	}
	w2, _ := s.Resolve("x-1")
	if len(w2.Groups) != 2 || w2.Groups[0] != "s20" || w2.Groups[1] != "backlog" || !w2.InGroup("backlog") || w2.InGroup("nope") {
		t.Fatalf("groups: %v", w2.Groups)
	}
}
