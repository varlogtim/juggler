// Package gitwt wraps the handful of git worktree operations juggler needs:
// create a worktree for a workstream, inspect it, remove it. It shells out
// to git; everything is relative to a "main" checkout that owns .git.
package gitwt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// IsRepo reports whether dir is inside a git work tree.
func IsRepo(dir string) bool {
	out, err := git(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// Toplevel returns the work tree root of dir.
func Toplevel(dir string) (string, error) { return git(dir, "rev-parse", "--show-toplevel") }

// MainRepo returns the main checkout that owns dir's .git (dir itself when
// it is not a linked worktree).
func MainRepo(dir string) (string, error) {
	common, err := git(dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		top, err := Toplevel(dir)
		if err != nil {
			return "", err
		}
		common = filepath.Join(top, common)
	}
	return filepath.Dir(filepath.Clean(common)), nil
}

// IsLinkedWorktree reports whether dir is a worktree whose .git is a file
// pointing into another checkout.
func IsLinkedWorktree(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && !fi.IsDir()
}

// Info is a snapshot of a checkout.
type Info struct {
	Branch   string // "" when detached
	Head     string // short hash
	Dirty    bool   // tracked changes (staged or not)
	Linked   bool   // a linked worktree (.git is a file)
	Main     string // the main checkout
	Ahead    int    // commits ahead of upstream (-1 = no upstream)
	Behind   int
	Upstream string
}

// Inspect reads Info for dir.
func Inspect(dir string) (Info, error) {
	var in Info
	if !IsRepo(dir) {
		return in, fmt.Errorf("%s is not a git checkout", dir)
	}
	in.Branch, _ = git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	in.Head, _ = git(dir, "rev-parse", "--short", "HEAD")
	st, _ := git(dir, "status", "--porcelain", "--untracked-files=no")
	in.Dirty = st != ""
	in.Linked = IsLinkedWorktree(dir)
	in.Main, _ = MainRepo(dir)
	in.Ahead, in.Behind = -1, -1
	if up, err := git(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil && up != "" {
		in.Upstream = up
		if lr, err := git(dir, "rev-list", "--left-right", "--count", "HEAD..."+up); err == nil {
			fmt.Sscanf(lr, "%d\t%d", &in.Ahead, &in.Behind)
		}
	}
	return in, nil
}

// DefaultBranch returns the remote's HEAD branch (e.g. "develop"), falling
// back to common names that exist.
func DefaultBranch(main, remote string) string {
	if out, err := git(main, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil && out != "" {
		return strings.TrimPrefix(out, remote+"/")
	}
	for _, b := range []string{"develop", "main", "master"} {
		if _, err := git(main, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+b); err == nil {
			return b
		}
	}
	return "main"
}

// Fetch updates remote/branch in main.
func Fetch(main, remote, branch string) error {
	_, err := git(main, "fetch", "--quiet", remote, branch)
	return err
}

// LocalBranchExists reports whether branch exists in main.
func LocalBranchExists(main, branch string) bool {
	_, err := git(main, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// RemoteBranchExists reports whether remote/branch is known locally.
func RemoteBranchExists(main, remote, branch string) bool {
	_, err := git(main, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+branch)
	return err == nil
}

// CheckedOutAt returns the worktree path where branch is checked out, or "".
func CheckedOutAt(main, branch string) string {
	out, err := git(main, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	var path string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			return path
		}
	}
	return ""
}

// AddOptions tunes Add.
type AddOptions struct {
	Remote string // default "origin"
	Base   string // branch to start from when Branch does not exist (default: remote HEAD)
	Fetch  bool   // fetch Base/Branch from Remote first
}

// Add creates a worktree of main at path on branch: reusing the branch when
// it exists locally, tracking it when it exists on the remote, else creating
// it from Base. It returns a one-line description of what it did.
func Add(main, path, branch string, opt AddOptions) (string, error) {
	if opt.Remote == "" {
		opt.Remote = "origin"
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists", path)
	}
	if at := CheckedOutAt(main, branch); at != "" {
		return "", fmt.Errorf("branch %s is already checked out at %s (git allows one worktree per branch)", branch, at)
	}
	if opt.Base == "" {
		opt.Base = DefaultBranch(main, opt.Remote)
	}
	fetchNote := ""
	if opt.Fetch {
		if err := Fetch(main, opt.Remote, opt.Base); err != nil {
			fetchNote = " (fetch of " + opt.Remote + "/" + opt.Base + " failed; used the local ref)"
		}
		_ = Fetch(main, opt.Remote, branch) // may not exist; fine
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	switch {
	case LocalBranchExists(main, branch):
		if _, err := git(main, "worktree", "add", path, branch); err != nil {
			return "", err
		}
		return fmt.Sprintf("worktree on existing branch %s", branch), nil
	case RemoteBranchExists(main, opt.Remote, branch):
		if _, err := git(main, "worktree", "add", "--track", "-b", branch, path, opt.Remote+"/"+branch); err != nil {
			return "", err
		}
		return fmt.Sprintf("worktree on %s (tracking %s/%s)", branch, opt.Remote, branch), nil
	default:
		base := opt.Remote + "/" + opt.Base
		if !RemoteBranchExists(main, opt.Remote, opt.Base) {
			if !LocalBranchExists(main, opt.Base) {
				return "", fmt.Errorf("base branch %s not found (neither %s nor a local branch)", opt.Base, base)
			}
			base = opt.Base
		}
		if _, err := git(main, "worktree", "add", "-b", branch, path, base); err != nil {
			return "", err
		}
		return fmt.Sprintf("worktree on new branch %s from %s%s", branch, base, fetchNote), nil
	}
}

// ErrDirty is returned by Remove when the worktree has changes.
var ErrDirty = errors.New("worktree has uncommitted or untracked changes")

// Changes lists the worktree's changed paths (tracked modifications and
// untracked files, relative to the worktree root).
func Changes(path string) ([]string, error) {
	st, err := git(path, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(st, "\n") {
		if len(line) > 3 {
			out = append(out, strings.TrimSpace(line[3:]))
		}
	}
	return out, nil
}

// Remove removes the linked worktree at path. Without force it refuses
// when the worktree has changes other than the paths in expected (files
// juggler itself put there, e.g. a seeded .envrc). The branch is kept.
func Remove(path string, force bool, expected []string) error {
	if !IsLinkedWorktree(path) {
		return fmt.Errorf("%s is not a linked worktree; refusing to remove a main checkout", path)
	}
	main, err := MainRepo(path)
	if err != nil {
		return err
	}
	if !force {
		changes, err := Changes(path)
		if err != nil {
			return err
		}
		ok := map[string]bool{}
		for _, e := range expected {
			ok[filepath.Clean(e)] = true
		}
		for _, c := range changes {
			if !ok[filepath.Clean(c)] {
				return fmt.Errorf("%w: %s", ErrDirty, c)
			}
		}
	}
	// Our check passed (or force): git's own refusal on untracked files
	// (the seeded ones) is bypassed with --force.
	_, err = git(main, "worktree", "remove", "--force", path)
	return err
}

// Repair fixes the main checkout's record of a linked worktree after the
// worktree directory was moved or renamed (git worktree repair, git >= 2.30).
func Repair(path string) error {
	main, err := MainRepo(path)
	if err != nil {
		return err
	}
	_, err = git(main, "worktree", "repair", path)
	return err
}

// Prune drops stale worktree records (after a directory vanished).
func Prune(main string) { _, _ = git(main, "worktree", "prune") }
