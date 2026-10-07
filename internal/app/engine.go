package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/varlogtim/juggler/internal/gitwt"
	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/picker"
	"github.com/varlogtim/juggler/internal/seed"
	"github.com/varlogtim/juggler/internal/store"
)

// Operations that need sway take the engine explicitly.

// Show displays w in the slot (parking the displayed workstream).
func (a *App) Show(e *layout.Engine, w *store.Workstream, noSwitch bool) error {
	return e.Show(w, layout.ShowOptions{NoSwitch: noSwitch})
}

// Toggle makes the focused workspace the slot, or releases it.
func (a *App) Toggle(e *layout.Engine) (on bool, slot *store.Slot, err error) {
	on, err = e.Toggle()
	return on, e.State.Slot, err
}

// Park hides the displayed workstream.
func (a *App) Park(e *layout.Engine) error { return e.Park() }

// ParkOthers moves parked workstreams found outside the lot into it.
func (a *App) ParkOthers(e *layout.Engine) error { return e.ParkStrays() }

// Lot visits the lot or comes back.
func (a *App) Lot(e *layout.Engine) error { return e.Lot() }

// ErrNothingParked is layout.ErrNothingParked.
var ErrNothingParked = layout.ErrNothingParked

// Close kills w's windows.
func (a *App) Close(e *layout.Engine, w *store.Workstream) error { return e.Close(w) }

// Current returns the displayed workstream, or nil.
func (a *App) Current(e *layout.Engine) (*store.Workstream, error) {
	if e.State.Displayed == "" {
		return nil, nil
	}
	return a.Load(e.State.Displayed)
}

// UnderFocus returns the workstream whose window has keyboard focus (e.g. a
// parked one in the lot), or nil.
func (a *App) UnderFocus(e *layout.Engine) (*store.Workstream, error) {
	tree, err := e.Sway.GetTree()
	if err != nil {
		return nil, err
	}
	n := layout.RootUnderFocus(tree)
	if n == "" {
		return nil, nil
	}
	return a.Load(n)
}

// Entries builds the picker rows' data.
func (a *App) Entries(e *layout.Engine) ([]picker.Entry, error) {
	all, err := a.Store.List()
	if err != nil {
		return nil, err
	}
	tree, err := e.Sway.GetTree()
	if err != nil {
		return nil, err
	}
	var es []picker.Entry
	for _, w := range all {
		live, _ := e.IsLive(tree, w)
		en := picker.Entry{W: w, Live: live, Displayed: e.State.Displayed == w.Name()}
		if s := e.State.Streams[w.Name()]; s != nil {
			en.Shown, _ = time.Parse(time.RFC3339, s.Shown)
		}
		es = append(es, en)
	}
	return es, nil
}

// Open opens a ref ("jira", "pr", …) or a URL in a browser window in w's
// right stack. queued reports that w is not displayed and the open will run
// on its next show.
func (a *App) Open(e *layout.Engine, w *store.Workstream, what string) (queued bool, err error) {
	key, url := what, ""
	if strings.Contains(what, "://") {
		key, url = "url:"+what, what
	} else if r := w.Ref(what); r != nil {
		url = r.URL
	} else {
		return false, fmt.Errorf("%w: %s has no %q ref", ErrNotFound, w.ID, what)
	}
	return e.Open(w, key, url)
}

// Term starts a terminal (optionally running command) in w's right stack.
func (a *App) Term(e *layout.Engine, w *store.Workstream, title, command string) error {
	return e.Spawn(w, title, command)
}

// Notes opens TODO.md in the editor, in w's right stack.
func (a *App) Notes(e *layout.Engine, w *store.Workstream) error {
	return e.Spawn(w, w.ID+" notes", a.Cfg.Editor+" "+layout.ShellQuote(layout.TodoFile(w)))
}

// Review opens the code dir's uncommitted diff read-only in the editor.
func (a *App) Review(e *layout.Engine, w *store.Workstream) error {
	command := fmt.Sprintf(`cd %s && { git diff HEAD --stat; echo; git diff HEAD; } | %s -R -c "set ft=diff nomodified" -`,
		layout.ShellQuote(w.ResolvedCodeDir()), a.Cfg.Editor)
	return e.Spawn(w, w.ID+" review", command)
}

// Focus focuses w's opencode window (rebuilding it if gone).
func (a *App) Focus(e *layout.Engine, w *store.Workstream) error { return e.FocusOpencode(w) }

// ---------------------------------------------------------------- set / remove

// SetOptions are the inputs of Set; empty fields are left alone.
type SetOptions struct {
	Category string
	ID       string
	Desc     string
	Jira     string // attach this ticket; also sets ID (and Category "work") unless given
}

// SetResult says what Set did.
type SetResult struct {
	OldName  string   `json:"old_name"`
	Name     string   `json:"name"`
	Moved    bool     `json:"moved"`
	Warnings []string `json:"warnings,omitempty"`
}

// Set renames / recategorizes w. The directory name is derived from
// category, id and description, so changing any of them moves the directory;
// live windows are closed first (opencode resumes its pinned session) and
// the workstream is shown again if it was displayed. e may be nil when sway
// is unavailable (windows are then left alone).
func (a *App) Set(e *layout.Engine, w *store.Workstream, o SetOptions) (SetResult, error) {
	res := SetResult{OldName: w.Name()}
	oldDir, oldTitle := w.Dir, w.SessionTitle()
	if o.Jira != "" {
		key := strings.ToUpper(o.Jira)
		if o.ID == "" && !strings.EqualFold(w.ID, key) {
			o.ID = key
		}
		if o.Category == "" && w.Category == a.Cfg.DefaultCategory {
			o.Category = "work"
		}
		u, err := a.JiraURL(key)
		if err != nil {
			return res, err
		}
		if r := w.Ref("jira"); r != nil {
			r.Key, r.URL = key, u
		} else {
			w.Refs = append(w.Refs, store.Ref{Type: "jira", Key: key, URL: u})
		}
	}
	if o.Category != "" {
		w.Category = strings.TrimSpace(o.Category)
	}
	if o.ID != "" {
		w.ID = strings.TrimSpace(o.ID)
	}
	if o.Desc != "" {
		w.Desc = strings.TrimSpace(o.Desc)
	}
	newDir := filepath.Join(a.Cfg.Root, store.DirName(w.Category, w.ID, w.Desc))
	res.Name = filepath.Base(newDir)
	if newDir == oldDir {
		return res, a.Store.Save(w) // only refs changed
	}
	if _, err := os.Stat(newDir); err == nil {
		return res, fmt.Errorf("%w: %s already exists", ErrInvalid, newDir)
	}

	wasDisplayed := false
	if e != nil {
		tree, err := e.Sway.GetTree()
		if err != nil {
			return res, err
		}
		wasDisplayed = e.State.Displayed == res.OldName
		if tree.ByMark(layout.RootMark(res.OldName)) != nil {
			if err := e.Close(&store.Workstream{Dir: oldDir}); err != nil {
				return res, err
			}
			// let the windows go before anything is launched in the new place
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if t, err := e.Sway.GetTree(); err == nil && t.ByMark(layout.RootMark(res.OldName)) == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return res, err
	}
	w.Dir = newDir
	res.Moved = true
	if err := a.Store.Save(w); err != nil {
		return res, err
	}
	code := w.ResolvedCodeDir()
	if w.CodeInside() && gitwt.IsLinkedWorktree(code) {
		if err := gitwt.Repair(code); err != nil {
			res.Warnings = append(res.Warnings, "git worktree repair: "+err.Error())
		}
		if err := seed.DirenvAllow(code); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
	}
	if e != nil && w.SessionTitle() != oldTitle {
		if err := e.RetitleSession(w); err != nil {
			res.Warnings = append(res.Warnings, "opencode session title not updated: "+err.Error())
		}
	}
	if wasDisplayed {
		if err := e.Show(w, layout.ShowOptions{NoSwitch: true}); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Remove closes w's windows (when e is given), removes its worktree
// (refusing while dirty unless force; the branch is kept) and deletes the
// workstream directory.
func (a *App) Remove(e *layout.Engine, w *store.Workstream, force bool) error {
	if e != nil {
		if err := e.Close(w); err != nil {
			return err
		}
	}
	if err := a.removeFiles(w, force); err != nil {
		return err
	}
	a.tombstoneIfSourced(w)
	return nil
}
