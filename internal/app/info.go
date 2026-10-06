package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/gitwt"
	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/sway"
)

// WorkstreamInfo is a workstream plus its live status — the one
// representation `jug ls --json`, the REST API and the web UI share.
type WorkstreamInfo struct {
	Name            string      `json:"name"`
	ID              string      `json:"id"`
	Category        string      `json:"category"`
	Desc            string      `json:"desc"`
	Created         time.Time   `json:"created"`
	Dir             string      `json:"dir"`
	CodeDir         string      `json:"code_dir"`    // as configured ("" = none)
	CodePath        string      `json:"code_path"`   // resolved
	CodeInside      bool        `json:"code_inside"` // the code dir lives in the workstream dir
	HasCode         bool        `json:"has_code"`    // a code dir is configured
	Refs            []store.Ref `json:"refs"`
	OpencodeSession string      `json:"opencode_session,omitempty"`
	State           string      `json:"state"` // displayed | parked | none
	TodosOpen       int         `json:"todos_open"`
	Git             *GitInfo    `json:"git,omitempty"`
	Shown           string      `json:"shown,omitempty"` // RFC3339, last displayed
}

// InfoOf builds the info for w. tree and st may be nil (no sway): state is
// then "none".
func (a *App) InfoOf(w *store.Workstream, tree *sway.Node, st *store.State) WorkstreamInfo {
	in := WorkstreamInfo{
		Name: w.Name(), ID: w.ID, Category: w.Category, Desc: w.Desc, Created: w.Created, Dir: w.Dir,
		CodeDir: w.CodeDir, CodePath: w.ResolvedCodeDir(), CodeInside: w.CodeInside(), HasCode: w.CodeDir != "",
		Refs: w.Refs, OpencodeSession: w.OpencodeSession, State: "none", TodosOpen: w.OpenTodos(),
	}
	if in.Refs == nil {
		in.Refs = []store.Ref{}
	}
	if tree != nil && st != nil {
		probe := &layout.Engine{Cfg: a.Cfg, State: st}
		if live, parked := probe.IsLive(tree, w); live {
			in.State = "parked"
			if !parked {
				in.State = "displayed"
			}
		}
		if st.Displayed == w.Name() {
			in.State = "displayed"
		}
		if s := st.Streams[w.Name()]; s != nil {
			in.Shown = s.Shown
		}
	}
	if gitwt.IsRepo(in.CodePath) {
		in.Git = a.Git(w)
	}
	return in
}

// Infos returns every workstream with live status. sway is best effort:
// without it, states are "none".
func (a *App) Infos() ([]WorkstreamInfo, error) {
	all, err := a.Store.List()
	if err != nil {
		return nil, err
	}
	tree, st := a.snapshot()
	out := make([]WorkstreamInfo, 0, len(all))
	for _, w := range all {
		out = append(out, a.InfoOf(w, tree, st))
	}
	return out, nil
}

// Info returns one workstream with live status.
func (a *App) Info(w *store.Workstream) WorkstreamInfo {
	tree, st := a.snapshot()
	return a.InfoOf(w, tree, st)
}

// snapshot reads the tree and the state, tolerating a missing compositor.
func (a *App) snapshot() (*sway.Node, *store.State) {
	st, _ := store.LoadState(a.Cfg.StateDir)
	var tree *sway.Node
	if c, err := a.Dial(); err == nil {
		tree, _ = c.GetTree()
		c.Close()
	}
	return tree, st
}

// Status is the slot/lot picture for a status bar or a page header.
type Status struct {
	Sway             bool        `json:"sway"`
	SwayError        string      `json:"sway_error,omitempty"`
	Slot             *store.Slot `json:"slot"`
	Displayed        string      `json:"displayed"`
	Lot              bool        `json:"lot"` // the lot workspace exists (something is parked)
	FocusedWorkspace string      `json:"focused_workspace,omitempty"`
	OnSlot           bool        `json:"on_slot"`
	Workstreams      int         `json:"workstreams"`
	Config           string      `json:"config"`
	Root             string      `json:"root"`
	Version          string      `json:"version"`
}

// Version is set by the binary (ldflags).
var Version = "dev"

// Status returns the current picture (sway best effort).
func (a *App) Status() Status {
	s := Status{Config: config.Path(), Root: a.Cfg.Root, Version: Version}
	if all, err := a.Store.List(); err == nil {
		s.Workstreams = len(all)
	}
	st, _ := store.LoadState(a.Cfg.StateDir)
	c, err := a.Dial()
	if err != nil {
		s.SwayError = err.Error()
		if st != nil {
			s.Slot, s.Displayed = st.Slot, st.Displayed
		}
		return s
	}
	defer c.Close()
	s.Sway = true
	if st != nil {
		e := &layout.Engine{Sway: c, Cfg: a.Cfg, Store: a.Store, State: st, Debug: a.Debug}
		if st.Slot != nil {
			_ = e.Reconcile()
		}
		s.Slot, s.Displayed = st.Slot, st.Displayed
		if tree, err := c.GetTree(); err == nil {
			s.Lot = tree.WorkspaceNamed(a.Cfg.LotLabel) != nil
		}
	}
	if wss, err := c.GetWorkspaces(); err == nil {
		for _, w := range wss {
			if w.Focused {
				s.FocusedWorkspace = w.Name
				s.OnSlot = s.Slot != nil && w.Num == s.Slot.Num
			}
		}
	}
	return s
}

// Check is one line of `jug doctor`.
type Check struct {
	OK     bool   `json:"ok"`
	What   string `json:"what"`
	Detail string `json:"detail"`
}

// Doctor checks sway, the workspace naming scheme, the tools and the config.
func (a *App) Doctor() []Check {
	var out []Check
	check := func(ok bool, what, detail string) { out = append(out, Check{ok, what, detail}) }
	conn, err := a.Dial()
	check(err == nil, "sway ipc", fmt.Sprint(err))
	if err == nil {
		defer conn.Close()
		wss, _ := conn.GetWorkspaces()
		var unnumbered []string
		for _, w := range wss {
			if w.Num < 0 && w.Name != a.Cfg.LotLabel {
				unnumbered = append(unnumbered, w.Name)
			}
		}
		check(len(unnumbered) == 0, "workspaces named N:name", strings.Join(unnumbered, ", "))
		cfgText, _ := swayConfigText()
		layoutOK := strings.Contains(cfgText, "workspace_layout stacking") || strings.Contains(cfgText, "workspace_layout tabbed")
		check(layoutOK, "workspace_layout stacking|tabbed", "(the lot needs sway to wrap a sole container in a new workspace; see README, Limitations)")
		st, _ := store.LoadState(a.Cfg.StateDir)
		if st != nil && st.Slot != nil {
			tree, _ := conn.GetTree()
			ws := tree.WorkspaceNum(st.Slot.Num)
			detail := fmt.Sprintf("workspace %d (%s)", st.Slot.Num, st.Slot.Name)
			if ws != nil {
				detail += " shape " + ws.Shape()
			} else {
				detail += " (currently empty/reaped)"
			}
			check(true, "slot", detail)
			check(true, "displayed", st.Displayed)
		} else {
			check(true, "slot", "none toggled")
		}
	}
	for _, tool := range []string{firstWord(a.Cfg.Terminal), "fuzzel", firstWord(a.Cfg.Browser), a.Cfg.Editor, firstWord(a.Cfg.Opencode), "notify-send", "git"} {
		p, err := exec.LookPath(tool)
		check(err == nil, tool, p)
	}
	if a.Cfg.Dictator != "" {
		p, err := exec.LookPath(a.Cfg.Dictator)
		check(err == nil, a.Cfg.Dictator, p)
	}
	_, err = os.Stat(a.Cfg.Root)
	check(err == nil, "store root", a.Cfg.Root)
	check(a.Cfg.JiraBaseURL != "", "jira_base_url", a.Cfg.JiraBaseURL+" (needed for --jira / jira refs)")
	for _, r := range a.Repos() {
		detail := r.Path
		if r.OK {
			base := r.DefaultBranch
			if base == "" {
				base = gitwt.DefaultBranch(r.Path, r.Remote)
			}
			detail += "  base " + base
			if r.SeedDir != "" {
				detail += "  seed " + layout.ShortDir(r.SeedDir)
			}
		}
		check(r.OK, "repo "+r.Name, detail)
	}
	return out
}

func swayConfigText() (string, error) {
	out, err := exec.Command("swaymsg", "-t", "get_config").Output()
	if err != nil {
		return "", err
	}
	var r struct {
		Config string `json:"config"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", err
	}
	return r.Config, nil
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return s
	}
	return f[0]
}
