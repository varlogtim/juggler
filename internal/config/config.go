// Package config loads juggler's static configuration
// (~/.config/juggler/config.toml). Every field has a default, so the file is
// optional.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the user configuration.
type Config struct {
	// Root is the workstream store: one directory per workstream.
	Root string `toml:"root"`
	// StateDir holds runtime state (which workspace is the slot, which
	// workstream is displayed, browser window ids).
	StateDir string `toml:"state_dir"`

	// SlotLabel is the workspace label while a workspace is the workstream
	// slot ("4:<SlotLabel>").
	SlotLabel string `toml:"slot_label"`
	// LotLabel names the hidden "parking lot" workspace that holds parked
	// workstreams (as tabs). It shows up in the bar's workspace list.
	LotLabel string `toml:"lot_label"`

	// Terminal is the terminal emulator; it must accept
	// `--class <app_id> --working-directory <dir> -e <cmd...>` (alacritty, foot).
	Terminal string `toml:"terminal"`
	// Opencode is the command run in the left stack (inside Terminal). When
	// OpencodeSessions is on, juggler appends `-s <session id>` so each
	// workstream resumes its own session (see README, "Sessions").
	Opencode string `toml:"opencode"`
	// OpencodeSessions makes juggler create one opencode session per
	// workstream (titled after it) and pin its id in workstream.toml.
	OpencodeSessions bool `toml:"opencode_sessions"`
	// Editor is used by `jug notes` and `jug review`.
	Editor string `toml:"editor"`
	// Browser opens URLs; the URL is appended.
	Browser string `toml:"browser"`
	// BrowserAppID is the sway app_id of Browser's windows, used to catch the
	// new window and move it into the right stack.
	BrowserAppID string `toml:"browser_app_id"`
	// Dictator is the dictation client used by `jug dictate` ("" disables).
	Dictator string `toml:"dictator"`

	// LeftWidthPPT is the left stack width in percent of the workspace.
	LeftWidthPPT int `toml:"left_width_ppt"`
	// DefaultCategory is used by `jug add` when none is given.
	DefaultCategory string `toml:"default_category"`
	// IDPrefix names workstreams that have no external key: <IDPrefix>-0001.
	// Default: your user name.
	IDPrefix string `toml:"id_prefix"`
	// JiraBaseURL builds ticket URLs from keys (jira refs), e.g.
	// "https://yourcompany.atlassian.net". Required for --jira / jira refs.
	JiraBaseURL string `toml:"jira_base_url"`

	// CodeSubdir is where a workstream's own worktree lives inside its
	// directory: <workstream>/<CodeSubdir>/<repo>.
	CodeSubdir string `toml:"code_subdir"`
	// Listen is the address `jug serve` binds (web UI + REST API). Keep it
	// on loopback: the API runs commands as you, with no authentication.
	Listen string `toml:"listen"`
	// Repos maps a short name (what `--repo` accepts) to a main checkout
	// that owns .git; new worktrees are linked to it.
	Repos map[string]Repo `toml:"repos"`
}

// Repo is a main checkout new worktrees are linked to.
type Repo struct {
	Path          string `toml:"path"`
	Remote        string `toml:"remote,omitempty"`         // default origin
	DefaultBranch string `toml:"default_branch,omitempty"` // base for new branches (default: the remote's HEAD)
	// SeedDir holds files copied into every new worktree of this repo
	// (untracked per-checkout files such as .envrc). Text files are
	// templated: {{id}} {{name}} {{repo}} {{dir}} {{code_dir}} {{home}}.
	// Default: ~/.config/juggler/seed/<repo name>, when it exists.
	SeedDir string `toml:"seed_dir,omitempty"`
}

// SeedDirFor returns the seed dir for repo name ("" when there is none).
func (c Config) SeedDirFor(name string) string {
	if r, ok := c.Repos[name]; ok && r.SeedDir != "" {
		return Expand(r.SeedDir)
	}
	d := filepath.Join(filepath.Dir(Path()), "seed", name)
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		return d
	}
	return ""
}

// Default returns the built-in configuration.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Root:             filepath.Join(home, "workstreams"),
		StateDir:         filepath.Join(xdgState(home), "juggler"),
		SlotLabel:        "WS",
		LotLabel:         "WS+",
		Terminal:         "alacritty",
		Opencode:         "opencode",
		OpencodeSessions: true,
		Editor:           "nvim",
		Browser:          "google-chrome --new-window",
		BrowserAppID:     "google-chrome",
		Dictator:         "dictator",
		LeftWidthPPT:     33,
		DefaultCategory:  "personal",
		IDPrefix:         userName(),
		JiraBaseURL:      "",
		CodeSubdir:       "src",
		Listen:           "127.0.0.1:7474",
	}
}

func userName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "ws"
}

func xdgState(home string) string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return d
	}
	return filepath.Join(home, ".local", "state")
}

// Path returns the config file path ($JUGGLER_CONFIG or
// ~/.config/juggler/config.toml).
func Path() string {
	if p := os.Getenv("JUGGLER_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "juggler", "config.toml")
}

// Load reads the config file over the defaults. A missing file is fine.
func Load() (Config, error) {
	cfg := Default()
	p := Path()
	if _, err := toml.DecodeFile(p, &cfg); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("%s: %w", p, err)
		}
	}
	cfg.Root = Expand(cfg.Root)
	cfg.StateDir = Expand(cfg.StateDir)
	for k, r := range cfg.Repos {
		r.Path = Expand(r.Path)
		if r.Remote == "" {
			r.Remote = "origin"
		}
		cfg.Repos[k] = r
	}
	return cfg, nil
}

// Expand expands a leading "~" and $VARS in a path.
func Expand(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		p = home + p[1:]
	}
	return os.ExpandEnv(p)
}
