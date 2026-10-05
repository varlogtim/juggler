// Package bar streams the displayed workstream to a status bar (waybar
// custom module, `return-type: json`).
package bar

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/sway"
)

// Snapshot is what the bar shows.
type Snapshot struct {
	Slot      *store.Slot
	W         *store.Workstream // displayed, or nil
	OnSlot    bool              // the slot workspace is focused
	SwayAlive bool
}

// Render formats a snapshot for a given output format.
type Render func(Snapshot) string

// Renderers by name.
var Renderers = map[string]Render{
	"plain":  renderPlain,
	"json":   renderJSON,
	"waybar": renderWaybar,
}

func text(s Snapshot) string {
	if s.Slot == nil {
		return ""
	}
	if s.W == nil {
		return "WS  —  (Super+t to pick)"
	}
	parts := []string{s.W.ID, s.W.Desc}
	if r := s.W.Ref("jira"); r != nil && r.Status != "" {
		parts = append(parts, r.Status)
	}
	if r := s.W.Ref("pr"); r != nil {
		label := r.Key
		if i := strings.LastIndex(label, "#"); i >= 0 {
			label = "PR" + label[i:]
		}
		if r.Status != "" {
			label += " " + r.Status
		}
		parts = append(parts, label)
	}
	if n := s.W.OpenTodos(); n > 0 {
		parts = append(parts, fmt.Sprintf("todo %d", n))
	}
	if s.W.CodeDir == "" {
		parts = append(parts, "no code")
	}
	return "WS  " + strings.Join(parts, "  ·  ")
}

func renderPlain(s Snapshot) string { return text(s) }

func renderJSON(s Snapshot) string {
	m := map[string]any{"slot": s.Slot, "on_slot": s.OnSlot}
	if s.W != nil {
		m["workstream"] = s.W.Name()
		m["id"] = s.W.ID
		m["desc"] = s.W.Desc
		m["refs"] = s.W.Refs
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func renderWaybar(s Snapshot) string {
	classes := []string{}
	var tip []string
	switch {
	case s.Slot == nil:
		classes = append(classes, "noslot")
	case s.W == nil:
		classes = append(classes, "empty")
		tip = append(tip, fmt.Sprintf("workspace %s is the workstream slot; nothing displayed", s.Slot.Name))
	default:
		classes = append(classes, "displayed")
		tip = append(tip, s.W.Name(), "code: "+layout.ShortDir(s.W.ResolvedCodeDir()))
		for _, r := range s.W.Refs {
			line := r.Type + ": " + r.URL
			if r.Status != "" {
				line += "  [" + r.Status + "]"
			}
			tip = append(tip, line)
		}
	}
	if s.Slot != nil && !s.OnSlot {
		classes = append(classes, "away")
	}
	b, _ := json.Marshal(map[string]any{
		"text":    text(s),
		"class":   classes,
		"tooltip": strings.Join(tip, "\n"),
	})
	return string(b)
}

// Watch prints a rendered snapshot whenever it changes, until stdout closes.
// It re-reads state on every sway workspace/window event and every tick.
func Watch(cfg config.Config, st store.Store, render Render) int {
	out := bufio.NewWriter(os.Stdout)
	last := "\x00"
	emit := func(s Snapshot) bool {
		line := render(s)
		if line == last {
			return true
		}
		last = line
		if _, err := fmt.Fprintln(out, line); err != nil {
			return false
		}
		return out.Flush() == nil
	}
	for {
		conn, err := sway.Dial()
		if err != nil {
			if !emit(Snapshot{}) {
				return 0
			}
			time.Sleep(2 * time.Second)
			continue
		}
		if err := conn.Subscribe("workspace", "window"); err != nil {
			conn.Close()
			time.Sleep(2 * time.Second)
			continue
		}
		if !emit(snapshot(cfg, st)) {
			conn.Close()
			return 0
		}
		for {
			// Our consumer (waybar) only notices us through writes, and we
			// only write on change — so detect its death by reparenting.
			if os.Getppid() == 1 {
				conn.Close()
				return 0
			}
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Next(); err != nil {
				if isTimeout(err) {
					if !emit(snapshot(cfg, st)) {
						conn.Close()
						return 0
					}
					continue
				}
				break // reconnect
			}
			if !emit(snapshot(cfg, st)) {
				conn.Close()
				return 0
			}
		}
		conn.Close()
		time.Sleep(time.Second)
	}
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

func snapshot(cfg config.Config, st store.Store) Snapshot {
	state, err := store.LoadState(cfg.StateDir)
	if err != nil || state.Slot == nil {
		return Snapshot{SwayAlive: true}
	}
	s := Snapshot{Slot: state.Slot, SwayAlive: true}
	if state.Displayed != "" {
		if w, err := st.Load(cfg.Root + "/" + state.Displayed); err == nil {
			s.W = w
		}
	}
	if c, err := sway.Dial(); err == nil {
		if wss, err := c.GetWorkspaces(); err == nil {
			for _, w := range wss {
				if w.Focused && w.Num == state.Slot.Num {
					s.OnSlot = true
				}
			}
		}
		c.Close()
	}
	return s
}
