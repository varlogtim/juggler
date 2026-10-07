package app

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/oc"
	"github.com/varlogtim/juggler/internal/store"
)

// ErrNoSession is returned when a workstream has no pinned opencode session.
var ErrNoSession = errors.New("no opencode session pinned (one is created on the next show)")

// ocServer starts a transient opencode server rooted at w's code dir.
func (a *App) ocServer(w *store.Workstream) (*oc.Server, error) {
	bin, err := oc.Binary(a.Cfg.Opencode)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	_ = cancel // the server is stopped explicitly by the caller
	return oc.Start(ctx, bin, w.ResolvedCodeDir())
}

// SessionGet returns w's pinned session as opencode knows it.
func (a *App) SessionGet(w *store.Workstream) (*oc.Session, error) {
	if w.OpencodeSession == "" {
		return nil, ErrNoSession
	}
	srv, err := a.ocServer(w)
	if err != nil {
		return nil, err
	}
	defer srv.Stop()
	s, err := srv.Get(w.ResolvedCodeDir(), w.OpencodeSession)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SessionCandidates lists sessions w could adopt: those of the code dir's
// project and of the home directory's (sessions started from ~ live in
// opencode's "global" project), newest first. Without all, only titles that
// mention the workstream id, a ref key, or (manual ids) the description.
func (a *App) SessionCandidates(w *store.Workstream, all bool) ([]oc.Session, error) {
	srv, err := a.ocServer(w)
	if err != nil {
		return nil, err
	}
	defer srv.Stop()
	home, _ := os.UserHomeDir()
	dirs := []string{w.ResolvedCodeDir()}
	if home != "" && home != w.ResolvedCodeDir() {
		dirs = append(dirs, home)
	}
	seen := map[string]bool{}
	var cands []oc.Session
	for _, d := range dirs {
		list, err := srv.List(d, "", 200)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			if seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			if all || a.SessionMatches(w, s) {
				cands = append(cands, s)
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Time.Updated > cands[j].Time.Updated })
	return cands, nil
}

// SessionMatches reports whether a session's title mentions the workstream.
func (a *App) SessionMatches(w *store.Workstream, s oc.Session) bool {
	t := strings.ToLower(s.Title)
	if strings.Contains(t, strings.ToLower(w.ID)) {
		return true
	}
	for _, r := range w.Refs {
		if r.Key != "" && strings.Contains(t, strings.ToLower(r.Key)) {
			return true
		}
	}
	if strings.HasPrefix(w.ID, a.Cfg.IDPrefix+"-") && w.Desc != "" && strings.Contains(t, strings.ToLower(w.Desc)) {
		return true
	}
	return false
}

// SessionPin pins id (verified to exist) and optionally relaunches opencode.
func (a *App) SessionPin(e *layout.Engine, w *store.Workstream, id string, relaunch bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("session id is required")
	}
	w.OpencodeSession = id
	if err := a.Store.Save(w); err != nil {
		return err
	}
	if relaunch && e != nil {
		return e.RelaunchOpencode(w)
	}
	return nil
}

// SessionNew creates a fresh titled session, pins it and optionally
// relaunches opencode on it.
func (a *App) SessionNew(e *layout.Engine, w *store.Workstream, relaunch bool) (string, error) {
	eng := e
	if eng == nil {
		eng = &layout.Engine{Cfg: a.Cfg, Store: a.Store}
	}
	id, err := eng.NewSession(w)
	if err != nil {
		return "", err
	}
	if relaunch && e != nil {
		return id, e.RelaunchOpencode(w)
	}
	return id, nil
}

// SessionUnpin forgets the pin.
func (a *App) SessionUnpin(w *store.Workstream) error {
	w.OpencodeSession = ""
	return a.Store.Save(w)
}

// SessionRelaunch restarts w's opencode window on the pinned session, in
// place (displayed or parked).
func (a *App) SessionRelaunch(e *layout.Engine, w *store.Workstream) error {
	return e.RelaunchOpencode(w)
}

// RelaunchReport says what RelaunchAll did.
type RelaunchReport struct {
	Relaunched []string `json:"relaunched"`       // workstreams whose opencode window was restarted
	Skipped    []string `json:"skipped"`          // "<id>: <why>" — no opencode window to restart
	Errors     []string `json:"errors,omitempty"` // "<id>: <error>"; the others were still done
}

// RelaunchAll restarts the opencode window of every workstream that has
// one — or of the named ones — each on its pinned session, the displayed
// workstream first, parked ones in place in the lot. This is how running
// sessions pick up new skills, instructions or an opencode upgrade: a
// session only reads them at start. A workstream without an opencode window
// is skipped, not started. One failure does not stop the rest.
func (a *App) RelaunchAll(e *layout.Engine, only []string) (RelaunchReport, error) {
	var rep RelaunchReport
	var todo []*store.Workstream
	if len(only) > 0 {
		for _, q := range only {
			w, err := a.Resolve(q)
			if err != nil {
				return rep, err
			}
			todo = append(todo, w)
		}
	} else {
		all, err := a.Store.List()
		if err != nil {
			return rep, err
		}
		todo = all
	}
	// the one on screen first: it is the one being waited for
	sort.SliceStable(todo, func(i, j int) bool {
		return todo[i].Name() == e.State.Displayed && todo[j].Name() != e.State.Displayed
	})
	rep.Relaunched, rep.Skipped = []string{}, []string{}
	for _, w := range todo {
		tree, err := e.Sway.GetTree()
		if err != nil {
			return rep, err
		}
		if !e.HasOpencode(tree, w) {
			switch live, _ := e.IsLive(tree, w); {
			case live:
				rep.Skipped = append(rep.Skipped, w.ID+": no opencode window (closed; `jug show` starts one)")
			case len(only) > 0:
				rep.Skipped = append(rep.Skipped, w.ID+": no windows")
			}
			continue // a workstream without windows is not a candidate unless named
		}
		if err := e.RelaunchOpencode(w); err != nil {
			rep.Errors = append(rep.Errors, w.ID+": "+err.Error())
			continue
		}
		rep.Relaunched = append(rep.Relaunched, w.ID)
	}
	return rep, nil
}
