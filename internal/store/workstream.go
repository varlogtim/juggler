// Package store is the workstream store: one directory per workstream under
// the root, each holding a workstream.toml, a TODO.md and a notes/ dir.
//
//	~/workstreams/
//	  work_AISW-53270_disagg-toggle-requires-pause/
//	    workstream.toml
//	    TODO.md
//	    notes/
//
// The directory name is <category>_<id>_<slug> and is the workstream's
// canonical name; the id (a Jira key, a GitHub issue, or tim-NNNN) is the
// short handle people type.
package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// FileName is the per-workstream metadata file.
const FileName = "workstream.toml"

// Ref links a workstream to something external: the ticket (leadership's
// interface), the PR (the code's interface), or any URL.
type Ref struct {
	Type    string    `toml:"type" json:"type"`                   // jira | pr | issue | url
	Key     string    `toml:"key,omitempty" json:"key,omitempty"` // AISW-53270, hpe/ezaddon-mlis#870
	URL     string    `toml:"url" json:"url"`
	Title   string    `toml:"title,omitempty" json:"title,omitempty"`
	Status  string    `toml:"status,omitempty" json:"status,omitempty"`   // cached external state, in the system's own words
	Closed  bool      `toml:"closed,omitempty" json:"closed,omitempty"`   // the external item is finished (normalized by the source)
	Updated time.Time `toml:"updated,omitempty" json:"updated,omitempty"` // when Status was last observed
}

// Workstream is the metadata file.
type Workstream struct {
	ID       string    `toml:"id" json:"id"`
	Category string    `toml:"category" json:"category"`
	Desc     string    `toml:"desc" json:"desc"`
	Created  time.Time `toml:"created" json:"created"`
	// CodeDir is the checkout the workstream works in (cwd for opencode and
	// terminals). Relative = inside the workstream directory (its own git
	// worktree, see `jug add --repo`); empty = the workstream directory.
	CodeDir string `toml:"code_dir,omitempty" json:"code_dir,omitempty"`
	// OpencodeSession pins the opencode session this workstream resumes
	// (`opencode -s <id>`). Empty means none yet: juggler creates one on the
	// next show (if opencode_sessions is on in the config).
	OpencodeSession string `toml:"opencode_session,omitempty" json:"opencode_session,omitempty"`
	Refs            []Ref  `toml:"refs,omitempty" json:"refs,omitempty"`
	// Groups tags the workstream into buckets (a sprint, a project, …).
	// Membership lives here; what a group is (dates, kind) lives in the
	// registry, see groups.go.
	Groups []string `toml:"groups,omitempty" json:"groups"`
	// Source names the configured source (e.g. a Jira board) that created
	// or adopted this workstream and keeps its identity ref and managed
	// group tags up to date. Empty: made by hand.
	Source string `toml:"source,omitempty" json:"source,omitempty"`
	// Owner is who the external item is assigned to when that is not you
	// (a teammate's sprint ticket). Empty: yours, or not applicable.
	Owner string `toml:"owner,omitempty" json:"owner,omitempty"`
	// Completed is when YOU marked the workstream done in juggler (zero =
	// not). Independent of the ticket's state: a source never sets or
	// clears it. See Finished.
	Completed time.Time `toml:"completed,omitempty" json:"completed,omitempty"`

	// Dir is the workstream directory (not serialized to TOML).
	Dir string `toml:"-" json:"dir"`
}

// Name is the canonical name: the directory base name.
func (w *Workstream) Name() string { return filepath.Base(w.Dir) }

// Slug returns the short description as a directory-safe slug.
func Slug(desc string) string {
	s := strings.ToLower(strings.TrimSpace(desc))
	s = nonSlug.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	if s == "" {
		s = "untitled"
	}
	return s
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// DirName builds <category>_<id>_<slug>.
func DirName(category, id, desc string) string {
	return fmt.Sprintf("%s_%s_%s", category, id, Slug(desc))
}

// Ref returns the first ref of the given type, or nil.
func (w *Workstream) Ref(typ string) *Ref {
	for i := range w.Refs {
		if w.Refs[i].Type == typ {
			return &w.Refs[i]
		}
	}
	return nil
}

// IdentityRef is the ref that names the external work item this
// workstream is about (a ticket: type jira or issue), or nil.
func (w *Workstream) IdentityRef() *Ref {
	for i := range w.Refs {
		if (w.Refs[i].Type == "jira" || w.Refs[i].Type == "issue") && w.Refs[i].Key != "" {
			return &w.Refs[i]
		}
	}
	return nil
}

// TicketClosed reports whether the external item is finished (as its
// source last saw it).
func (w *Workstream) TicketClosed() bool {
	r := w.IdentityRef()
	return r != nil && r.Closed
}

// Finished is "nothing left to do here": the ticket is closed, or you
// marked the workstream completed. Finished workstreams are hidden from the
// picker (unless they still have windows) and from the web UI's default
// view.
func (w *Workstream) Finished() bool { return !w.Completed.IsZero() || w.TicketClosed() }

// ResolvedCodeDir returns CodeDir expanded, or Dir when unset. A relative
// CodeDir ("src/repo") lives inside the workstream directory.
func (w *Workstream) ResolvedCodeDir() string {
	if w.CodeDir == "" {
		return w.Dir
	}
	p := expand(w.CodeDir)
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Dir, p)
	}
	return p
}

// CodeInside reports whether the code dir is inside the workstream
// directory (and so belongs to it).
func (w *Workstream) CodeInside() bool {
	rel, err := filepath.Rel(w.Dir, w.ResolvedCodeDir())
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// SessionTitle is the title juggler gives the workstream's opencode session.
func (w *Workstream) SessionTitle() string { return w.ID + ": " + w.Desc }

// TodoPath is the workstream's TODO.md.
func (w *Workstream) TodoPath() string { return filepath.Join(w.Dir, "TODO.md") }

// NotesDir is where dictation / free-form notes go.
func (w *Workstream) NotesDir() string { return filepath.Join(w.Dir, "notes") }

// Untouched reports whether the workstream holds nothing of yours yet: no
// code dir, no pinned session, the default TODO.md, an empty notes dir, not
// completed. What a source made and you never opened.
func (w *Workstream) Untouched() bool {
	if w.CodeDir != "" || w.OpencodeSession != "" || !w.Completed.IsZero() {
		return false
	}
	b, err := os.ReadFile(w.TodoPath())
	if err != nil || string(b) != defaultTodo(w) {
		return false
	}
	entries, err := os.ReadDir(w.NotesDir())
	if err != nil {
		return true // no notes dir at all
	}
	return len(entries) == 0
}

// OpenTodos counts unchecked "- [ ]" items in TODO.md.
func (w *Workstream) OpenTodos() int {
	b, err := os.ReadFile(w.TodoPath())
	if err != nil {
		return 0
	}
	return bytes.Count(b, []byte("- [ ]"))
}

func expand(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		p = home + p[1:]
	}
	return os.ExpandEnv(p)
}

// Store is the root directory.
type Store struct {
	Root string
}

// List loads every workstream, sorted by name.
func (s Store) List() ([]*Workstream, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Workstream
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		w, err := s.Load(filepath.Join(s.Root, e.Name()))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // a stray directory without workstream.toml
			}
			return nil, err
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Load reads one workstream directory.
func (s Store) Load(dir string) (*Workstream, error) {
	var w Workstream
	if _, err := toml.DecodeFile(filepath.Join(dir, FileName), &w); err != nil {
		return nil, err
	}
	w.Dir = dir
	return &w, nil
}

// Save writes the metadata file (creating the directory, TODO.md and notes/
// if needed).
func (s Store) Save(w *Workstream) error {
	if w.Dir == "" {
		w.Dir = filepath.Join(s.Root, DirName(w.Category, w.ID, w.Desc))
	}
	if err := os.MkdirAll(w.NotesDir(), 0o755); err != nil {
		return err
	}
	w.Groups = NormalizeGroups(w.Groups)
	var buf bytes.Buffer
	buf.WriteString("# juggler workstream — see `jug help`\n")
	if err := toml.NewEncoder(&buf).Encode(w); err != nil {
		return err
	}
	// temp + rename: a reader (the web server, a sync) never sees a half file
	tmp := filepath.Join(w.Dir, FileName+".tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(w.Dir, FileName)); err != nil {
		return err
	}
	if _, err := os.Stat(w.TodoPath()); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(w.TodoPath(), []byte(defaultTodo(w)), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// DefaultTodo is the TODO.md a new workstream starts with. Untouched means
// "still equal to this".
func DefaultTodo(w *Workstream) string { return defaultTodo(w) }

func defaultTodo(w *Workstream) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", w.ID, w.Desc)
	b.WriteString("Three interfaces have to agree before this is done:\n\n")
	b.WriteString("## Leadership (ticket)\n\n- [ ] ticket reflects the real state\n\n")
	b.WriteString("## Code (PR)\n\n- [ ] PR up\n- [ ] CI green\n- [ ] reviewed\n- [ ] merged\n\n")
	b.WriteString("## Follow-ups (things found on the way)\n\n")
	return b.String()
}

// Resolve finds a workstream by canonical name, id, or unique prefix of
// either (case-insensitive).
func (s Store) Resolve(q string) (*Workstream, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	lq := strings.ToLower(q)
	var exact, prefix []*Workstream
	for _, w := range all {
		name, id := strings.ToLower(w.Name()), strings.ToLower(w.ID)
		switch {
		case name == lq || id == lq:
			exact = append(exact, w)
		case strings.HasPrefix(name, lq) || strings.HasPrefix(id, lq):
			prefix = append(prefix, w)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(exact) > 1:
		return nil, fmt.Errorf("%q matches %d workstreams: %s", q, len(exact), names(exact))
	case len(prefix) == 1:
		return prefix[0], nil
	case len(prefix) > 1:
		return nil, fmt.Errorf("%q is ambiguous: %s", q, names(prefix))
	}
	return nil, fmt.Errorf("no workstream matches %q", q)
}

func names(ws []*Workstream) string {
	var n []string
	for _, w := range ws {
		n = append(n, w.Name())
	}
	return strings.Join(n, ", ")
}

// NextManualID returns the next free "<prefix>-NNNN" id among existing
// workstreams.
func (s Store) NextManualID(prefix string) (string, error) {
	all, err := s.List()
	if err != nil {
		return "", err
	}
	max := 0
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-(\d+)$`)
	for _, w := range all {
		if m := re.FindStringSubmatch(w.ID); m != nil {
			var n int
			fmt.Sscanf(m[1], "%d", &n)
			if n > max {
				max = n
			}
		}
	}
	return fmt.Sprintf("%s-%04d", prefix, max+1), nil
}
