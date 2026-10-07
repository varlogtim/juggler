package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSlugAndDirName(t *testing.T) {
	cases := map[string]string{
		"disagg toggle requires pause":                 "disagg-toggle-requires-pause",
		"  Reduce DB round-trips in GET /deployments ": "reduce-db-round-trips-in-get-deployments",
		"":              "untitled",
		"!!!":           "untitled",
		"MakeSomeThing": "makesomething",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := DirName("work", "AISW-53270", "disagg toggle requires pause"); got != "work_AISW-53270_disagg-toggle-requires-pause" {
		t.Errorf("DirName = %q", got)
	}
	long := Slug("a very long description that goes on and on and on and on and on and on")
	if len(long) > 48 {
		t.Errorf("slug not truncated: %d chars", len(long))
	}
}

func newStore(t *testing.T) Store {
	t.Helper()
	s := Store{Root: t.TempDir()}
	for _, w := range []*Workstream{
		{ID: "AISW-53270", Category: "work", Desc: "disagg toggle requires pause", Created: time.Now()},
		{ID: "AISW-52787", Category: "work", Desc: "deprecate all projects", Created: time.Now()},
		{ID: "tim-0001", Category: "personal", Desc: "juggler dev", Created: time.Now()},
		{ID: "tim-0003", Category: "personal", Desc: "gap in numbering", Created: time.Now()},
	} {
		if err := s.Save(w); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestSaveCreatesLayout(t *testing.T) {
	s := newStore(t)
	w, err := s.Resolve("AISW-53270")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(w.Dir) != "work_AISW-53270_disagg-toggle-requires-pause" {
		t.Errorf("dir %q", w.Dir)
	}
	if n := w.OpenTodos(); n != 5 {
		t.Errorf("default TODO.md has %d open items, want 5", n)
	}
	if _, err := filepath.Glob(w.NotesDir()); err != nil {
		t.Error(err)
	}
}

func TestResolve(t *testing.T) {
	s := newStore(t)
	for q, want := range map[string]string{
		"AISW-53270": "AISW-53270",
		"aisw-52787": "AISW-52787",
		"work_AISW-53270_disagg-toggle-requires-pause": "AISW-53270",
		"tim-0001":          "tim-0001",
		"personal_tim-0003": "tim-0003",
		"AISW-532":          "AISW-53270", // unique prefix
	} {
		w, err := s.Resolve(q)
		if err != nil {
			t.Errorf("Resolve(%q): %v", q, err)
			continue
		}
		if w.ID != want {
			t.Errorf("Resolve(%q) = %s, want %s", q, w.ID, want)
		}
	}
	for _, q := range []string{"AISW", "tim", "nope"} {
		if _, err := s.Resolve(q); err == nil {
			t.Errorf("Resolve(%q) should fail (ambiguous or missing)", q)
		}
	}
}

func TestNextManualID(t *testing.T) {
	s := newStore(t)
	id, err := s.NextManualID("tim")
	if err != nil {
		t.Fatal(err)
	}
	if id != "tim-0004" {
		t.Errorf("NextManualID = %q, want tim-0004", id)
	}
}

func TestRefLookupAndCodeDir(t *testing.T) {
	w := &Workstream{Dir: "/x", Refs: []Ref{{Type: "jira", Key: "AISW-1"}, {Type: "pr", Key: "o/r#2"}}}
	if w.Ref("pr").Key != "o/r#2" || w.Ref("issue") != nil {
		t.Error("Ref lookup")
	}
	if w.ResolvedCodeDir() != "/x" {
		t.Errorf("empty code dir should be the workstream dir, got %q", w.ResolvedCodeDir())
	}
}

func TestFinishedAndUntouched(t *testing.T) {
	s := newStore(t)
	w, _ := s.Resolve("AISW-53270")
	if w.Finished() || !w.Untouched() {
		t.Fatalf("fresh: finished=%v untouched=%v", w.Finished(), w.Untouched())
	}
	w.Refs = []Ref{{Type: "pr", Key: "o/r#1", URL: "https://x/pull/1", Closed: true}}
	if w.Finished() {
		t.Fatal("a closed PR must not finish a workstream")
	}
	w.Refs = append(w.Refs, Ref{Type: "jira", Key: "AISW-53270", URL: "https://x/browse/AISW-53270", Closed: true})
	if !w.Finished() || !w.TicketClosed() || w.IdentityRef().Key != "AISW-53270" {
		t.Fatal("closed ticket should finish")
	}
	w.Refs[1].Closed = false
	w.Completed = time.Now()
	if !w.Finished() || w.TicketClosed() || w.Untouched() {
		t.Fatal("completed should finish and count as touched")
	}
	w.Completed = time.Time{}
	os.WriteFile(w.TodoPath(), []byte("- [ ] mine\n"), 0o644)
	if w.Untouched() {
		t.Fatal("edited TODO.md should count as touched")
	}
	os.WriteFile(w.TodoPath(), []byte(DefaultTodo(w)), 0o644)
	os.WriteFile(filepath.Join(w.NotesDir(), "a.md"), []byte("x"), 0o644)
	if w.Untouched() {
		t.Fatal("a note should count as touched")
	}
}
