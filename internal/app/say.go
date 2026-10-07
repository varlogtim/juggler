package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/oc"
	"github.com/varlogtim/juggler/internal/store"
)

// Talking to a workstream's opencode session from outside — another
// workstream's session, a script, the web UI.
//
// The session lives in opencode's store, but the process that shows it is
// the TUI in the workstream's window, and the TUI only renders what goes
// through ITS server. So juggler starts every TUI with `--port <p>` (see
// layout.EnsureSession) and talks to that: a prompt sent there appears in
// the window like a typed one, the model turn runs in that process, and
// permission prompts it raises are answered there, by the human looking at
// that window — never here. Writing to the session from a second server
// would be invisible to the TUI and race it, so when there is a window
// but it does not answer, Say refuses rather than falling back.

// probeTimeout bounds "is the TUI there": loopback, so anything slower is
// not a live server.
const probeTimeout = 1500 * time.Millisecond

// SayWaitTimeout is how long Say waits for a reply (or for idle) by default.
const SayWaitTimeout = 20 * time.Minute

// SessionState is where a workstream's opencode session stands, as seen
// from outside: is a TUI serving it, and what is it doing.
type SessionState struct {
	ID      string `json:"id"`                // workstream id
	Name    string `json:"name"`              // canonical name
	Session string `json:"session,omitempty"` // pinned session id
	Port    int    `json:"port,omitempty"`    // where its TUI serves the HTTP API
	// Status: none (no session pinned) | no-port (pinned, but the TUI was
	// started before ports existed) | closed (nothing answers on the port:
	// no live TUI) | idle | busy | retry (what the TUI reports) | error
	// (it answered health but not the rest; Detail says what).
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`  // retry: attempt and message; closed/error: why
	Version string `json:"version,omitempty"` // the opencode answering
	// Pending lists permission prompts waiting in that window for this
	// session — the one way a "busy" session is actually stuck on a human.
	Pending []string `json:"pending_permissions,omitempty"`
}

// Live reports whether a TUI answered for the session.
func (s SessionState) Live() bool {
	switch s.Status {
	case "idle", "busy", "retry", "error":
		return true
	}
	return false
}

// Line is the one-line form for a terminal.
func (s SessionState) Line() string {
	out := s.Status
	if s.Port > 0 && s.Live() {
		out += "  127.0.0.1:" + strconv.Itoa(s.Port)
	}
	if len(s.Pending) > 0 {
		out += "  permission pending: " + strings.Join(s.Pending, "; ")
	}
	if s.Detail != "" {
		out += "  (" + s.Detail + ")"
	}
	return out
}

// SessionState probes w's TUI (bounded by probeTimeout per request).
func (a *App) SessionState(ctx context.Context, w *store.Workstream) SessionState {
	st := SessionState{ID: w.ID, Name: w.Name(), Session: w.OpencodeSession, Port: w.OpencodePort}
	switch {
	case w.OpencodeSession == "":
		st.Status = "none"
		return st
	case w.OpencodePort == 0:
		st.Status = "no-port"
		st.Detail = "its opencode was started before ports: `jug relaunch " + w.ID + "`"
		return st
	}
	c := oc.Loopback(w.OpencodePort)
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	v, err := c.Health(pctx)
	if err != nil {
		st.Status = "closed"
		st.Detail = "nothing answers on 127.0.0.1:" + strconv.Itoa(w.OpencodePort)
		return st
	}
	st.Version = v
	statuses, err := c.Statuses(pctx)
	if err != nil {
		st.Status = "error"
		st.Detail = err.Error()
		return st
	}
	if s, ok := statuses[w.OpencodeSession]; ok && s.Type != "" {
		st.Status = s.Type
		if s.Type == "retry" {
			st.Detail = fmt.Sprintf("attempt %d: %s", s.Attempt, s.Message)
		}
	} else {
		st.Status = "idle"
	}
	if perms, err := c.Permissions(pctx); err == nil {
		for _, p := range perms {
			if p.SessionID == w.OpencodeSession {
				st.Pending = append(st.Pending, p.Summary())
			}
		}
	}
	return st
}

// SessionStates probes every workstream that has a session, concurrently,
// in name order.
func (a *App) SessionStates(ctx context.Context) ([]SessionState, error) {
	all, err := a.Store.List()
	if err != nil {
		return nil, err
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out []SessionState
	)
	for _, w := range all {
		if w.OpencodeSession == "" {
			continue
		}
		wg.Add(1)
		go func(w *store.Workstream) {
			defer wg.Done()
			st := a.SessionState(ctx, w)
			mu.Lock()
			out = append(out, st)
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SayMode is how a message reaches the session.
type SayMode string

const (
	SayAsync SayMode = "async" // prompt, return once accepted; the turn runs in the window
	SayWait  SayMode = "wait"  // prompt and wait for the reply
	SayDraft SayMode = "draft" // put the text in the TUI's prompt box, do not submit
)

// SayOptions tune Say.
type SayOptions struct {
	Mode    SayMode
	Timeout time.Duration // wait / queue: bound (0 = SayWaitTimeout)
	Queue   bool          // busy: wait for idle first instead of refusing
	Force   bool          // busy: send anyway (opencode queues or drops it, version-dependent)
	// Stdout/Stderr receive the output of a headless `opencode run` (wait
	// mode, no window). nil Stdout = capture into the result's Reply.
	Stdout, Stderr io.Writer
}

// SayResult says what Say did.
type SayResult struct {
	ID      string  `json:"id"`
	Session string  `json:"session"`
	Mode    SayMode `json:"mode"`
	Via     string  `json:"via"`              // tui (the live window's server) | run (headless `opencode run`)
	Port    int     `json:"port,omitempty"`   // tui
	Waited  string  `json:"waited,omitempty"` // queue: how long for idle
	Reply   string  `json:"reply,omitempty"`  // wait: the assistant's text
	Cost    float64 `json:"cost,omitempty"`   // wait via tui
}

// ErrBusy is returned when the session is in a turn and neither Queue nor
// Force was given.
var ErrBusy = errors.New("session is busy")

// ErrNoLiveTUI is returned when nothing serves the session and the mode
// needs a window.
var ErrNoLiveTUI = errors.New("no live opencode window")

// Say sends text to w's opencode session. See the package comment above
// for why it goes through the window's own server, and when it refuses.
func (a *App) Say(ctx context.Context, w *store.Workstream, text string, o SayOptions) (SayResult, error) {
	res := SayResult{ID: w.ID, Session: w.OpencodeSession, Mode: o.Mode, Port: w.OpencodePort}
	text = strings.TrimSpace(text)
	if text == "" {
		return res, errors.New("nothing to say: the message is required")
	}
	if o.Mode == "" {
		o.Mode = SayAsync
		res.Mode = SayAsync
	}
	if !a.Cfg.OpencodeSessions {
		return res, errors.New("opencode_sessions is off in the config: juggler does not know this workstream's session")
	}
	if w.OpencodeSession == "" {
		return res, ErrNoSession
	}
	if self := os.Getenv("JUG_WORKSTREAM"); self != "" && self == w.Name() {
		return res, fmt.Errorf("refusing to message the workstream this command runs in (%s): pass --ws", w.ID)
	}
	if o.Timeout <= 0 {
		o.Timeout = SayWaitTimeout
	}

	st := a.SessionState(ctx, w)
	if !st.Live() {
		// No TUI answers. Is there a window anyway? Then the TUI is there
		// but unreachable (started before ports, or starting up): a
		// headless turn now would be a second writer behind its back.
		if tree, _ := a.snapshot(); tree != nil && tree.ByAppID(layout.OpencodeAppID(w.Name())) != nil {
			if st.Status == "no-port" {
				return res, fmt.Errorf("%s has an opencode window started before ports — `jug relaunch %s` first", w.ID, w.ID)
			}
			return res, fmt.Errorf("%s has an opencode window but %s — starting up? else `jug relaunch %s`", w.ID, st.Detail, w.ID)
		}
		switch o.Mode {
		case SayWait:
			return a.sayHeadless(ctx, w, text, o, res)
		case SayDraft:
			return res, fmt.Errorf("%w for %s to draft into: `jug show %s` starts one", ErrNoLiveTUI, w.ID, w.ID)
		default:
			return res, fmt.Errorf("%w for %s: `jug show %s` starts one (the turn then runs there), or `jug say --wait` runs it headless now", ErrNoLiveTUI, w.ID, w.ID)
		}
	}
	if st.Status == "error" {
		return res, fmt.Errorf("%s's opencode answers but misbehaves: %s", w.ID, st.Detail)
	}

	c := oc.Loopback(w.OpencodePort)
	res.Via = "tui"
	if o.Mode == SayDraft {
		dctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		return res, c.AppendPrompt(dctx, text)
	}

	// busy: a prompt sent now is queued or dropped by opencode, version
	// permitting — not something to do silently. Wait for idle on request.
	if st.Status != "idle" && !o.Force {
		if !o.Queue {
			return res, fmt.Errorf("%w: %s is %s; `jug session status --ws %s` to watch it, --queue to send when idle, --force to send now", ErrBusy, w.ID, st.Line(), w.ID)
		}
		start := time.Now()
		wctx, cancel := context.WithTimeout(ctx, o.Timeout)
		defer cancel()
		for st.Status != "idle" {
			select {
			case <-wctx.Done():
				return res, fmt.Errorf("%s still %s after %s: %w", w.ID, st.Line(), o.Timeout.Round(time.Second), wctx.Err())
			case <-time.After(2 * time.Second):
			}
			st = a.SessionState(ctx, w)
			if !st.Live() {
				return res, fmt.Errorf("%s's opencode went away while waiting (%s)", w.ID, st.Status)
			}
		}
		res.Waited = time.Since(start).Round(time.Second).String()
	}

	switch o.Mode {
	case SayWait:
		wctx, cancel := context.WithTimeout(ctx, o.Timeout)
		defer cancel()
		rep, err := c.Message(wctx, w.OpencodeSession, text)
		if err != nil {
			if errors.Is(wctx.Err(), context.DeadlineExceeded) {
				return res, fmt.Errorf("no reply from %s within %s (it keeps working; `jug session status --ws %s`)", w.ID, o.Timeout.Round(time.Second), w.ID)
			}
			return res, err
		}
		res.Reply, res.Cost = rep.Text(), rep.Info.Cost
		return res, rep.Err()
	default:
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		return res, c.PromptAsync(pctx, w.OpencodeSession, text)
	}
}

// sayHeadless continues the session with `opencode run -s <id>` in the
// code dir, with the workstream's JUG_* environment — the thing to do when
// no window shows the session: nothing is racing it, and the reply comes
// back here. No human is attached, so a permission prompt cannot be
// answered.
func (a *App) sayHeadless(ctx context.Context, w *store.Workstream, text string, o SayOptions, res SayResult) (SayResult, error) {
	bin, err := oc.Binary(a.Cfg.Opencode)
	if err != nil {
		return res, err
	}
	res.Via, res.Port = "run", 0
	rctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, bin, "run", "-s", w.OpencodeSession, "--dir", w.ResolvedCodeDir(), text)
	cmd.Dir = w.ResolvedCodeDir()
	cmd.Env = os.Environ()
	for k, v := range layout.Env(w) {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var captured strings.Builder
	if o.Stdout != nil {
		cmd.Stdout = o.Stdout
	} else {
		cmd.Stdout = &captured
	}
	if o.Stderr != nil {
		cmd.Stderr = o.Stderr
	} else {
		cmd.Stderr = &captured
	}
	err = cmd.Run()
	res.Reply = strings.TrimSpace(captured.String())
	if err != nil {
		if errors.Is(rctx.Err(), context.DeadlineExceeded) {
			return res, fmt.Errorf("opencode run did not finish within %s (killed)", o.Timeout.Round(time.Second))
		}
		return res, fmt.Errorf("opencode run: %w", err)
	}
	return res, nil
}
