package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/sway"
)

// Hibernation gives the memory back that finished work still holds.
//
// A parked workstream keeps its windows alive as a tab of the lot: the
// terminals (a shell each, nothing), the browser windows (the browser
// unloads tabs it has not shown for a while — cheap too), and the opencode
// TUI, which is a full process holding its session in memory, forever,
// whether anyone looks at it or not. Finished workstreams pile up, and
// with them their TUIs. So once a workstream is finished (ticket closed,
// or completed by you) and has been parked without being shown for
// HibernateAfter, `jug serve` quits its opencode and closes its browser
// windows; a note in the TUI's place says so. The session is untouched —
// the next show starts opencode on it again — and nothing is deleted.
//
// What is never hibernated: the displayed workstream, one whose window has
// focus, one whose opencode is in the middle of a turn or waiting on a
// permission (that work would be lost), and anything not finished.

// hibernateEvery is how often `jug serve` looks for due workstreams; the
// first look is a minute after start.
const hibernateEvery = 10 * time.Minute

// HibernateOptions tune one pass.
type HibernateOptions struct {
	// Only names workstreams to hibernate now, regardless of how long they
	// have been finished or idle (an explicit request); empty = every
	// workstream that is due.
	Only []string
	// DryRun lists what would happen without doing it.
	DryRun bool
}

// Hibernated is one workstream a pass put to sleep (or would, DryRun).
type Hibernated struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Opencode bool   `json:"opencode"` // its opencode window was quit
	Browsers int    `json:"browsers"` // browser windows closed
	Why      string `json:"why"`      // "finished 7d ago, not shown for 9d" or "on request"
}

// HibernateReport says what a pass did.
type HibernateReport struct {
	DryRun     bool         `json:"dry_run"`
	Hibernated []Hibernated `json:"hibernated"`
	Skipped    []string     `json:"skipped"`          // "<id>: <why>" — named, or due but in use
	Errors     []string     `json:"errors,omitempty"` // "<id>: <error>"; the rest was still done
}

// Summary is the one-line form.
func (r HibernateReport) Summary() string {
	s := fmt.Sprintf("%d hibernated", len(r.Hibernated))
	if r.DryRun {
		s = fmt.Sprintf("%d would hibernate", len(r.Hibernated))
	}
	if len(r.Skipped) > 0 {
		s += fmt.Sprintf(", %d skipped", len(r.Skipped))
	}
	if len(r.Errors) > 0 {
		s += fmt.Sprintf(", %d errors", len(r.Errors))
	}
	return s
}

// hibernateCandidate is the decision for one workstream.
type hibernateCandidate struct {
	w    *store.Workstream
	why  string // reason to hibernate
	skip string // reason not to ("" = go)
}

// hibernateDue applies the rules to every workstream: finished, parked
// (has a root, not displayed), and idle — not shown since it finished, or
// for HibernateAfter, whichever is later — for at least HibernateAfter.
// Pure given its inputs; the live checks (focus, a busy opencode) happen in
// Hibernate.
func hibernateDue(all []*store.Workstream, st *store.State, tree *sway.Node, after time.Duration, now time.Time) []hibernateCandidate {
	var out []hibernateCandidate
	for _, w := range all {
		if !w.Finished() || w.Name() == st.Displayed {
			continue
		}
		if tree.ByMark(layout.RootMark(w.Name())) == nil {
			continue // no windows: nothing to give back
		}
		ss := st.Streams[w.Name()]
		if ss != nil && ss.Hibernated != "" {
			continue // already asleep
		}
		since := w.FinishedSince()
		shown := time.Time{}
		if ss != nil && ss.Shown != "" {
			shown, _ = time.Parse(time.RFC3339, ss.Shown)
		}
		idleFrom := since
		if shown.After(idleFrom) {
			idleFrom = shown
		}
		if idleFrom.IsZero() {
			continue // finished, but no clock to measure by: leave it
		}
		if now.Sub(idleFrom) < after {
			continue
		}
		why := "finished " + store.Age(now.Sub(since)) + " ago"
		if !shown.IsZero() {
			why += ", not shown for " + store.Age(now.Sub(shown))
		} else {
			why += ", never shown since"
		}
		out = append(out, hibernateCandidate{w: w, why: why})
	}
	return out
}

// Hibernate runs one pass: the due workstreams (or the named ones, now).
// Each is probed first — a busy opencode, or one waiting on a permission,
// is left alone and reported. One failure does not stop the rest.
func (a *App) Hibernate(ctx context.Context, e *layout.Engine, opt HibernateOptions) (HibernateReport, error) {
	rep := HibernateReport{DryRun: opt.DryRun, Hibernated: []Hibernated{}, Skipped: []string{}}
	tree, err := e.Sway.GetTree()
	if err != nil {
		return rep, err
	}
	var cands []hibernateCandidate
	if len(opt.Only) > 0 {
		for _, q := range opt.Only {
			w, err := a.Resolve(q)
			if err != nil {
				return rep, err
			}
			c := hibernateCandidate{w: w, why: "on request"}
			switch {
			case w.Name() == e.State.Displayed:
				c.skip = "displayed; park it first"
			case tree.ByMark(layout.RootMark(w.Name())) == nil:
				c.skip = "no windows"
			}
			cands = append(cands, c)
		}
	} else {
		after, err := a.Cfg.HibernateAfterDuration()
		if err != nil {
			return rep, err
		}
		if after == 0 {
			return rep, nil
		}
		all, err := a.Store.List()
		if err != nil {
			return rep, err
		}
		cands = hibernateDue(all, e.State, tree, after, time.Now())
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].w.Name() < cands[j].w.Name() })
	for _, c := range cands {
		if c.skip == "" {
			if s := a.SessionState(ctx, c.w); s.Status == "busy" || s.Status == "retry" || len(s.Pending) > 0 {
				c.skip = "its opencode is " + s.Status
				if len(s.Pending) > 0 {
					c.skip = "its opencode is waiting on a permission"
				}
			}
		}
		if c.skip != "" {
			rep.Skipped = append(rep.Skipped, c.w.ID+": "+c.skip)
			continue
		}
		h := Hibernated{ID: c.w.ID, Name: c.w.Name(), Why: c.why}
		if opt.DryRun {
			// what it would do: counted from the tree
			if root := tree.ByMark(layout.RootMark(c.w.Name())); root != nil {
				h.Opencode = tree.ByAppID(layout.OpencodeAppID(c.w.Name())) != nil
				for _, v := range root.Views() {
					if v.AppID == a.Cfg.BrowserAppID {
						h.Browsers++
					}
				}
			}
			rep.Hibernated = append(rep.Hibernated, h)
			continue
		}
		res, err := e.Hibernate(c.w, a.hibernateNote(c.w, c.why))
		if err != nil {
			rep.Errors = append(rep.Errors, c.w.ID+": "+err.Error())
			continue
		}
		h.Opencode, h.Browsers = res.Opencode, res.Browsers
		rep.Hibernated = append(rep.Hibernated, h)
	}
	return rep, nil
}

// hibernateNote is what the terminal in opencode's place says.
func (a *App) hibernateNote(w *store.Workstream, why string) []string {
	after, _ := a.Cfg.HibernateAfterDuration()
	lines := []string{
		"",
		"  juggler put this workstream to sleep to save memory.",
		"",
		"  " + w.ID + " — " + w.Desc,
		"  " + why + ".",
	}
	if after > 0 && why != "on request" {
		lines = append(lines, "  (finished + parked + not shown for "+shortDuration(after)+" = hibernate_after in config.toml)")
	}
	lines = append(lines,
		"",
		"  opencode was quit; its session is kept and resumes on the next show:",
		"      jug show "+w.ID+"        (or: jug relaunch "+w.ID+")",
	)
	if w.OpencodeSession != "" {
		lines = append(lines, "      session "+w.OpencodeSession)
	}
	lines = append(lines,
		"  Its browser windows were closed; `jug open jira|pr` opens them again.",
		"",
		"  Press Enter to close this window.",
	)
	return lines
}

// HibernateLoop is `jug serve`'s housekeeping: a pass a minute after start,
// then every hibernateEvery, while ctx lasts. Without sway a pass is
// skipped, not failed. Nothing is logged when nothing happened.
func (a *App) HibernateLoop(ctx context.Context, logger *log.Logger) {
	after, err := a.Cfg.HibernateAfterDuration()
	if err != nil {
		if logger != nil {
			logger.Printf("hibernate: %v — disabled", err)
		}
		return
	}
	if after == 0 {
		return
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		var rep HibernateReport
		err := a.WithEngine(func(e *layout.Engine) (err error) {
			rep, err = a.Hibernate(ctx, e, HibernateOptions{})
			return
		})
		if logger != nil {
			var se *SwayError
			switch {
			case errors.As(err, &se):
				// no compositor: nothing to do
			case err != nil:
				logger.Printf("hibernate: %v", err)
			case len(rep.Hibernated)+len(rep.Errors) > 0:
				var parts []string
				for _, h := range rep.Hibernated {
					what := []string{}
					if h.Opencode {
						what = append(what, "quit opencode")
					}
					if h.Browsers > 0 {
						what = append(what, fmt.Sprintf("closed %d browser window(s)", h.Browsers))
					}
					parts = append(parts, fmt.Sprintf("%s: %s (%s)", h.ID, strings.Join(what, ", "), h.Why))
				}
				parts = append(parts, rep.Errors...)
				logger.Printf("hibernate: %s — %s", rep.Summary(), strings.Join(parts, "; "))
			}
		}
		if a.OnHibernate != nil && len(rep.Hibernated) > 0 {
			a.OnHibernate(rep)
		}
		timer.Reset(hibernateEvery)
	}
}
