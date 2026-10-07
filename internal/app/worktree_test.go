package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varlogtim/juggler/internal/config"
)

// gitRepo makes a main checkout with one commit holding components/x/README,
// so a worktree of it has a subdir to point at.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mono")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(filepath.Join(dir, "components", "x"), 0o755)
	os.WriteFile(filepath.Join(dir, "components", "x", "README"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "README"), []byte("mono\n"), 0o644)
	run("init", "-q", "-b", "develop")
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

// A worktree of a monorepo whose code_dir is a component inside it: windows
// start in the component, but the worktree — seeds, the dirty guard,
// removal through git — is the root. This is what the hand-edited
// code_dir used to break: with .git two levels up, removal skipped git and
// the dirty check.
func TestWorktreeSubdir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	main := gitRepo(t)
	a := newApp(t)
	seedDir := filepath.Join(t.TempDir(), "seed")
	os.MkdirAll(seedDir, 0o755)
	os.WriteFile(filepath.Join(seedDir, ".envrc"), []byte("# {{id}} in {{code_dir}}\n"), 0o644)
	a.Cfg.Repos = map[string]config.Repo{"mono": {Path: main, Remote: "origin", Subdir: "components/x", SeedDir: seedDir}}

	w, err := a.Create(CreateOptions{Desc: "component work", Jira: "MONO-1"})
	if err != nil {
		t.Fatal(err)
	}
	// the repo's default subdir applies
	res, err := a.AddWorktree(w, WorktreeOptions{Repo: "mono", NoFetch: true, Base: "develop"})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(w.Dir, "src", "mono")
	if res.Path != root || res.CodeDir != filepath.Join(root, "components", "x") || w.CodeDir != filepath.Join("src", "mono", "components", "x") {
		t.Fatalf("paths: %+v code_dir=%s", res, w.CodeDir)
	}
	// seeds land at the root, templated with the code dir
	if b, err := os.ReadFile(filepath.Join(root, ".envrc")); err != nil || !strings.Contains(string(b), "MONO-1 in "+res.CodeDir) {
		t.Fatalf("seed at root: %q %v (seeded: %v)", b, err, res.Seeded)
	}
	if _, err := os.Stat(filepath.Join(res.CodeDir, ".envrc")); err == nil {
		t.Fatal("seed landed in the subdir")
	}
	if got, ok := a.WorktreeRoot(w); !ok || got != root {
		t.Fatalf("worktree root: %q %v", got, ok)
	}
	if g := a.Git(w); g == nil || !g.Linked || g.Root != root || g.Branch != "MONO-1" || g.Main != main {
		t.Fatalf("git info from the subdir: %+v", g)
	}
	if a.RepoNameOf(w) != "mono" {
		t.Fatalf("repo name: %q", a.RepoNameOf(w))
	}

	// a change inside the component is seen by the dirty guard at the root
	os.WriteFile(filepath.Join(res.CodeDir, "README"), []byte("changed\n"), 0o644)
	p := a.Plan(w)
	if !p.HasWorktree || p.Worktree != root || p.Branch != "MONO-1" || p.Dirty != "components/x/README" {
		t.Fatalf("plan: %+v", p)
	}
	if err := a.Remove(nil, w, false); !errors.Is(err, ErrDirty) {
		t.Fatalf("dirty worktree removed without force: %v", err)
	}

	// --subdir on set: "." is the root, a missing dir is refused, a sibling works
	if _, err := a.Set(nil, w, SetOptions{Subdir: strPtr("nope")}); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing subdir accepted: %v", err)
	}
	if _, err := a.Set(nil, w, SetOptions{Subdir: strPtr("../../escape")}); err == nil {
		t.Fatal("escaping subdir accepted")
	}
	sr, err := a.Set(nil, w, SetOptions{Subdir: strPtr(".")})
	if err != nil || sr.Moved || sr.CodeDir != root || w.CodeDir != filepath.Join("src", "mono") {
		t.Fatalf("set subdir .: %+v %v code_dir=%s", sr, err, w.CodeDir)
	}
	sr, err = a.Set(nil, w, SetOptions{Subdir: strPtr("components/x/")})
	if err != nil || sr.CodeDir != filepath.Join(root, "components", "x") {
		t.Fatalf("set subdir back: %+v %v", sr, err)
	}
	// a rename keeps the worktree usable from the subdir
	sr, err = a.Set(nil, w, SetOptions{Desc: "renamed"})
	if err != nil || !sr.Moved || len(sr.Warnings) != 0 {
		t.Fatalf("rename: %+v %v", sr, err)
	}
	if g := a.Git(w); g == nil || !g.Linked || g.Branch != "MONO-1" {
		t.Fatalf("git after rename: %+v", g)
	}

	// forced removal goes through git: the main checkout forgets the worktree
	if err := a.Remove(nil, w, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dir still there")
	}
	out, _ := exec.Command("git", "-C", main, "worktree", "list", "--porcelain").Output()
	if strings.Contains(string(out), "src/mono") {
		t.Fatalf("main checkout still lists the worktree:\n%s", out)
	}

	// an explicit --subdir overrides the repo default; one that is not in
	// the branch is refused and leaves nothing behind
	w2, _ := a.Create(CreateOptions{Desc: "root work", Jira: "MONO-2"})
	if _, err := a.AddWorktree(w2, WorktreeOptions{Repo: "mono", NoFetch: true, Base: "develop", Subdir: "components/missing"}); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing subdir on add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w2.Dir, "src", "mono")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed add left the worktree behind")
	}
	if w2.CodeDir != "" {
		t.Fatalf("failed add set code_dir %q", w2.CodeDir)
	}
	res2, err := a.AddWorktree(w2, WorktreeOptions{Repo: "mono", NoFetch: true, Base: "develop", Subdir: "."})
	if err != nil || res2.CodeDir != res2.Path || w2.CodeDir != filepath.Join("src", "mono") {
		t.Fatalf("subdir .: %+v %v code_dir=%s", res2, err, w2.CodeDir)
	}
}

func strPtr(s string) *string { return &s }
