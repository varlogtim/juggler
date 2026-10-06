package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/gitwt"
	"github.com/varlogtim/juggler/internal/seed"
	"github.com/varlogtim/juggler/internal/store"
)

// WorktreeOptions are the inputs of AddWorktree.
type WorktreeOptions struct {
	Repo    string // a configured repo name, or a path to a main checkout
	Branch  string // "" = DefaultBranch(w)
	Base    string // "" = the repo's default_branch, else the remote's HEAD
	NoFetch bool
}

// WorktreeResult says what AddWorktree did.
type WorktreeResult struct {
	Repo   string   `json:"repo"`
	Branch string   `json:"branch"`
	Path   string   `json:"path"`
	What   string   `json:"what"`   // one line: "worktree on new branch X from origin/develop"
	Seeded []string `json:"seeded"` // files copied from the seed dir
}

var ticketID = regexp.MustCompile(`^[A-Z][A-Z0-9]+-[0-9]+$`)

// DefaultBranch names a workstream's branch: the ticket key as-is, else
// <user>/<slug>.
func DefaultBranch(w *store.Workstream) string {
	if ticketID.MatchString(w.ID) {
		return w.ID
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "me"
	}
	return user + "/" + store.Slug(w.Desc)
}

// RepoInfo describes a configured repo.
type RepoInfo struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Remote        string `json:"remote"`
	DefaultBranch string `json:"default_branch"` // "" = the remote's HEAD
	SeedDir       string `json:"seed_dir,omitempty"`
	OK            bool   `json:"ok"` // Path is a git checkout
}

// Repos lists the configured repos, sorted by name.
func (a *App) Repos() []RepoInfo {
	var out []RepoInfo
	for name, r := range a.Cfg.Repos {
		out = append(out, RepoInfo{Name: name, Path: r.Path, Remote: r.Remote, DefaultBranch: r.DefaultBranch, SeedDir: a.Cfg.SeedDirFor(name), OK: gitwt.IsRepo(r.Path)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResolveRepo turns a repo name or path into (name, main checkout, remote,
// default base).
func (a *App) ResolveRepo(q string) (name, mainRepo, remote, base string, err error) {
	if r, ok := a.Cfg.Repos[q]; ok {
		return q, r.Path, r.Remote, r.DefaultBranch, nil
	}
	p := config.Expand(q)
	if abs, e := filepath.Abs(p); e == nil {
		p = abs
	}
	if !gitwt.IsRepo(p) {
		if len(a.Cfg.Repos) > 0 {
			var names []string
			for k := range a.Cfg.Repos {
				names = append(names, k)
			}
			sort.Strings(names)
			return "", "", "", "", fmt.Errorf("%q is neither a configured repo (%s) nor a git checkout", q, strings.Join(names, ", "))
		}
		return "", "", "", "", fmt.Errorf("%q is not a git checkout (configure [repos] in %s to use short names)", q, config.Path())
	}
	top, e := gitwt.Toplevel(p)
	if e != nil {
		return "", "", "", "", e
	}
	mainRepo, e = gitwt.MainRepo(top)
	if e != nil {
		return "", "", "", "", e
	}
	return filepath.Base(mainRepo), mainRepo, "origin", "", nil
}

// AddWorktree creates <ws>/<code_subdir>/<repo> as a linked worktree, points
// code_dir at it and copies the repo's seed files in.
func (a *App) AddWorktree(w *store.Workstream, o WorktreeOptions) (WorktreeResult, error) {
	var res WorktreeResult
	if w.CodeInside() {
		return res, fmt.Errorf("%s already has its own code dir (%s)", w.ID, w.CodeDir)
	}
	name, mainRepo, remote, base, err := a.ResolveRepo(o.Repo)
	if err != nil {
		return res, err
	}
	if o.Base != "" {
		base = o.Base
	}
	branch := o.Branch
	if branch == "" {
		branch = DefaultBranch(w)
	}
	rel := filepath.Join(a.Cfg.CodeSubdir, name)
	path := filepath.Join(w.Dir, rel)
	what, err := gitwt.Add(mainRepo, path, branch, gitwt.AddOptions{Remote: remote, Base: base, Fetch: !o.NoFetch})
	if err != nil {
		return res, err
	}
	w.CodeDir = rel
	if err := a.Store.Save(w); err != nil {
		return res, err
	}
	res = WorktreeResult{Repo: name, Branch: branch, Path: path, What: what}
	res.Seeded, err = a.seedInto(w, name, false)
	return res, err
}

// Seed (re)copies the repo's seed files into w's worktree.
func (a *App) Seed(w *store.Workstream, force bool) ([]string, error) {
	name := a.RepoNameOf(w)
	if name == "" || a.Cfg.SeedDirFor(name) == "" {
		return nil, fmt.Errorf("no seed dir for %s (expected %s)", w.ID, filepath.Join(filepath.Dir(config.Path()), "seed", name))
	}
	return a.seedInto(w, name, force)
}

// seedInto copies the seed files (templated) and trusts a seeded .envrc.
func (a *App) seedInto(w *store.Workstream, repoName string, force bool) ([]string, error) {
	sd := a.Cfg.SeedDirFor(repoName)
	if sd == "" {
		return nil, nil
	}
	home, _ := os.UserHomeDir()
	written, err := seed.Apply(sd, w.ResolvedCodeDir(), seed.Vars{
		ID: w.ID, Name: w.Name(), Repo: repoName, Dir: w.Dir, CodeDir: w.ResolvedCodeDir(), Home: home,
	}, force)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	for _, rel := range written {
		if rel == ".envrc" {
			if err := seed.DirenvAllow(w.ResolvedCodeDir()); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// SeedPaths lists the relative paths the repo's seed dir would put into w's
// worktree — untracked files that do not count as "dirty".
func (a *App) SeedPaths(w *store.Workstream) []string {
	name := a.RepoNameOf(w)
	sd := a.Cfg.SeedDirFor(name)
	if sd == "" {
		return nil
	}
	var paths []string
	_ = filepath.WalkDir(sd, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(sd, p)
			paths = append(paths, rel)
		}
		return nil
	})
	return paths
}

// RepoNameOf returns the configured repo name whose main checkout owns w's
// code dir, else the main checkout's base name, else "".
func (a *App) RepoNameOf(w *store.Workstream) string {
	mainRepo, err := gitwt.MainRepo(w.ResolvedCodeDir())
	if err != nil {
		return ""
	}
	for name, r := range a.Cfg.Repos {
		if r.Path == mainRepo {
			return name
		}
	}
	return filepath.Base(mainRepo)
}

// GitInfo is the code dir's git state (nil when it is not a checkout).
type GitInfo struct {
	Branch   string `json:"branch"`
	Head     string `json:"head"`
	Dirty    bool   `json:"dirty"`
	Linked   bool   `json:"linked"` // a linked worktree
	Main     string `json:"main"`   // the main checkout
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
}

// Git inspects w's code dir.
func (a *App) Git(w *store.Workstream) *GitInfo {
	in, err := gitwt.Inspect(w.ResolvedCodeDir())
	if err != nil {
		return nil
	}
	return &GitInfo{Branch: in.Branch, Head: in.Head, Dirty: in.Dirty, Linked: in.Linked, Main: in.Main, Upstream: in.Upstream, Ahead: in.Ahead, Behind: in.Behind}
}

// RemovePlan is what Remove would do.
type RemovePlan struct {
	Name        string `json:"name"`
	Dir         string `json:"dir"`
	HasWorktree bool   `json:"has_worktree"`
	Worktree    string `json:"worktree,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Dirty       string `json:"dirty,omitempty"` // first path that blocks removal without force
}

// Plan describes the effect of removing w.
func (a *App) Plan(w *store.Workstream) RemovePlan {
	p := RemovePlan{Name: w.Name(), Dir: w.Dir}
	code := w.ResolvedCodeDir()
	if w.CodeInside() && gitwt.IsLinkedWorktree(code) {
		p.HasWorktree, p.Worktree = true, code
		if in, err := gitwt.Inspect(code); err == nil {
			p.Branch = in.Branch
		}
		if changes, _ := gitwt.Changes(code); len(changes) > 0 {
			exp := map[string]bool{}
			for _, s := range a.SeedPaths(w) {
				exp[s] = true
			}
			for _, c := range changes {
				if !exp[c] {
					p.Dirty = c
					break
				}
			}
		}
	}
	return p
}

// ErrDirty is gitwt.ErrDirty, re-exported for callers.
var ErrDirty = gitwt.ErrDirty

// removeFiles removes w's worktree (through git) and its directory. Windows
// must already be closed.
func (a *App) removeFiles(w *store.Workstream, force bool) error {
	code := w.ResolvedCodeDir()
	if w.CodeInside() && gitwt.IsLinkedWorktree(code) {
		if err := gitwt.Remove(code, force, a.SeedPaths(w)); err != nil {
			if errors.Is(err, gitwt.ErrDirty) {
				return fmt.Errorf("%s: %w (commit or stash, or force)", code, err)
			}
			return err
		}
	}
	return os.RemoveAll(w.Dir)
}
