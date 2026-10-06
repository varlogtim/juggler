package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/sway"
)

func newApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Default()
	cfg.Root = filepath.Join(t.TempDir(), "ws")
	cfg.StateDir = filepath.Join(t.TempDir(), "st")
	cfg.IDPrefix = "me"
	cfg.JiraBaseURL = "https://jira.example.com/"
	cfg.Repos = nil
	a := New(cfg)
	a.DialFunc = func() (*sway.Conn, error) { return nil, errors.New("no sway") }
	return a
}

func TestCreateRules(t *testing.T) {
	a := newApp(t)
	w, err := a.Create(CreateOptions{Desc: "Fix the thing", Jira: "proj-7"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Name() != "work_PROJ-7_fix-the-thing" || w.Category != "work" || w.ID != "PROJ-7" {
		t.Fatalf("ticket create: %s %s %s", w.Name(), w.Category, w.ID)
	}
	if r := w.Ref("jira"); r == nil || r.URL != "https://jira.example.com/browse/PROJ-7" {
		t.Fatalf("jira ref: %+v", w.Refs)
	}
	w2, err := a.Create(CreateOptions{Desc: "notes only"})
	if err != nil {
		t.Fatal(err)
	}
	if w2.Name() != "personal_me-0001_notes-only" {
		t.Fatalf("plain create: %s", w2.Name())
	}
	w3, _ := a.Create(CreateOptions{Desc: "forced", Jira: "PROJ-8", Category: "personal"})
	if w3.Category != "personal" {
		t.Fatalf("explicit category must win: %s", w3.Category)
	}
	if _, err := a.Create(CreateOptions{Desc: ""}); err == nil {
		t.Fatal("empty description accepted")
	}
	if _, err := a.Create(CreateOptions{Desc: "again", Jira: "PROJ-7"}); err == nil {
		t.Fatal("duplicate accepted")
	}
	for _, in := range []WorkstreamInfo{a.InfoOf(w, nil, nil)} {
		if in.State != "none" || in.HasCode || in.Git != nil || in.TodosOpen != 5 {
			t.Fatalf("info: %+v", in)
		}
	}
	a.Cfg.JiraBaseURL = ""
	if _, err := a.Create(CreateOptions{Desc: "x", Jira: "PROJ-9"}); err == nil {
		t.Fatal("jira without jira_base_url accepted")
	}
}

func TestRefs(t *testing.T) {
	a := newApp(t)
	w, _ := a.Create(CreateOptions{Desc: "refs"})
	if _, err := a.AddRef(w, "pr", "https://gh.example.com/o/r/pull/3", "", "open"); err != nil {
		t.Fatal(err)
	}
	if w.Ref("pr").Key != "o/r#3" {
		t.Fatalf("pr key %q", w.Ref("pr").Key)
	}
	// replace by key
	if _, err := a.AddRef(w, "pr", "https://gh.example.com/o/r/pull/3", "", "merged"); err != nil {
		t.Fatal(err)
	}
	if len(w.Refs) != 1 || w.Refs[0].Status != "merged" {
		t.Fatalf("replace: %+v", w.Refs)
	}
	if _, err := a.AddRef(w, "url", "nope", "", ""); err == nil {
		t.Fatal("non-url accepted")
	}
	if err := a.RemoveRef(w, "pr", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveRef(w, "pr", ""); err == nil {
		t.Fatal("removing a missing ref succeeded")
	}
	// persisted
	w2, _ := a.Resolve(w.ID)
	if len(w2.Refs) != 0 {
		t.Fatalf("refs not persisted: %+v", w2.Refs)
	}
}

func TestSetAndRemoveWithoutSway(t *testing.T) {
	a := newApp(t)
	w, _ := a.Create(CreateOptions{Desc: "before"})
	oldDir := w.Dir
	res, err := a.Set(nil, w, SetOptions{Jira: "proj-1", Desc: "after"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Moved || res.Name != "work_PROJ-1_after" || w.ID != "PROJ-1" || w.Category != "work" {
		t.Fatalf("set: %+v id=%s cat=%s", res, w.ID, w.Category)
	}
	if _, err := os.Stat(oldDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old dir still there")
	}
	if _, err := os.Stat(filepath.Join(w.Dir, "TODO.md")); err != nil {
		t.Fatal("TODO.md did not move")
	}
	// refs-only change does not move
	res, err = a.Set(nil, w, SetOptions{Jira: "PROJ-1"})
	if err != nil || res.Moved {
		t.Fatalf("refs-only set: %+v %v", res, err)
	}
	p := a.Plan(w)
	if p.HasWorktree || p.Name != w.Name() {
		t.Fatalf("plan: %+v", p)
	}
	if err := a.Remove(nil, w, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dir still there after remove")
	}
}

func TestTargetAndTodo(t *testing.T) {
	a := newApp(t)
	w, _ := a.Create(CreateOptions{Desc: "t"})
	t.Setenv("JUG_WORKSTREAM", "")
	if _, err := a.Target("", ""); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("want ErrNoTarget, got %v", err)
	}
	if got, _ := a.Target("", w.Name()); got.ID != w.ID {
		t.Fatal("displayed fallback")
	}
	t.Setenv("JUG_WORKSTREAM", w.ID)
	if got, _ := a.Target("", ""); got.ID != w.ID {
		t.Fatal("env fallback")
	}
	if err := a.TodoWrite(w, "- [ ] a\n- [ ] b\n"); err != nil {
		t.Fatal(err)
	}
	if txt, _ := a.TodoRead(w); txt != "- [ ] a\n- [ ] b\n" || w.OpenTodos() != 2 {
		t.Fatalf("todo roundtrip: %q %d", txt, w.OpenTodos())
	}
	st := a.Status()
	if st.Sway || st.Workstreams != 1 || st.SwayError == "" {
		t.Fatalf("status: %+v", st)
	}
}
