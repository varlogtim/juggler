// Package layout is juggler's sway layout engine. It turns a workstream into
// windows and back:
//
//	show   — put a workstream's windows in the slot workspace (building them
//	         the first time, restoring them from the scratchpad after)
//	park   — hide the displayed workstream (its windows stay alive, parked as
//	         one tab of the "lot": a tabbed container on a hidden workspace)
//	spawn  — run a command in a new terminal inside the right stack
//	open   — open a URL in a browser window inside the right stack, or focus
//	         the window if that URL is already open
//
// The shape of a displayed workstream (see README "How it works"):
//
//	workspace "N:WS"
//	└── splith                       mark jug:<name>        (root)
//	    ├── stacking  33%            mark jug:<name>:left   opencode
//	    └── stacking  67%            mark jug:<name>:right  terminals, editor, browser…
//
// Parked workstreams live in the lot, not in sway's scratchpad, so the
// user's scratchpad keys are unaffected:
//
//	workspace "WS+"                  (hidden; `jug lot` visits it)
//	└── tabbed                        mark jug:lot             (the lotbox)
//	    ├── splith  mark jug:<a>      a parked workstream, full size
//	    └── splith  mark jug:<b>
//
// Two sway facts shape the moves (verified in scripts/lot-poc.sh): moving a
// tiling container into an EMPTY workspace dissolves it into the workspace,
// and into a non-empty workspace inserts it beside that workspace's last
// focused view. So a root never travels by "move container to workspace"
// while tiling: it either targets the lotbox mark (stays intact) or hops
// through the scratchpad as a floating unit and is tiled again on arrival.
//
// The shape is not sacred to sway or to the user: closing a stack's last
// window reaps the stack (and its mark), moving the opencode window with
// the keyboard promotes it to a bare child of the root at 50/50 or drops
// it into the other stack, floating it twice does the same. The marks say
// where things SHOULD be; the windows (app_id jug-oc:<name>,
// jug-term:<name>) say where they ARE. ensure reconciles from the windows
// (scripts/ensure-poc.sh has the facts), so a workstream never gets a
// second opencode while one exists.
//
// Every multi-step sway change is sent as ONE command string, which sway
// applies in one transaction (no intermediate frames).
package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"context"

	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/oc"
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/sway"
)

// Engine binds sway, the store and the runtime state.
type Engine struct {
	Sway  *sway.Conn
	Cfg   config.Config
	Store store.Store
	State *store.State
	Debug func(format string, args ...any)
}

// Marks and app_ids. Marks go on split containers only (they would render
// in a view's title bar); views are identified by app_id.
func RootMark(name string) string  { return "jug:" + name }
func LeftMark(name string) string  { return "jug:" + name + ":left" }
func RightMark(name string) string { return "jug:" + name + ":right" }
func OpencodeAppID(name string) string {
	return "jug-oc:" + name
}

// LotMark is on the tabbed container that holds parked workstreams.
const LotMark = "jug:lot"

// MenuAppID is the app_id of the floating single-key menu window.
const MenuAppID = "jug-menu"

// LotName is the hidden workspace that holds the lotbox.
func (e *Engine) LotName() string { return e.Cfg.LotLabel }

// ErrNothingParked is returned by Lot when the lot does not exist.
var ErrNothingParked = errors.New("nothing is parked")

// SlotName is the workspace name while toggled, e.g. "4:WS".
func (e *Engine) SlotName(num int) string {
	return strconv.Itoa(num) + ":" + e.Cfg.SlotLabel
}

func (e *Engine) debugf(format string, args ...any) {
	if e.Debug != nil {
		e.Debug(format, args...)
	}
}

func (e *Engine) cmd(parts ...string) error {
	c := strings.Join(parts, "; ")
	e.debugf("swaymsg %s", c)
	_, err := e.Sway.Command(c)
	return err
}

func (e *Engine) save() error { return store.SaveState(e.Cfg.StateDir, e.State) }

// ---------------------------------------------------------------- slot

// ErrNoSlot is returned when no workspace is toggled.
var ErrNoSlot = errors.New("no workspace is toggled as the workstream slot (run `jug toggle` on one)")

// FocusedWorkspace returns the focused workspace entry.
func (e *Engine) FocusedWorkspace() (sway.Workspace, error) {
	wss, err := e.Sway.GetWorkspaces()
	if err != nil {
		return sway.Workspace{}, err
	}
	for _, w := range wss {
		if w.Focused {
			return w, nil
		}
	}
	return sway.Workspace{}, errors.New("no focused workspace")
}

// Toggle makes the focused workspace the slot, or un-makes it when it
// already is. If another workspace is the slot, that one is released first.
func (e *Engine) Toggle() (on bool, err error) {
	ws, err := e.FocusedWorkspace()
	if err != nil {
		return false, err
	}
	if e.State.Slot != nil && e.State.Slot.Num == ws.Num {
		return false, e.untoggle()
	}
	if e.State.Slot != nil {
		if err := e.untoggle(); err != nil {
			return false, err
		}
	}
	if ws.Num < 0 {
		return false, fmt.Errorf("workspace %q has no number; name workspaces \"N:name\" so the label can change while $mod+N keeps working (see README)", ws.Name)
	}
	name := e.SlotName(ws.Num)
	if err := e.cmd("rename workspace to " + sway.Quote(name)); err != nil {
		return false, err
	}
	e.State.Slot = &store.Slot{Num: ws.Num, OriginalName: ws.Name, Name: name}
	return true, e.save()
}

// untoggle parks the displayed workstream and restores the slot's name.
func (e *Engine) untoggle() error {
	slot := e.State.Slot
	if slot == nil {
		return nil
	}
	if e.State.Displayed != "" {
		if err := e.Park(); err != nil {
			return err
		}
	}
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	if ws := tree.WorkspaceNum(slot.Num); ws != nil && ws.Name == slot.Name {
		// rename works on the focused workspace only, so target it by criteria
		// on a child when it has one, else switch to it.
		if err := e.cmd(`rename workspace ` + sway.Quote(slot.Name) + ` to ` + sway.Quote(slot.OriginalName)); err != nil {
			return err
		}
	}
	e.State.Slot = nil
	return e.save()
}

// slotWorkspace returns the slot's workspace node, creating the workspace
// (and switching to it) when it was reaped for being empty.
func (e *Engine) slotWorkspace(tree *sway.Node, create bool) (*sway.Node, error) {
	slot := e.State.Slot
	if slot == nil {
		return nil, ErrNoSlot
	}
	if ws := tree.WorkspaceNum(slot.Num); ws != nil {
		return ws, nil
	}
	if !create {
		return nil, nil
	}
	if err := e.cmd("workspace number " + sway.Quote(slot.Name)); err != nil {
		return nil, err
	}
	tree, err := e.Sway.GetTree()
	if err != nil {
		return nil, err
	}
	ws := tree.WorkspaceNum(slot.Num)
	if ws == nil {
		return nil, fmt.Errorf("could not create workspace %s", slot.Name)
	}
	return ws, nil
}

// ---------------------------------------------------------------- show

// ShowOptions tunes Show.
type ShowOptions struct {
	// NoSwitch leaves the visible workspace and focus alone (used when a
	// program, not the user, asks for the workstream).
	NoSwitch bool
}

// Show displays w in the slot. If no slot exists the focused workspace
// becomes the slot.
func (e *Engine) Show(w *store.Workstream, opt ShowOptions) error {
	if e.State.Slot == nil {
		if _, err := e.Toggle(); err != nil {
			return err
		}
	}
	name := w.Name()
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	prevFocus := tree.FocusedNode()
	focusedWS, _ := e.FocusedWorkspace()
	onSlot := focusedWS.Num == e.State.Slot.Num
	if tree.ByAppID(OpencodeAppID(name)) == nil {
		// opencode is about to be started: make sure it has a session to resume
		if err := e.EnsureSession(w); err != nil {
			Notify("normal", "juggler", "opencode session: "+err.Error()+" — starting opencode without one")
		}
	}

	if e.State.Displayed != "" && e.State.Displayed != name {
		if err := e.parkTree(tree, e.State.Displayed); err != nil {
			return err
		}
		if tree, err = e.Sway.GetTree(); err != nil {
			return err
		}
	}

	root := tree.ByMark(RootMark(name))
	switch {
	case root == nil:
		// first time (or sway restarted / windows closed): build it — around
		// an opencode window that survived, when there is one
		if err := e.build(w); err != nil {
			return err
		}
	case !e.inSlot(tree, root):
		// parked (lot, scratchpad or floating anywhere): bring it back
		if err := e.restore(tree, name, onSlot, prevFocus); err != nil {
			return err
		}
	}
	// From here on the root is in the slot: record that first, so a failure
	// in the repair step cannot leave state.json contradicting the tree.
	e.State.Displayed = name
	st := e.State.Stream(name)
	st.Shown = time.Now().Format(time.RFC3339)
	if err := e.save(); err != nil {
		return err
	}
	ensureErr := e.ensure(w)

	// where should focus end up?
	tree, _ = e.Sway.GetTree()
	if !opt.NoSwitch {
		if err := e.cmd("workspace number "+sway.Quote(e.State.Slot.Name), e.focusCrit(tree, name)+" focus"); err != nil {
			e.debugf("focus: %v", err)
		}
	} else if !onSlot && prevFocus != nil {
		_ = e.cmd(sway.IDCrit(prevFocus.ID) + " focus")
	}
	if ensureErr != nil {
		return fmt.Errorf("%s is displayed but could not be fully repaired: %w", w.ID, ensureErr)
	}

	// opens requested while it was hidden
	pending := append([]string(nil), st.Pending...)
	st.Pending = nil
	_ = e.save()
	for _, key := range pending {
		if err := e.openPending(w, key); err != nil {
			e.debugf("pending open %s: %v", key, err)
		}
	}
	return nil
}

// inSlot reports whether root is tiling in the slot workspace.
func (e *Engine) inSlot(tree *sway.Node, root *sway.Node) bool {
	ws := tree.WorkspaceOf(root.ID)
	if ws == nil || e.State.Slot == nil || ws.Num != e.State.Slot.Num {
		return false
	}
	for _, f := range ws.FloatingNodes {
		if f.ByID(root.ID) != nil {
			return false
		}
	}
	return true
}

// restore brings a parked root back into the slot workspace with the
// canonical shape, in one transaction: strays already in the slot are
// adopted into the root's right stack (while it is still hidden), the root
// hops through the scratchpad so it arrives as a floating unit, is tiled
// into the now-empty slot (which wraps it), unwrapped, resized, and focus is
// put back where it was.
func (e *Engine) restore(tree *sway.Node, name string, onSlot bool, prevFocus *sway.Node) error {
	slot := e.State.Slot
	root := tree.ByMark(RootMark(name))
	if root == nil {
		return fmt.Errorf("%s has no windows", name)
	}
	rootC := sway.MarkCrit(RootMark(name))
	var cmds []string
	if ws := tree.WorkspaceNum(slot.Num); ws != nil && tree.ByMark(RightMark(name)) != nil {
		for _, n := range ws.Nodes {
			cmds = append(cmds, sway.IDCrit(n.ID)+" move container to mark "+sway.Quote(RightMark(name)))
		}
		cmds = append(cmds, e.returnStrays(tree, name, ws.Nodes)...)
	}
	if !tree.InScratchpad(root.ID) {
		cmds = append(cmds, rootC+" move scratchpad")
	}
	cmds = append(cmds,
		rootC+" move container to workspace "+sway.Quote(slot.Name),
		rootC+" floating disable",
		rootC+" split none", // undo the wrapper workspace_layout adds to a sole child
	)
	// A criteria that matches nothing fails the whole batch ("No matching
	// node."), so only touch stacks that exist; ensure() rebuilds the rest.
	if tree.ByMark(LeftMark(name)) != nil && len(root.Nodes) == 2 {
		cmds = append(cmds, sway.MarkCrit(LeftMark(name))+" resize set width "+strconv.Itoa(e.Cfg.LeftWidthPPT)+" ppt")
	}
	switch {
	case onSlot:
		cmds = append(cmds, "workspace number "+sway.Quote(slot.Name), e.focusCrit(tree, name)+" focus")
	case prevFocus != nil:
		cmds = append(cmds, sway.IDCrit(prevFocus.ID)+" focus")
	}
	return e.cmd(cmds...)
}

// focusCrit is the criteria for "the natural focus of workstream name":
// its opencode window, or its root when that window is gone.
func (e *Engine) focusCrit(tree *sway.Node, name string) string {
	if tree.ByAppID(OpencodeAppID(name)) != nil {
		return sway.AppIDCrit(OpencodeAppID(name))
	}
	return sway.MarkCrit(RootMark(name))
}

// DisplayedFromTree returns the workstream whose root is tiling in the slot
// workspace, or "" — the truth, when state.json disagrees (e.g. after a
// failed show).
func (e *Engine) DisplayedFromTree(tree *sway.Node) string {
	if e.State.Slot == nil {
		return ""
	}
	ws := tree.WorkspaceNum(e.State.Slot.Num)
	if ws == nil {
		return ""
	}
	for _, n := range ws.Nodes {
		for _, m := range n.Marks {
			if strings.HasPrefix(m, "jug:") && m != LotMark && !strings.HasSuffix(m, ":left") && !strings.HasSuffix(m, ":right") {
				return strings.TrimPrefix(m, "jug:")
			}
		}
	}
	return ""
}

// Reconcile makes state.json agree with the tree about what is displayed.
func (e *Engine) Reconcile() error {
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	if got := e.DisplayedFromTree(tree); got != e.State.Displayed {
		e.debugf("reconcile: displayed %q -> %q", e.State.Displayed, got)
		e.State.Displayed = got
		return e.save()
	}
	return nil
}

// build creates the windows and the tree shape for w (M0: F1–F5). It
// switches to the slot workspace because `exec` spawns on the focused one.
// An opencode window that survived without its root (the user closed the
// other stack, or moved the window out) is adopted, not duplicated: it is
// re-tiled at the top of the slot and the shape is built around it.
func (e *Engine) build(w *store.Workstream) error {
	name := w.Name()
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	ws, err := e.slotWorkspace(tree, true)
	if err != nil {
		return err
	}
	if err := e.cmd("workspace number " + sway.Quote(e.State.Slot.Name)); err != nil {
		return err
	}
	ocID, termID := OpencodeAppID(name), "jug-term:"+name

	// A surviving opencode window: hop it to the top of the slot. A floating
	// container is tiled beside/into the slot's most recently focused tiling
	// container, so the slot must hold none while it lands — tiling strays
	// wait in the scratchpad and are adopted into the right stack below.
	var held []int64
	oc := tree.ByAppID(ocID)
	if oc != nil {
		if err := e.cmd(sway.IDCrit(oc.ID) + " move scratchpad"); err != nil {
			return err
		}
		if tree, err = e.Sway.GetTree(); err != nil {
			return err
		}
		var cmds []string
		if ws = tree.WorkspaceNum(e.State.Slot.Num); ws != nil {
			for _, n := range ws.Nodes {
				cmds = append(cmds, sway.IDCrit(n.ID)+" move scratchpad")
				held = append(held, n.ID)
			}
		}
		cmds = append(cmds, sway.IDCrit(oc.ID)+" move container to workspace "+sway.Quote(e.State.Slot.Name),
			sway.IDCrit(oc.ID)+" floating disable")
		if err := e.cmd(cmds...); err != nil {
			return err
		}
	} else {
		// Strays: whatever already sits in the slot workspace. Focus the
		// workspace node itself so the opencode view spawns at the top level.
		if len(ws.Nodes) > 0 {
			if err := e.cmd(sway.IDCrit(ws.Nodes[0].ID)+" focus", "focus parent"); err != nil {
				return err
			}
		}
		// 1. opencode, left
		if err := e.EnsureSession(w); err != nil {
			Notify("normal", "juggler", "opencode session: "+err.Error()+" — starting opencode without one")
		}
		before := viewIDs(tree, ocID)
		if err := e.cmd("exec " + e.terminalCmd(w, ocID, e.opencodeCmd(w))); err != nil {
			return err
		}
		if oc, err = e.waitNewView(ocID, before, 15*time.Second); err != nil {
			return fmt.Errorf("opencode terminal did not appear: %w", err)
		}
	}
	tree, _ = e.Sway.GetTree()
	parent := tree.ParentOf(oc.ID)
	if parent == nil {
		return errors.New("lost the opencode window")
	}
	if parent.IsWorkspace() {
		// no workspace_layout stacking: make the workspace a stack around it;
		// the splith below then wraps that stack exactly like the normal path
		if err := e.cmd(sway.IDCrit(oc.ID) + " layout stacking"); err != nil {
			return err
		}
	}

	// 2. terminal, right — spawned with the WORKSPACE focused so it lands
	//    beside the opencode stack, not inside it
	before := viewIDs(tree, termID)
	if err := e.cmd(sway.IDCrit(oc.ID)+" focus", "focus parent", "exec "+e.terminalCmd(w, termID, "")); err != nil {
		return err
	}
	// a plain terminal has no unique app_id we can wait for by name… so we
	// gave it one — and wait for a NEW one, since every terminal of the
	// workstream carries it
	term, err := e.waitNewView(termID, before, 15*time.Second)
	if err != nil {
		return fmt.Errorf("terminal did not appear: %w", err)
	}

	// 3. wrap the terminal in its own stack, 4. wrap both stacks in the root
	if err := e.cmd(sway.IDCrit(term.ID)+" focus", "splitv", "layout stacking"); err != nil {
		return err
	}
	tree, _ = e.Sway.GetTree()
	right := tree.ParentOf(term.ID)
	left := tree.ParentOf(oc.ID)
	if right == nil || left == nil || right.IsWorkspace() || left.IsWorkspace() {
		return fmt.Errorf("unexpected tree after split: %s", tree.WorkspaceNum(e.State.Slot.Num).Shape())
	}
	if err := e.cmd(sway.IDCrit(left.ID)+" mark --add "+sway.Quote(LeftMark(name)),
		sway.IDCrit(right.ID)+" mark --add "+sway.Quote(RightMark(name))); err != nil {
		return err
	}
	// adopt strays into the right stack before wrapping: what is still in
	// the slot, and what waited in the scratchpad (floating now, so hopped)
	tree, _ = e.Sway.GetTree()
	ws = tree.WorkspaceNum(e.State.Slot.Num)
	var adopt []string
	var strays []*sway.Node
	for _, n := range ws.Nodes {
		if n.ID != left.ID && n.ID != right.ID {
			adopt = append(adopt, sway.IDCrit(n.ID)+" move container to mark "+sway.Quote(RightMark(name)))
			strays = append(strays, n)
		}
	}
	for _, id := range held {
		if n := tree.ByID(id); n != nil { // closed meanwhile otherwise
			adopt = append(adopt, hopInto(id, right, e.State.Slot.Name)...)
			strays = append(strays, n)
		}
	}
	adopt = append(adopt, e.returnStrays(tree, name, strays)...)
	if len(adopt) > 0 {
		if err := e.cmd(adopt...); err != nil {
			return err
		}
	}
	// the root: with the workspace focused, splith wraps all its children
	if err := e.cmd(sway.IDCrit(oc.ID)+" focus", "focus parent", "focus parent", "splith"); err != nil {
		return err
	}
	tree, _ = e.Sway.GetTree()
	ws = tree.WorkspaceNum(e.State.Slot.Num)
	if ws == nil || len(ws.Nodes) != 1 {
		return fmt.Errorf("unexpected tree after wrapping: %s", ws.Shape())
	}
	root := ws.Nodes[0]
	return e.cmd(
		sway.IDCrit(root.ID)+" mark --add "+sway.Quote(RootMark(name)),
		sway.IDCrit(left.ID)+" resize set width "+strconv.Itoa(e.Cfg.LeftWidthPPT)+" ppt",
		sway.IDCrit(oc.ID)+" focus",
	)
}

// returnStrays returns the commands that send other workstreams' opencode
// windows found among strays back to their own root (its left stack when it
// still has one). Strays adopted into name's right stack can include such a
// window — a workstream whose root was gone when it was parked left it
// behind — and inside another workstream's stack it would be out of reach
// of its own show. To be appended to the adoption batch: the ids survive
// the move. Nothing when there are none.
func (e *Engine) returnStrays(tree *sway.Node, name string, strays []*sway.Node) []string {
	var cmds []string
	for _, n := range strays {
		for _, v := range n.Views() {
			cmds = append(cmds, e.returnStray(tree, name, v)...)
		}
	}
	return cmds
}

// returnStray is returnStrays for one view.
func (e *Engine) returnStray(tree *sway.Node, name string, v *sway.Node) []string {
	other, ok := strings.CutPrefix(v.AppID, "jug-oc:")
	if !ok || other == name {
		return nil
	}
	target := tree.ByMark(LeftMark(other))
	if target == nil {
		target = tree.ByMark(RootMark(other))
	}
	if target == nil {
		return nil // no home to send it to; it stays a stray until its workstream is shown and built around it
	}
	ws := tree.WorkspaceOf(target.ID)
	if ws == nil {
		return nil
	}
	return hopInto(v.ID, target, ws.Name)
}

// ensure repairs a displayed workstream whose shape the user (or sway)
// changed: a stack closed (empty containers are reaped, taking our marks
// with them), the opencode window moved out of its stack, floated, left
// bare in the root, or carried off into another workstream's stack. It
// works from the WINDOWS, not the marks: the opencode window is found by
// app_id and adopted wherever it is — a second one is never started while
// one exists (two opencode TUIs on one session fight over it). Every batch
// ends by putting focus back where it was, so repairing a workstream on a
// non-visible slot never switches what the user sees.
//
// What sway does to the shape, verified by scripts/ensure-poc.sh: closing
// a stack's last view reaps the stack but NOT the root (it keeps one
// child); `move left/right` on a stack's only view promotes it to the root
// as a bare view with the width fractions reset (that is the 50/50);
// `floating toggle` twice lands it in the neighbouring stack.
func (e *Engine) ensure(w *store.Workstream) error {
	name := w.Name()
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	root := tree.ByMark(RootMark(name))
	left := tree.ByMark(LeftMark(name))
	right := tree.ByMark(RightMark(name))
	if root == nil && left == nil && right == nil {
		return nil // nothing left; Show will rebuild next time (build adopts a surviving opencode window)
	}
	ocID, termID := OpencodeAppID(name), "jug-term:"+name
	slotWS := e.State.Slot.Name
	prev := tree.FocusedNode()
	focusedWS, _ := e.FocusedWorkspace()
	onSlot := e.State.Slot != nil && focusedWS.Num == e.State.Slot.Num
	tail := func(focusNew *sway.Node) []string {
		if onSlot {
			if focusNew != nil {
				return []string{sway.IDCrit(focusNew.ID) + " focus"}
			}
			return nil
		}
		if prev != nil {
			return []string{sway.IDCrit(prev.ID) + " focus"}
		}
		return nil
	}
	run := func(focusNew *sway.Node, cmds ...string) error {
		return e.cmd(append(cmds, tail(focusNew)...)...)
	}
	reread := func() error {
		if tree, err = e.Sway.GetTree(); err != nil {
			return err
		}
		root, left, right = tree.ByMark(RootMark(name)), tree.ByMark(LeftMark(name)), tree.ByMark(RightMark(name))
		return nil
	}

	// 1. the root
	if root == nil {
		// re-wrap whatever survived
		keep := left
		if keep == nil {
			keep = right
		}
		if err := run(nil, sway.IDCrit(keep.ID)+" focus", "focus parent", "splith"); err != nil {
			return err
		}
		if err := reread(); err != nil {
			return err
		}
		p := tree.ParentOf(keep.ID)
		if p == nil || p.IsWorkspace() {
			return errors.New("could not re-wrap the workstream root")
		}
		if err := run(nil, sway.IDCrit(p.ID)+" mark --add "+sway.Quote(RootMark(name))); err != nil {
			return err
		}
		if err := reread(); err != nil {
			return err
		}
	}

	// 2. the right stack
	if right == nil {
		// Spawn with a child of the root focused: the new view becomes its
		// sibling inside the root (a new view is placed beside the focused
		// node; `move container to mark` would instead resolve to a leaf
		// and put it inside the other stack). The left stack when it is
		// there, else whatever the root still holds.
		anchor := left
		if anchor == nil {
			if len(root.Nodes) == 0 {
				return errors.New("cannot repair: the workstream root is empty")
			}
			anchor = root.Nodes[0]
		}
		before := viewIDs(tree, termID)
		if err := run(nil, sway.IDCrit(anchor.ID)+" focus", "exec "+e.terminalCmd(w, termID, "")); err != nil {
			return err
		}
		term, err := e.waitNewView(termID, before, 15*time.Second)
		if err != nil {
			return err
		}
		if err := run(nil, sway.IDCrit(term.ID)+" focus", "splitv", "layout stacking"); err != nil {
			return err
		}
		if err := reread(); err != nil {
			return err
		}
		p := tree.ParentOf(term.ID)
		if p == nil || p.IsWorkspace() || p.ID == root.ID {
			return errors.New("could not wrap the new terminal in a stack")
		}
		cmds := []string{sway.IDCrit(p.ID) + " mark --add " + sway.Quote(RightMark(name))}
		if left != nil {
			cmds = append(cmds, sway.IDCrit(left.ID)+" resize set width "+strconv.Itoa(e.Cfg.LeftWidthPPT)+" ppt")
		}
		if err := run(term, cmds...); err != nil {
			return err
		}
		if err := reread(); err != nil {
			return err
		}
	}

	// 3. opencode: the window, wherever it is; a new one only when there is none
	oc := tree.ByAppID(ocID)
	switch {
	case oc == nil:
		if err := e.EnsureSession(w); err != nil {
			Notify("normal", "juggler", "opencode session: "+err.Error()+" — starting opencode without one")
		}
		before := viewIDs(tree, ocID)
		if err := run(nil, sway.IDCrit(right.ID)+" focus", "exec "+e.terminalCmd(w, ocID, e.opencodeCmd(w))); err != nil {
			return err
		}
		if oc, err = e.waitNewView(ocID, before, 15*time.Second); err != nil {
			return err
		}
		if err := reread(); err != nil {
			return err
		}
	case left != nil && left.ByID(oc.ID) != nil:
		// in its stack: only the order can be off (an earlier repair swapped
		// a correct order into the wrong one)
		if indexIn(root, left.ID) > indexIn(root, right.ID) {
			return run(oc, sway.IDCrit(left.ID)+" swap container with mark "+sway.Quote(RightMark(name)),
				sway.IDCrit(left.ID)+" resize set width "+strconv.Itoa(e.Cfg.LeftWidthPPT)+" ppt")
		}
		return nil
	case left != nil:
		// the window wandered (into the right stack, floating, another
		// workspace, another workstream's root): bring it back
		return run(oc, hopInto(oc.ID, left, slotWS)...)
	}

	// No left stack: build one around the window (just started, or found).
	if p := tree.ParentOf(oc.ID); p == nil || p.ID != root.ID {
		if err := run(nil, hopInto(oc.ID, root, slotWS)...); err != nil {
			return err
		}
		if err := reread(); err != nil {
			return err
		}
	}
	// splitv wraps the view in a new container because it has a sibling
	// (the right stack); on a singleton it would relayout the root instead
	if err := run(nil, sway.IDCrit(oc.ID)+" focus", "splitv", "layout stacking"); err != nil {
		return err
	}
	if err := reread(); err != nil {
		return err
	}
	p := tree.ParentOf(oc.ID)
	if p == nil || p.IsWorkspace() || p.ID == root.ID {
		return errors.New("could not wrap the opencode window in a stack")
	}
	cmds := []string{sway.IDCrit(p.ID) + " mark --add " + sway.Quote(LeftMark(name))}
	if indexIn(root, p.ID) != 0 {
		// it landed after the right stack: swap the two (`move left` would
		// descend INTO the sibling stack rather than reorder)
		cmds = append(cmds, sway.IDCrit(p.ID)+" swap container with mark "+sway.Quote(RightMark(name)))
	}
	cmds = append(cmds, sway.IDCrit(p.ID)+" resize set width "+strconv.Itoa(e.Cfg.LeftWidthPPT)+" ppt")
	return run(oc, cmds...)
}

// ---------------------------------------------------------------- park

// Park hides the displayed workstream: strays in the slot are adopted into
// its right stack, then the root becomes a tab of the lotbox.
func (e *Engine) Park() error {
	if e.State.Displayed == "" {
		return nil
	}
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	return e.parkTree(tree, e.State.Displayed)
}

func (e *Engine) parkTree(tree *sway.Node, name string) error {
	root := tree.ByMark(RootMark(name))
	if root == nil {
		if e.State.Displayed == name {
			e.State.Displayed = ""
		}
		return e.save()
	}
	prev := tree.FocusedNode()
	focusedWS, _ := e.FocusedWorkspace()
	onSlot := e.State.Slot != nil && focusedWS.Num == e.State.Slot.Num
	rootC := sway.MarkCrit(RootMark(name))
	var cmds []string
	if e.inSlot(tree, root) {
		// the workstream's own opencode window travels with the root: bring
		// it back in first when it wandered out (moved, floated, adopted
		// elsewhere), else it would be left behind as a stray
		if oc := tree.ByAppID(OpencodeAppID(name)); oc != nil && root.ByID(oc.ID) == nil {
			target := tree.ByMark(LeftMark(name))
			if target == nil {
				target = root
			}
			cmds = append(cmds, hopInto(oc.ID, target, e.State.Slot.Name)...)
		}
		if tree.ByMark(RightMark(name)) != nil {
			var strays []*sway.Node
			for _, n := range tree.WorkspaceOf(root.ID).Nodes {
				if n.ID != root.ID {
					cmds = append(cmds, sway.IDCrit(n.ID)+" move container to mark "+sway.Quote(RightMark(name)))
					strays = append(strays, n)
				}
			}
			cmds = append(cmds, e.returnStrays(tree, name, strays)...)
		}
	}
	floating := tree.InScratchpad(root.ID) || !e.tiling(tree, root)
	lotbox := tree.ByMark(LotMark)
	if lotbox != nil {
		parent := tree.ParentOf(root.ID)
		if parent != nil && parent.ID == lotbox.ID {
			// already a tab
			if e.State.Displayed == name {
				e.State.Displayed = ""
			}
			return e.save()
		}
		if lotbox.ByID(root.ID) != nil {
			// nested inside another tab (a bad earlier move): lift it out
			cmds = append(cmds, rootC+" move scratchpad")
			floating = true
		}
		// A tiling container moved to a container mark becomes its child,
		// intact; a floating one would stay floating. So a floating root is
		// tiled in the SLOT workspace first — wherever it lands there, the
		// next command moves it out. (Tiling it in the lot would put it
		// inside a tab, and moving into an ancestor is a no-op.)
		if floating {
			cmds = append(cmds, rootC+" move container to workspace "+sway.Quote(e.State.Slot.Name), rootC+" floating disable")
		}
		cmds = append(cmds, rootC+" move container to mark "+sway.Quote(LotMark))
		cmds = append(cmds, e.focusAfter(onSlot, prev)...)
		if err := e.cmd(cmds...); err != nil {
			return err
		}
	} else {
		// No lot yet. Hop through the scratchpad so the root arrives floating
		// (moving it tiling into an empty workspace would dissolve it), tile
		// it — the empty workspace wraps it — and make that wrapper the lotbox.
		if !floating {
			cmds = append(cmds, rootC+" move scratchpad")
		}
		cmds = append(cmds, rootC+" move container to workspace "+sway.Quote(e.LotName()), rootC+" floating disable")
		cmds = append(cmds, e.focusAfter(onSlot, prev)...)
		if err := e.cmd(cmds...); err != nil {
			return err
		}
		tree, err := e.Sway.GetTree()
		if err != nil {
			return err
		}
		root = tree.ByMark(RootMark(name))
		wrapper := tree.ParentOf(root.ID)
		if wrapper == nil || wrapper.IsWorkspace() {
			return errors.New("the lot needs `workspace_layout stacking` (or tabbed) in the sway config: a parked workstream was not wrapped in a container (see README, Limitations)")
		}
		if err := e.cmd(sway.IDCrit(wrapper.ID)+" mark --add "+sway.Quote(LotMark), rootC+" layout tabbed"); err != nil {
			return err
		}
	}
	if e.State.Displayed == name {
		e.State.Displayed = ""
	}
	return e.save()
}

// ParkStrays moves every live, non-displayed workstream that is not already
// a tab of the lot — e.g. roots parked in the scratchpad by an older
// juggler — into the lot. The displayed workstream is left alone.
func (e *Engine) ParkStrays() error {
	all, err := e.Store.List()
	if err != nil {
		return err
	}
	for _, w := range all {
		tree, err := e.Sway.GetTree()
		if err != nil {
			return err
		}
		root := tree.ByMark(RootMark(w.Name()))
		if root == nil || w.Name() == e.State.Displayed {
			continue
		}
		if lot := tree.ByMark(LotMark); lot != nil {
			if p := tree.ParentOf(root.ID); p != nil && p.ID == lot.ID {
				continue // already a tab
			}
		}
		if err := e.parkTree(tree, w.Name()); err != nil {
			return fmt.Errorf("%s: %w", w.ID, err)
		}
	}
	return nil
}

// tiling reports whether root is a tiling node (not floating, not in the
// scratchpad).
func (e *Engine) tiling(tree *sway.Node, root *sway.Node) bool {
	ws := tree.WorkspaceOf(root.ID)
	if ws == nil {
		return false
	}
	for _, f := range ws.FloatingNodes {
		if f.ByID(root.ID) != nil {
			return false
		}
	}
	return true
}

// focusAfter returns the commands that put focus back after a tree move
// that may have shifted it (sway refocuses near the moved container).
func (e *Engine) focusAfter(onSlot bool, prev *sway.Node) []string {
	if onSlot && e.State.Slot != nil {
		return []string{"workspace number " + sway.Quote(e.State.Slot.Name)}
	}
	if prev != nil {
		return []string{sway.IDCrit(prev.ID) + " focus"}
	}
	return nil
}

// Lot switches to the lot workspace, or back if it is already focused.
func (e *Engine) Lot() error {
	ws, err := e.FocusedWorkspace()
	if err != nil {
		return err
	}
	if ws.Name == e.LotName() {
		if e.State.Slot != nil {
			return e.cmd("workspace number " + sway.Quote(e.State.Slot.Name))
		}
		return e.cmd("workspace back_and_forth")
	}
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	if tree.WorkspaceNamed(e.LotName()) == nil {
		return ErrNothingParked
	}
	return e.cmd("workspace " + sway.Quote(e.LotName()))
}

// RootUnderFocus returns the workstream name whose root contains the
// focused node, or "".
func RootUnderFocus(tree *sway.Node) string {
	f := tree.FocusedNode()
	if f == nil {
		return ""
	}
	for n := f; n != nil && !n.IsWorkspace(); n = tree.ParentOf(n.ID) {
		for _, m := range n.Marks {
			if strings.HasPrefix(m, "jug:") && m != LotMark && !strings.HasSuffix(m, ":left") && !strings.HasSuffix(m, ":right") {
				return strings.TrimPrefix(m, "jug:")
			}
		}
	}
	return ""
}

// Close kills every window of w (parked or displayed). Session data on disk
// is untouched; the next Show rebuilds.
func (e *Engine) Close(w *store.Workstream) error {
	name := w.Name()
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	root := tree.ByMark(RootMark(name))
	var cmds []string
	if root != nil {
		cmds = append(cmds, sway.MarkCrit(RootMark(name))+" kill")
	}
	// windows of this workstream that wandered out of the root (or survived
	// it): the opencode window and its terminals carry the workstream's
	// app_ids wherever they are
	for _, v := range tree.FindAll(func(n *sway.Node) bool {
		return n.AppID == OpencodeAppID(name) || n.AppID == "jug-term:"+name
	}) {
		if root == nil || root.ByID(v.ID) == nil {
			cmds = append(cmds, sway.IDCrit(v.ID)+" kill")
		}
	}
	if len(cmds) > 0 {
		if err := e.cmd(cmds...); err != nil {
			return err
		}
	}
	if e.State.Displayed == name {
		e.State.Displayed = ""
	}
	delete(e.State.Streams, name)
	return e.save()
}

// ---------------------------------------------------------------- spawn / open

// Spawn runs a command in a new terminal inside w's right stack (M0: F8).
// If w is not displayed it is an error; the hotkeys only ever target the
// displayed workstream.
func (e *Engine) Spawn(w *store.Workstream, title string, command string) error {
	name := w.Name()
	if e.State.Displayed != name {
		return fmt.Errorf("%s is not displayed", w.ID)
	}
	if err := e.ensure(w); err != nil {
		return err
	}
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	right := tree.ByMark(RightMark(name))
	if right == nil {
		return errors.New("right stack missing")
	}
	prev := tree.FocusedNode()
	focusedWS, _ := e.FocusedWorkspace()
	cmds := []string{
		sway.IDCrit(right.ID) + " focus",
		"focus child",
		"exec " + e.terminalCmdTitled(w, "jug-term:"+name, title, command),
	}
	if focusedWS.Num != e.State.Slot.Num && prev != nil {
		cmds = append(cmds, sway.IDCrit(prev.ID)+" focus")
	}
	return e.cmd(cmds...)
}

// FocusOpencode focuses w's opencode window (switching to the slot),
// rebuilding it first if it is gone.
func (e *Engine) FocusOpencode(w *store.Workstream) error {
	if err := e.ensure(w); err != nil {
		return err
	}
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	return e.cmd("workspace number "+sway.Quote(e.State.Slot.Name), e.focusCrit(tree, w.Name())+" focus")
}

// Open opens url in a browser window inside w's right stack. key names the
// window ("jira", "pr", or "url:<url>") so a second Open focuses the
// existing window instead of opening another. When w is not displayed the
// open is queued and runs on the next Show (M0: F9 — the browser ignores
// launch context, so the new window is caught and moved).
func (e *Engine) Open(w *store.Workstream, key, url string) (queued bool, err error) {
	name := w.Name()
	st := e.State.Stream(name)
	tree, err := e.Sway.GetTree()
	if err != nil {
		return false, err
	}
	if id, ok := st.Windows[key]; ok {
		if n := tree.ByID(id); n != nil && e.State.Displayed == name {
			return false, e.cmd("workspace number "+sway.Quote(e.State.Slot.Name), sway.IDCrit(id)+" focus")
		} else if n == nil {
			delete(st.Windows, key)
		}
	}
	if e.State.Displayed != name {
		for _, p := range st.Pending {
			if p == key {
				return true, e.save()
			}
		}
		st.Pending = append(st.Pending, key)
		return true, e.save()
	}
	if err := e.ensure(w); err != nil {
		return false, err
	}
	tree, _ = e.Sway.GetTree()
	right := tree.ByMark(RightMark(name))
	if right == nil {
		return false, errors.New("right stack missing")
	}
	prev := tree.FocusedNode()
	focusedWS, _ := e.FocusedWorkspace()

	before := map[int64]bool{}
	for _, n := range tree.FindAll(func(n *sway.Node) bool { return n.AppID == e.Cfg.BrowserAppID }) {
		before[n.ID] = true
	}
	ev, err := sway.Dial()
	if err != nil {
		return false, err
	}
	defer ev.Close()
	if err := ev.Subscribe("window"); err != nil {
		return false, err
	}
	if err := e.cmd("exec " + e.Cfg.Browser + " " + shellQuote(url)); err != nil {
		return false, err
	}
	deadline := time.Now().Add(20 * time.Second)
	var win *sway.Node
	for win == nil {
		if err := ev.SetDeadline(deadline); err != nil {
			return false, err
		}
		evt, err := ev.Next()
		if err != nil {
			return false, fmt.Errorf("no new %s window appeared: %w", e.Cfg.BrowserAppID, err)
		}
		if evt.Type != "window" {
			continue
		}
		var we sway.WindowEvent
		if json.Unmarshal(evt.Payload, &we) != nil || we.Change != "new" {
			continue
		}
		if we.Container.AppID == e.Cfg.BrowserAppID && !before[we.Container.ID] {
			c := we.Container
			win = &c
		}
	}
	cmds := []string{sway.IDCrit(win.ID) + " move container to mark " + sway.Quote(RightMark(name))}
	if focusedWS.Num == e.State.Slot.Num {
		cmds = append(cmds, sway.IDCrit(win.ID)+" focus")
	} else if prev != nil {
		cmds = append(cmds, sway.IDCrit(prev.ID)+" focus")
	}
	if err := e.cmd(cmds...); err != nil {
		return false, err
	}
	st.Windows[key] = win.ID
	return false, e.save()
}

// openPending resolves a queued key back to its URL and opens it.
func (e *Engine) openPending(w *store.Workstream, key string) error {
	url := ""
	switch {
	case strings.HasPrefix(key, "url:"):
		url = strings.TrimPrefix(key, "url:")
	default:
		if r := w.Ref(key); r != nil {
			url = r.URL
		}
	}
	if url == "" {
		return fmt.Errorf("no url for %q", key)
	}
	_, err := e.Open(w, key, url)
	return err
}

// ---------------------------------------------------------------- opencode sessions

// opencodeCmd is the command for the left stack: the configured opencode
// command plus `-s <id>` when the workstream has a pinned session.
func (e *Engine) opencodeCmd(w *store.Workstream) string {
	if e.Cfg.OpencodeSessions && w.OpencodeSession != "" {
		return e.Cfg.Opencode + " -s " + shellQuote(w.OpencodeSession)
	}
	return e.Cfg.Opencode
}

// EnsureSession gives w a pinned opencode session if it has none: a fresh,
// empty session titled after the workstream, created in its code dir.
func (e *Engine) EnsureSession(w *store.Workstream) error {
	if !e.Cfg.OpencodeSessions || w.OpencodeSession != "" {
		return nil
	}
	id, err := e.NewSession(w)
	if err != nil {
		return err
	}
	e.debugf("created opencode session %s for %s", id, w.ID)
	return nil
}

// NewSession creates a titled session for w, pins it and saves.
func (e *Engine) NewSession(w *store.Workstream) (string, error) {
	bin, err := oc.Binary(e.Cfg.Opencode)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv, err := oc.Start(ctx, bin, w.ResolvedCodeDir())
	if err != nil {
		return "", err
	}
	defer srv.Stop()
	sess, err := srv.Create(w.ResolvedCodeDir(), w.SessionTitle())
	if err != nil {
		return "", err
	}
	w.OpencodeSession = sess.ID
	return sess.ID, e.Store.Save(w)
}

// RetitleSession renames w's pinned opencode session to the workstream's
// current title. Best effort: a missing session or binary is not an error.
func (e *Engine) RetitleSession(w *store.Workstream) error {
	if !e.Cfg.OpencodeSessions || w.OpencodeSession == "" {
		return nil
	}
	bin, err := oc.Binary(e.Cfg.Opencode)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv, err := oc.Start(ctx, bin, w.ResolvedCodeDir())
	if err != nil {
		return err
	}
	defer srv.Stop()
	return srv.Rename(w.ResolvedCodeDir(), w.OpencodeSession, w.SessionTitle())
}

// RelaunchOpencode closes w's opencode window (if any) and, when w is
// displayed, starts it again — with the currently pinned session. A parked
// workstream gets its new opencode on the next show.
func (e *Engine) RelaunchOpencode(w *store.Workstream) error {
	name := w.Name()
	tree, err := e.Sway.GetTree()
	if err != nil {
		return err
	}
	if v := tree.ByAppID(OpencodeAppID(name)); v != nil {
		if err := e.cmd(sway.IDCrit(v.ID) + " kill"); err != nil {
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			t, err := e.Sway.GetTree()
			if err != nil {
				return err
			}
			if t.ByID(v.ID) == nil {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	if e.State.Displayed != name {
		return nil
	}
	if err := e.EnsureSession(w); err != nil {
		return err
	}
	return e.ensure(w)
}

// ---------------------------------------------------------------- helpers

// Env is the environment juggler gives every process it starts for w.
func Env(w *store.Workstream) map[string]string {
	return map[string]string{
		"JUG_WORKSTREAM":     w.Name(),
		"JUG_WORKSTREAM_ID":  w.ID,
		"JUG_WORKSTREAM_DIR": w.Dir,
		"JUG_CODE_DIR":       w.ResolvedCodeDir(),
	}
}

// terminalCmd builds `env … <terminal> --class <appid> --working-directory <dir> [-e sh -c <command>]`.
func (e *Engine) terminalCmd(w *store.Workstream, appID, command string) string {
	return e.terminalCmdTitled(w, appID, "", command)
}

func (e *Engine) terminalCmdTitled(w *store.Workstream, appID, title, command string) string {
	var b strings.Builder
	b.WriteString("env")
	for k, v := range Env(w) {
		b.WriteString(" " + k + "=" + shellQuote(v))
	}
	b.WriteString(" " + e.Cfg.Terminal)
	b.WriteString(" --class " + shellQuote(appID))
	b.WriteString(" --working-directory " + shellQuote(w.ResolvedCodeDir()))
	if title != "" {
		b.WriteString(" -T " + shellQuote(title))
	}
	if command != "" {
		b.WriteString(" -e sh -c " + shellQuote(command))
	}
	return b.String()
}

// viewIDs returns the ids of every view with appID — the set to pass to
// waitNewView before exec'ing another one.
func viewIDs(tree *sway.Node, appID string) map[int64]bool {
	out := map[int64]bool{}
	for _, n := range tree.FindAll(func(n *sway.Node) bool { return n.AppID == appID }) {
		out[n.ID] = true
	}
	return out
}

// waitNewView polls the tree until a view with appID exists that is not in
// before. app_ids are per workstream, not per window: a workstream's
// terminals all carry jug-term:<name>, and an opencode window that was moved
// out of its stack still carries jug-oc:<name>. Waiting for ANY view with
// the id would hand back one of those — and the repair would then be applied
// to the wrong window while the one just started lands wherever focus is.
func (e *Engine) waitNewView(appID string, before map[int64]bool, timeout time.Duration) (*sway.Node, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		tree, err := e.Sway.GetTree()
		if err != nil {
			return nil, err
		}
		for _, n := range tree.FindAll(func(n *sway.Node) bool { return n.AppID == appID }) {
			if !before[n.ID] {
				return n, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("timeout waiting for a new window %q", appID)
}

// hopInto returns the commands that make the container id a child of
// target, wherever id is now: tiling in another stack, floating, in the
// scratchpad, on another workspace, inside another workstream's root. It
// hops through the scratchpad (which detaches id from anything, as a
// floating unit), lands on target's workspace, and is tiled with target
// focused: sway tiles a floating container into the most recently focused
// tiling container of its workspace (`seat_get_focus_inactive_tiling`), so
// focusing a split container first makes id its child — deterministically,
// unlike `floating disable` on its own. `move container to mark` would do
// for a tiling id (it becomes the mark's child) but leaves a floating one
// floating; this works for both. The caller ends the batch with the focus
// it wants.
func hopInto(id int64, target *sway.Node, wsName string) []string {
	c := sway.IDCrit(id)
	return []string{
		c + " move scratchpad",
		sway.IDCrit(target.ID) + " focus",
		c + " move container to workspace " + sway.Quote(wsName),
		c + " floating disable",
	}
}

// indexIn returns the position of id among parent's tiling children, or -1.
func indexIn(parent *sway.Node, id int64) int {
	for i, c := range parent.Nodes {
		if c.ID == id {
			return i
		}
	}
	return -1
}

// shellQuote quotes s for /bin/sh in a way sway's own command splitter also
// understands. sway splits a batch on ";" while tracking quotes and
// backslash escapes — but an escaped quote directly followed by a quote
// (the shell idiom for a quote inside single quotes, when it ends a word)
// makes it lose track and hand the rest of the batch to the shell (verified
// on sway 1.9). So: a quote inside single quotes is written '"'"' (close, a
// double-quoted quote, reopen) and a backslash as '\\' (close, an escaped
// backslash, reopen) — never an odd run of backslashes before a quote. sh
// and sway read both identically.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`;,&|<>()*?[]{}~#!=") {
		return s
	}
	// quotes first, then backslashes (the quote encoding has no backslash,
	// the backslash encoding has quotes — this order keeps them apart)
	s = strings.ReplaceAll(s, "'", `'"'"'`)
	s = strings.ReplaceAll(s, `\`, `'\\'`)
	return "'" + s + "'"
}

// Notify raises a desktop notification (hotkey-invoked commands have no
// terminal to print to).
func Notify(urgency, title, body string) {
	_ = exec.Command("notify-send", "-a", "juggler", "-u", urgency, title, body).Run()
}

// StateDirFor returns the state dir, creating it.
func StateDirFor(cfg config.Config) (string, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return "", err
	}
	return cfg.StateDir, nil
}

// IsLive reports whether w currently has windows (displayed or parked):
// its root, or an opencode window that outlived the root. parked is true
// when those windows are not tiling in the slot workspace.
func (e *Engine) IsLive(tree *sway.Node, w *store.Workstream) (live bool, parked bool) {
	if root := tree.ByMark(RootMark(w.Name())); root != nil {
		return true, !e.inSlot(tree, root)
	}
	if oc := tree.ByAppID(OpencodeAppID(w.Name())); oc != nil {
		return true, !e.inSlot(tree, oc)
	}
	return false, false
}

// ShortDir abbreviates $HOME to ~ for display.
func ShortDir(p string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// ShellQuote is shellQuote for other packages.
func ShellQuote(s string) string { return shellQuote(s) }

// TodoFile is where `jug notes` edits.
func TodoFile(w *store.Workstream) string { return filepath.Join(w.Dir, "TODO.md") }
