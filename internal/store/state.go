package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// State is juggler's runtime state (what sway cannot remember for us).
// It lives in $XDG_STATE_HOME/juggler/state.json.
type State struct {
	// Slot is the workspace toggled into "workstream slot" mode, if any.
	Slot *Slot `json:"slot,omitempty"`
	// Displayed is the canonical name of the workstream shown in the slot.
	Displayed string `json:"displayed,omitempty"`
	// Streams holds per-workstream runtime facts, keyed by canonical name.
	Streams map[string]*StreamState `json:"streams,omitempty"`
}

// Slot describes the toggled workspace.
type Slot struct {
	Num          int    `json:"num"`           // the workspace number ($mod+N)
	OriginalName string `json:"original_name"` // e.g. "4:󰾗", restored on toggle-off
	Name         string `json:"name"`          // e.g. "4:WS"
}

// StreamState is what we remember about a workstream's windows.
type StreamState struct {
	// Windows maps a ref type / purpose ("jira", "pr", "url:<url>") to the
	// sway con_id of the browser window we opened for it.
	Windows map[string]int64 `json:"windows,omitempty"`
	// Pending are opens requested while the workstream was not displayed;
	// they run on the next show.
	Pending []string `json:"pending,omitempty"`
	// Shown is when the workstream was last displayed (for picker ordering).
	Shown string `json:"shown,omitempty"`
}

// Stream returns (creating) the state for name.
func (s *State) Stream(name string) *StreamState {
	if s.Streams == nil {
		s.Streams = map[string]*StreamState{}
	}
	st := s.Streams[name]
	if st == nil {
		st = &StreamState{}
		s.Streams[name] = st
	}
	if st.Windows == nil {
		st.Windows = map[string]int64{}
	}
	return st
}

// LoadState reads the state file; a missing file is an empty state.
func LoadState(dir string) (*State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &st, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// SaveState writes the state file atomically.
func SaveState(dir string, st *State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}
