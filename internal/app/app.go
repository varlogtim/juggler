// Package app is juggler's service layer: every operation the CLI, the REST
// API and the web UI offer, as functions that take inputs and return values
// instead of printing. The CLI (cmd/jug) formats results for a terminal;
// internal/web formats them as JSON. Nothing in here knows about either.
//
// Operations that touch sway take a *layout.Engine (one IPC connection plus
// the runtime state); get one with Engine (CLI: one per invocation) or
// WithEngine (server: one per request, serialized). Store-only operations
// take no engine and work without a compositor.
package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/sway"
)

// App binds the configuration and the store.
type App struct {
	Cfg   config.Config
	Store store.Store
	// Debug, when set, receives every sway command issued.
	Debug func(format string, args ...any)
	// DialFunc overrides how sway is reached (tests).
	DialFunc func() (*sway.Conn, error)
	// Scheduler is set when this process runs the source scheduler (jug
	// serve), so statuses can say "running / next run".
	Scheduler *Scheduler

	mu sync.Mutex
}

// New returns an App for cfg.
func New(cfg config.Config) *App {
	return &App{Cfg: cfg, Store: store.Store{Root: cfg.Root}}
}

// SwayError wraps a failure to reach sway so callers can map it (the web
// layer answers 503).
type SwayError struct{ Err error }

func (e *SwayError) Error() string { return e.Err.Error() }
func (e *SwayError) Unwrap() error { return e.Err }

// Dial opens a sway IPC connection.
func (a *App) Dial() (*sway.Conn, error) {
	if a.DialFunc != nil {
		return a.DialFunc()
	}
	return sway.Dial()
}

// Engine opens a sway connection, loads the runtime state and reconciles it
// with the tree. The caller must call done when finished.
func (a *App) Engine() (e *layout.Engine, done func(), err error) {
	conn, err := a.Dial()
	if err != nil {
		return nil, nil, &SwayError{err}
	}
	st, err := store.LoadState(a.Cfg.StateDir)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	e = &layout.Engine{Sway: conn, Cfg: a.Cfg, Store: a.Store, State: st, Debug: a.Debug}
	if st.Slot != nil {
		if err := e.Reconcile(); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}
	return e, func() { conn.Close() }, nil
}

// WithEngine runs fn with a fresh engine. Engine operations are serialized:
// two requests must not do tree surgery at the same time.
func (a *App) WithEngine(fn func(e *layout.Engine) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, done, err := a.Engine()
	if err != nil {
		return err
	}
	defer done()
	return fn(e)
}

// ---------------------------------------------------------------- lookup

// List returns every workstream, sorted by name.
func (a *App) List() ([]*store.Workstream, error) { return a.Store.List() }

// Resolve finds a workstream by canonical name, id or unique prefix.
func (a *App) Resolve(q string) (*store.Workstream, error) {
	w, err := a.Store.Resolve(q)
	if err != nil {
		if strings.HasPrefix(err.Error(), "no workstream matches") {
			return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err) // ambiguous
	}
	return w, nil
}

// Load reads a workstream by canonical name.
func (a *App) Load(name string) (*store.Workstream, error) {
	return a.Store.Load(filepath.Join(a.Cfg.Root, name))
}

// ErrNoTarget is returned when no workstream was named and none is displayed.
var ErrNoTarget = errors.New("no workstream displayed and none given")

// ErrNotFound marks "no such workstream / ref" errors (HTTP 404).
var ErrNotFound = errors.New("not found")

// ErrInvalid marks bad-input errors (HTTP 400).
var ErrInvalid = errors.New("invalid")

// Target resolves "which workstream": explicit, else $JUG_WORKSTREAM (set in
// every terminal juggler spawns), else the displayed one.
func (a *App) Target(explicit, displayed string) (*store.Workstream, error) {
	q := explicit
	if q == "" {
		q = os.Getenv("JUG_WORKSTREAM")
	}
	if q == "" {
		q = displayed
	}
	if q == "" {
		return nil, ErrNoTarget
	}
	return a.Store.Resolve(q)
}

// Displayed returns the displayed workstream's canonical name from the
// saved state (no sway needed; may be stale by one event).
func (a *App) Displayed() string {
	st, err := store.LoadState(a.Cfg.StateDir)
	if err != nil || st == nil {
		return ""
	}
	return st.Displayed
}

// ---------------------------------------------------------------- create

// CreateOptions are the inputs of Create.
type CreateOptions struct {
	Category string // "" = work with a ticket, else the configured default
	ID       string // "" = the ticket key, else <id_prefix>-NNNN
	Desc     string
	CodeDir  string // an existing directory ("" = the workstream dir)
	Jira     string // ticket key: becomes a jira ref (and the id)
	PR       string // pull request URL: becomes a pr ref
	Groups   []string
}

// Complete marks w done in juggler (your call, independent of the ticket).
func (a *App) Complete(w *store.Workstream) error {
	if !w.Completed.IsZero() {
		return nil
	}
	w.Completed = time.Now()
	return a.Store.Save(w)
}

// Reopen clears the completion mark.
func (a *App) Reopen(w *store.Workstream) error {
	if w.Completed.IsZero() {
		return nil
	}
	w.Completed = time.Time{}
	return a.Store.Save(w)
}

// RemoveHook: when a sourced workstream is removed, its key is tombstoned
// in that source's ledger so the next sync does not bring it back.
func (a *App) tombstoneIfSourced(w *store.Workstream) {
	if w.Source == "" {
		return
	}
	if r := identityRef(w); r != nil {
		_ = a.Tombstone(w.Source, r.Key)
	}
}

// CategoryFor applies the one rule: a workstream with a ticket is "work"
// unless the category was given explicitly.
func (a *App) CategoryFor(explicit, jira string) string {
	switch {
	case explicit != "":
		return explicit
	case jira != "":
		return "work"
	}
	return a.Cfg.DefaultCategory
}

// KnownCategories is what a "category:" prefix may name: the built-ins and
// every category already in use.
func (a *App) KnownCategories() map[string]bool {
	known := map[string]bool{"work": true, "personal": true, a.Cfg.DefaultCategory: true}
	if all, err := a.Store.List(); err == nil {
		for _, w := range all {
			known[w.Category] = true
		}
	}
	return known
}

// NextManualID returns the next free <id_prefix>-NNNN.
func (a *App) NextManualID() (string, error) { return a.Store.NextManualID(a.Cfg.IDPrefix) }

// JiraURL builds the browse URL for a ticket key; jira_base_url must be set.
func (a *App) JiraURL(key string) (string, error) {
	if a.Cfg.JiraBaseURL == "" {
		return "", fmt.Errorf("jira_base_url is not set in %s (e.g. \"https://yourcompany.atlassian.net\")", config.Path())
	}
	return strings.TrimRight(a.Cfg.JiraBaseURL, "/") + "/browse/" + strings.ToUpper(key), nil
}

var prURL = regexp.MustCompile(`https?://[^/]+/([^/]+/[^/]+)/pull/(\d+)`)

// PRKey turns https://host/owner/repo/pull/870 into owner/repo#870.
func PRKey(url string) string {
	if m := prURL.FindStringSubmatch(url); m != nil {
		return m[1] + "#" + m[2]
	}
	return url
}

// Create makes a workstream directory with its metadata, TODO.md and notes/.
func (a *App) Create(o CreateOptions) (*store.Workstream, error) {
	category := a.CategoryFor(o.Category, o.Jira)
	id := o.ID
	if id == "" && o.Jira != "" {
		id = strings.ToUpper(o.Jira)
	}
	desc := strings.TrimSpace(o.Desc)
	if desc == "" {
		return nil, fmt.Errorf("%w: a description is required", ErrInvalid)
	}
	if id == "" {
		var err error
		if id, err = a.NextManualID(); err != nil {
			return nil, err
		}
	}
	// ids are how people name workstreams; two with the same id would make
	// every `jug <verb> <id>` ambiguous
	if all, err := a.Store.List(); err == nil {
		for _, w := range all {
			if strings.EqualFold(w.ID, id) {
				return nil, fmt.Errorf("%w: a workstream with id %s already exists (%s)", ErrInvalid, w.ID, w.Name())
			}
		}
	}
	codeDir := o.CodeDir
	if codeDir != "" {
		abs, err := filepath.Abs(config.Expand(codeDir))
		if err != nil {
			return nil, err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%w: code dir %s is not a directory", ErrInvalid, abs)
		}
		codeDir = layout.ShortDir(abs)
	}
	w := &store.Workstream{ID: id, Category: category, Desc: desc, Created: time.Now(), CodeDir: codeDir}
	for _, g := range store.NormalizeGroups(o.Groups) {
		g, err := store.NormalizeGroupName(g)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		w.Groups = append(w.Groups, g)
	}
	if o.Jira != "" {
		u, err := a.JiraURL(o.Jira)
		if err != nil {
			return nil, err
		}
		w.Refs = append(w.Refs, store.Ref{Type: "jira", Key: strings.ToUpper(o.Jira), URL: u})
	}
	if o.PR != "" {
		w.Refs = append(w.Refs, store.Ref{Type: "pr", Key: PRKey(o.PR), URL: o.PR})
	}
	dir := filepath.Join(a.Cfg.Root, store.DirName(category, id, desc))
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("%w: %s already exists", ErrInvalid, dir)
	}
	w.Dir = dir
	if err := a.Store.Save(w); err != nil {
		return nil, err
	}
	return w, nil
}

// ---------------------------------------------------------------- refs

// AddRef attaches (or replaces) a ref. jira values are ticket keys, pr
// values are URLs; anything else is a URL stored as given.
func (a *App) AddRef(w *store.Workstream, typ, value, title, status string) (store.Ref, error) {
	typ = strings.ToLower(strings.TrimSpace(typ))
	value = strings.TrimSpace(value)
	if typ == "" || value == "" {
		return store.Ref{}, fmt.Errorf("%w: ref type and value are required", ErrInvalid)
	}
	r := store.Ref{Type: typ, URL: value, Title: title, Status: status}
	if status != "" || title != "" {
		r.Updated = time.Now()
	}
	switch typ {
	case "jira":
		r.Key = strings.ToUpper(value)
		u, err := a.JiraURL(value)
		if err != nil {
			return store.Ref{}, err
		}
		r.URL = u
	case "pr":
		r.Key = PRKey(value)
	}
	if !strings.Contains(r.URL, "://") {
		return store.Ref{}, fmt.Errorf("%w: %s ref needs a URL, got %q", ErrInvalid, typ, value)
	}
	replaced := false
	for i := range w.Refs {
		if w.Refs[i].Type == typ && (w.Refs[i].Key == r.Key || w.Refs[i].URL == r.URL) {
			w.Refs[i] = r
			replaced = true
		}
	}
	if !replaced {
		w.Refs = append(w.Refs, r)
	}
	return r, a.Store.Save(w)
}

// RemoveRef drops the ref of type typ whose key or URL equals ident ("" =
// the first ref of that type).
func (a *App) RemoveRef(w *store.Workstream, typ, ident string) error {
	kept := w.Refs[:0]
	removed := false
	for _, r := range w.Refs {
		match := r.Type == typ && (ident == "" || r.Key == ident || r.URL == ident)
		if match && !removed {
			removed = true
			continue
		}
		kept = append(kept, r)
	}
	if !removed {
		return fmt.Errorf("%w: %s has no %s ref %q", ErrNotFound, w.ID, typ, ident)
	}
	w.Refs = kept
	return a.Store.Save(w)
}

// ---------------------------------------------------------------- files

// Env is the environment juggler gives processes started for w.
func (a *App) Env(w *store.Workstream) map[string]string { return layout.Env(w) }

// TodoRead returns TODO.md.
func (a *App) TodoRead(w *store.Workstream) (string, error) {
	b, err := os.ReadFile(w.TodoPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// TodoWrite replaces TODO.md.
func (a *App) TodoWrite(w *store.Workstream, text string) error {
	return os.WriteFile(w.TodoPath(), []byte(text), 0o644)
}

// Dictate toggles dictator's notes mode into w's notes dir (w nil: the
// dictator default directory).
func (a *App) Dictate(w *store.Workstream) error {
	if a.Cfg.Dictator == "" {
		return errors.New("dictator is disabled in config")
	}
	argv := []string{"-notify", "toggle", "notes"}
	if w != nil {
		argv = []string{"-notify", "-dir", w.NotesDir(), "toggle", "notes"}
	}
	out, err := exec.Command(a.Cfg.Dictator, argv...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", a.Cfg.Dictator, strings.TrimSpace(string(out)))
	}
	return nil
}
