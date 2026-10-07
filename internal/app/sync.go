package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/source"
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/sway"
)

// Sync turns a source's Snapshot into workstreams and groups. The rules,
// which hold for every connector:
//
//   - A snapshot item is matched to a workstream by its identity ref (type +
//     key: the jira ref AISW-123), never by directory name.
//   - No match and the item was Discovered: a workstream is CREATED — id =
//     key, description = the item's summary, the identity ref with title and
//     status, the managed group tags, owner, source. Unless its key is
//     tombstoned in the ledger (the user removed it before: it stays gone),
//     or the item is already FINISHED (closed, done, cancelled): finished
//     work is refreshed where a workstream exists but never gets a new one.
//   - A match that has no source yet is ADOPTED: it gets source = this one.
//     Its description, category and code dir are never touched — those are
//     the user's; the item's current summary lives in the ref title.
//   - On every match the identity ref is refreshed (title, status, updated)
//     and the owner is updated.
//   - Group tags: the source OWNS the names it registers (its sprints, its
//     backlog). On a discovered item, the workstream's tags from that owned
//     set are replaced by the item's; tags from other groups (the user's,
//     other sources') are left alone. Known items the queries no longer
//     select keep their tags (finished work stays in the sprint it finished
//     in).
//   - Managed groups are registered/updated in groups.toml (kind, dates,
//     URL, desc) — a user-made group of the same name is taken over by the
//     source (that is how the hand-rolled sprint becomes the synced one).
//   - Nothing is ever deleted by a sync. Workstreams the source made that it
//     no longer selects (or that finished) and that you never touched are
//     reported as PRUNABLE; `jug source prune` removes those, on request.
//
// Everything a sync writes it also records in the per-source ledger
// (<state>/sync/<source>.json): keys it owns, keys it must not recreate,
// keys it selected last time, the last run. The ledger is what lets the next
// run tell "removed by the user" from "never seen".

// Report is what one sync did (or would do, with DryRun).
type Report struct {
	Source  string    `json:"source"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	DryRun  bool      `json:"dry_run"`
	Notes   []string  `json:"notes,omitempty"`

	GroupsRegistered []string `json:"groups_registered,omitempty"` // new in the registry
	GroupsUpdated    []string `json:"groups_updated,omitempty"`    // metadata changed

	Created   []string `json:"created,omitempty"`    // new workstreams
	Adopted   []string `json:"adopted,omitempty"`    // existing, now sourced
	Updated   []string `json:"updated,omitempty"`    // ref/owner changed
	Regrouped []string `json:"regrouped,omitempty"`  // managed tags changed
	Tombstone []string `json:"tombstoned,omitempty"` // removed by the user, not recreated
	Finished  []string `json:"finished,omitempty"`   // selected but already finished: no workstream made
	Prunable  []string `json:"prunable,omitempty"`   // owned, untouched, no longer selected or finished: `jug source prune`
	Unchanged int      `json:"unchanged"`
	Errors    []string `json:"errors,omitempty"`
}

// Summary is the one-line form.
func (r Report) Summary() string {
	parts := []string{}
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(len(r.Created), "created")
	add(len(r.Adopted), "adopted")
	add(len(r.Updated), "updated")
	add(len(r.Regrouped), "regrouped")
	add(len(r.Tombstone), "tombstoned")
	add(len(r.GroupsRegistered), "groups registered")
	add(len(r.GroupsUpdated), "groups updated")
	add(len(r.Errors), "errors")
	if len(parts) == 0 {
		parts = append(parts, "nothing to do")
	}
	tail := fmt.Sprintf("%d unchanged", r.Unchanged)
	if n := len(r.Finished); n > 0 {
		tail += fmt.Sprintf(", %d finished skipped", n)
	}
	if n := len(r.Prunable); n > 0 {
		tail += fmt.Sprintf(", %d prunable", n)
	}
	s := fmt.Sprintf("%s: %s (%s, %s)", r.Source, strings.Join(parts, ", "), tail, r.Ended.Sub(r.Started).Round(time.Millisecond))
	if r.DryRun {
		s = "[dry run] " + s
	}
	return s
}

// Ledger is the per-source memory on disk.
type Ledger struct {
	Source string `json:"source"`
	// Owned: keys this source created or adopted, with when.
	Owned map[string]time.Time `json:"owned"`
	// Tombstones: keys the user removed; never recreated until forgiven.
	Tombstones map[string]time.Time `json:"tombstones"`
	// LastSelected: keys the source's queries selected on the last pull;
	// what Prune measures "no longer selected" against.
	LastSelected []string  `json:"last_selected,omitempty"`
	LastRun      time.Time `json:"last_run,omitempty"`
	LastOK       time.Time `json:"last_ok,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	LastReport   *Report   `json:"last_report,omitempty"`
}

// ledgerPath is where src's ledger lives: <state_dir>/sync/<src>.json.
func (a *App) ledgerPath(src string) string {
	return filepath.Join(a.Cfg.StateDir, "sync", src+".json")
}

// LoadLedger reads a source's ledger (empty when absent).
func (a *App) LoadLedger(src string) (*Ledger, error) {
	l := &Ledger{Source: src, Owned: map[string]time.Time{}, Tombstones: map[string]time.Time{}}
	b, err := os.ReadFile(a.ledgerPath(src))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, fmt.Errorf("%s: %w", a.ledgerPath(src), err)
	}
	if l.Owned == nil {
		l.Owned = map[string]time.Time{}
	}
	if l.Tombstones == nil {
		l.Tombstones = map[string]time.Time{}
	}
	return l, nil
}

// SaveLedger writes it atomically.
func (a *App) SaveLedger(l *Ledger) error {
	p := a.ledgerPath(l.Source)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// Tombstone marks key as removed-by-the-user for src (never recreated).
func (a *App) Tombstone(src, key string) error {
	l, err := a.LoadLedger(src)
	if err != nil {
		return err
	}
	l.Tombstones[key] = time.Now()
	delete(l.Owned, key)
	return a.SaveLedger(l)
}

// Forgive removes a tombstone so the next sync may recreate key.
func (a *App) Forgive(src, key string) error {
	l, err := a.LoadLedger(src)
	if err != nil {
		return err
	}
	if _, ok := l.Tombstones[key]; !ok {
		return fmt.Errorf("%w: %s has no tombstone for %s", ErrNotFound, src, key)
	}
	delete(l.Tombstones, key)
	return a.SaveLedger(l)
}

// ---------------------------------------------------------------- sources

// Sources opens every configured source. Errors are per source (a broken
// one does not hide the others).
func (a *App) Sources() ([]source.Source, map[string]error) {
	kinds, err := a.Cfg.SourceKinds()
	errs := map[string]error{}
	if err != nil {
		errs[""] = err
		return nil, errs
	}
	env := source.Env{JiraBaseURL: a.Cfg.JiraBaseURL, DefaultCategory: a.Cfg.DefaultCategory, Secret: configSecretFn}
	names := make([]string, 0, len(kinds))
	for n := range kinds {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []source.Source
	for _, n := range names {
		name := n
		src, err := source.Open(kinds[name], name, func(into any) error { return a.Cfg.DecodeSource(name, into) }, env)
		if err != nil {
			errs[name] = err
			continue
		}
		out = append(out, src)
	}
	return out, errs
}

// SourceByName opens one source.
func (a *App) SourceByName(name string) (source.Source, error) {
	srcs, errs := a.Sources()
	for _, s := range srcs {
		if s.Name() == name {
			return s, nil
		}
	}
	if err, ok := errs[name]; ok {
		return nil, err
	}
	if err, ok := errs[""]; ok {
		return nil, err
	}
	return nil, fmt.Errorf("%w: no source %q (configured: %s)", ErrNotFound, name, a.sourceNames())
}

// sourceNames lists the configured sources for error messages ("none"
// when there are none).
func (a *App) sourceNames() string {
	kinds, _ := a.Cfg.SourceKinds()
	var n []string
	for k := range kinds {
		n = append(n, k)
	}
	sort.Strings(n)
	if len(n) == 0 {
		return "none"
	}
	return strings.Join(n, ", ")
}

// SourceStatus is a source plus its ledger, for `jug source ls` and the API.
type SourceStatus struct {
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Describe   string     `json:"describe"`
	Poll       string     `json:"poll"`
	Error      string     `json:"error,omitempty"` // the source cannot run: bad config, or its ledger cannot be read
	LastRun    *time.Time `json:"last_run,omitempty"`
	LastOK     *time.Time `json:"last_ok,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	LastReport *Report    `json:"last_report,omitempty"`
	Owned      int        `json:"owned"`
	Tombstones []string   `json:"tombstones"`
	Running    bool       `json:"running"`
	NextRun    *time.Time `json:"next_run,omitempty"`
}

// SourceStatuses lists every configured source with its ledger state.
func (a *App) SourceStatuses() []SourceStatus {
	srcs, errs := a.Sources()
	var out []SourceStatus
	seen := map[string]bool{}
	add := func(st SourceStatus) {
		// an unreadable ledger stops the sync (it is the source's memory), so say so
		if l, err := a.LoadLedger(st.Name); err != nil && st.Error == "" {
			st.Error = "ledger: " + err.Error()
		} else if err == nil {
			if !l.LastRun.IsZero() {
				t := l.LastRun
				st.LastRun = &t
			}
			if !l.LastOK.IsZero() {
				t := l.LastOK
				st.LastOK = &t
			}
			st.LastError, st.LastReport, st.Owned = l.LastError, l.LastReport, len(l.Owned)
			for k := range l.Tombstones {
				st.Tombstones = append(st.Tombstones, k)
			}
			sort.Strings(st.Tombstones)
		}
		if st.Tombstones == nil {
			st.Tombstones = []string{}
		}
		if a.Scheduler != nil {
			st.Running, st.NextRun = a.Scheduler.state(st.Name)
		}
		out = append(out, st)
		seen[st.Name] = true
	}
	for _, s := range srcs {
		add(SourceStatus{Name: s.Name(), Kind: s.Kind(), Describe: s.Describe(), Poll: shortDuration(s.Poll())})
	}
	kinds, _ := a.Cfg.SourceKinds()
	names := make([]string, 0, len(kinds))
	for n := range kinds {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !seen[n] {
			msg := ""
			if e := errs[n]; e != nil {
				msg = e.Error()
			}
			add(SourceStatus{Name: n, Kind: kinds[n], Error: msg})
		}
	}
	if err := errs[""]; err != nil {
		out = append(out, SourceStatus{Name: "(config)", Error: err.Error(), Tombstones: []string{}})
	}
	return out
}

// ---------------------------------------------------------------- the sync

var syncMu sync.Mutex // one sync at a time, process-wide: they all write groups.toml

// SyncOptions tune one run.
type SyncOptions struct {
	DryRun bool
	// Snapshot, when set, is used instead of pulling (tests, replays).
	Snapshot *source.Snapshot
}

// Sync pulls src and reconciles the result into the store.
func (a *App) Sync(ctx context.Context, src source.Source, opt SyncOptions) (*Report, error) {
	syncMu.Lock()
	defer syncMu.Unlock()
	rep := &Report{Source: src.Name(), Started: time.Now(), DryRun: opt.DryRun}
	ledger, err := a.LoadLedger(src.Name())
	if err != nil {
		return rep, err
	}
	finish := func(err error) (*Report, error) {
		rep.Ended = time.Now()
		if !opt.DryRun {
			ledger.LastRun = rep.Ended
			if err != nil {
				ledger.LastError = err.Error()
			} else {
				ledger.LastError = ""
				ledger.LastOK = rep.Ended
			}
			ledger.LastReport = rep
			if lerr := a.SaveLedger(ledger); lerr != nil && err == nil {
				err = lerr
			}
		}
		return rep, err
	}

	// what the store already attributes to this source
	all, err := a.Store.List()
	if err != nil {
		return finish(err)
	}
	var known []string
	for _, w := range all {
		if w.Source == src.Name() {
			if r := identityRef(w); r != nil {
				known = append(known, r.Key)
			}
		}
	}
	for k := range ledger.Owned {
		known = append(known, k)
	}
	known = uniqueSorted(known)

	snap := opt.Snapshot
	if snap == nil {
		snap, err = src.Pull(ctx, known)
		if err != nil {
			return finish(err)
		}
	}
	rep.Notes = append(rep.Notes, snap.Notes...)

	// 1. managed groups -> registry
	reg, err := a.Store.LoadGroups()
	if err != nil {
		return finish(err)
	}
	owned := map[string]bool{} // group names this source owns
	for _, g := range reg {
		if g.Source == src.Name() {
			owned[g.Name] = true
		}
	}
	regChanged := false
	for _, g := range snap.Groups {
		g.Group.Source = src.Name()
		if err := g.Group.Validate(); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("group %q: %v", g.Name, err))
			continue
		}
		owned[g.Name] = true
		idx := -1
		for i := range reg {
			if reg[i].Name == g.Name {
				idx = i
			}
		}
		switch {
		case idx < 0:
			reg = append(reg, g.Group)
			rep.GroupsRegistered = append(rep.GroupsRegistered, g.Name)
			regChanged = true
		case reg[idx] != g.Group:
			reg[idx] = g.Group
			rep.GroupsUpdated = append(rep.GroupsUpdated, g.Name)
			regChanged = true
		}
	}
	if regChanged && !opt.DryRun {
		if err := a.Store.SaveGroups(reg); err != nil {
			return finish(err)
		}
	}

	// 2. items -> workstreams
	byRef := map[string]*store.Workstream{} // "type\x00key" -> workstream
	byID := map[string]*store.Workstream{}
	for _, w := range all {
		byID[strings.ToLower(w.ID)] = w
		for i := range w.Refs {
			r := w.Refs[i]
			if r.Key != "" {
				byRef[r.Type+"\x00"+r.Key] = w
			}
		}
	}
	var selected []string
	for _, it := range snap.Items {
		if it.Key == "" || it.Ref.Type == "" {
			rep.Errors = append(rep.Errors, fmt.Sprintf("item without key/ref: %+v", it))
			continue
		}
		if it.Discovered && !it.Closed {
			selected = append(selected, it.Key)
		}
		w := byRef[it.Ref.Type+"\x00"+it.Key]
		if w == nil {
			if cand := byID[strings.ToLower(it.Key)]; cand != nil && cand.Source == src.Name() {
				w = cand
			}
		}
		if w == nil {
			if !it.Discovered {
				continue // refreshed-only and gone: nothing to update
			}
			if _, dead := ledger.Tombstones[it.Key]; dead {
				continue
			}
			if it.Closed {
				rep.Finished = append(rep.Finished, it.Key) // finished before we ever saw it: no workstream
				continue
			}
			if _, was := ledger.Owned[it.Key]; was {
				// we made it, it is gone: the user removed it
				rep.Tombstone = append(rep.Tombstone, it.Key)
				if !opt.DryRun {
					ledger.Tombstones[it.Key] = time.Now()
					delete(ledger.Owned, it.Key)
				}
				continue
			}
			if opt.DryRun {
				rep.Created = append(rep.Created, it.Key)
				continue
			}
			nw, err := a.createFromItem(src.Name(), it)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", it.Key, err))
				continue
			}
			ledger.Owned[it.Key] = time.Now()
			byRef[it.Ref.Type+"\x00"+it.Key] = nw
			rep.Created = append(rep.Created, it.Key)
			continue
		}
		// existing: adopt / refresh ref / owner / managed tags
		changed, regrouped, adopted := false, false, false
		if w.Source == "" {
			w.Source = src.Name()
			adopted, changed = true, true
		} else if w.Source != src.Name() {
			continue // another source's; not ours to touch
		}
		if r := refOf(w, it.Ref.Type, it.Key); r != nil {
			if r.Title != it.Ref.Title || r.Status != it.Ref.Status || r.URL != it.Ref.URL || r.Closed != it.Closed {
				r.Title, r.Status, r.URL, r.Closed, r.Updated = it.Ref.Title, it.Ref.Status, it.Ref.URL, it.Closed, time.Now()
				changed = true
			}
		} else {
			nr := it.Ref
			nr.Closed, nr.Updated = it.Closed, time.Now()
			w.Refs = append(w.Refs, nr)
			changed = true
		}
		if w.Owner != it.Owner {
			w.Owner = it.Owner
			changed = true
		}
		if it.Discovered {
			var kept []string
			for _, g := range w.Groups {
				if !owned[g] {
					kept = append(kept, g)
				}
			}
			next := store.NormalizeGroups(append(kept, it.Groups...))
			if strings.Join(next, "\x00") != strings.Join(w.Groups, "\x00") {
				w.Groups = next
				regrouped = true
			}
		}
		switch { // one bucket per key: adopted > regrouped > updated > unchanged
		case adopted:
			rep.Adopted = append(rep.Adopted, it.Key)
		case regrouped:
			rep.Regrouped = append(rep.Regrouped, it.Key)
		case changed:
			rep.Updated = append(rep.Updated, it.Key)
		default:
			rep.Unchanged++
		}
		if (changed || regrouped) && !opt.DryRun {
			if err := a.Store.Save(w); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", it.Key, err))
			}
			if _, ok := ledger.Owned[it.Key]; !ok {
				ledger.Owned[it.Key] = time.Now()
			}
		}
	}
	if !opt.DryRun {
		ledger.LastSelected = uniqueSorted(selected)
	}
	rep.Prunable = a.prunable(src.Name(), all, uniqueSorted(selected))
	for _, l := range [][]string{rep.Created, rep.Adopted, rep.Updated, rep.Regrouped, rep.Tombstone, rep.Finished, rep.GroupsRegistered, rep.GroupsUpdated} {
		sort.Strings(l)
	}
	return finish(nil)
}

// prunable lists the keys of workstreams this source owns that it no longer
// selects (or that finished) and that hold nothing of the user's.
func (a *App) prunable(src string, all []*store.Workstream, selected []string) []string {
	sel := map[string]bool{}
	for _, k := range selected {
		sel[k] = true
	}
	var out []string
	for _, w := range all {
		if w.Source != src {
			continue
		}
		r := w.IdentityRef()
		if r == nil {
			continue
		}
		if (!sel[r.Key] || w.Finished()) && w.Untouched() {
			out = append(out, r.Key)
		}
	}
	sort.Strings(out)
	return out
}

// Prune removes the workstreams Report.Prunable names: owned by src, no
// longer selected by it (or finished), and untouched — a source's leftovers.
// Windows, if any, are closed first (e may be nil when sway is away; a live
// workstream is then skipped). Pruned keys leave the ledger's Owned set
// without a tombstone: should the source select one again, it is simply
// recreated. Returns the keys removed (or, dryRun, that would be).
func (a *App) Prune(e *layout.Engine, src string, dryRun bool) ([]string, []string, error) {
	syncMu.Lock()
	defer syncMu.Unlock()
	ledger, err := a.LoadLedger(src)
	if err != nil {
		return nil, nil, err
	}
	all, err := a.Store.List()
	if err != nil {
		return nil, nil, err
	}
	var tree *sway.Node
	if e != nil {
		tree, _ = e.Sway.GetTree()
	}
	var removed, skipped []string
	for _, key := range a.prunable(src, all, ledger.LastSelected) {
		var w *store.Workstream
		for _, c := range all {
			if c.Source == src {
				if r := c.IdentityRef(); r != nil && r.Key == key {
					w = c
				}
			}
		}
		if w == nil {
			continue
		}
		if tree != nil {
			if live, _ := e.IsLive(tree, w); live {
				skipped = append(skipped, key+" (has windows)")
				continue
			}
		} else if e == nil && a.Cfg.StateDir != "" {
			// no compositor to ask: a workstream with recorded windows is left alone
			if st, err := store.LoadState(a.Cfg.StateDir); err == nil && st.Streams[w.Name()] != nil && len(st.Streams[w.Name()].Windows) > 0 {
				skipped = append(skipped, key+" (may have windows; sway unavailable)")
				continue
			}
		}
		if dryRun {
			removed = append(removed, key)
			continue
		}
		if err := a.removeFiles(w, false); err != nil {
			skipped = append(skipped, key+" ("+err.Error()+")")
			continue
		}
		delete(ledger.Owned, key)
		removed = append(removed, key)
	}
	if !dryRun && len(removed) > 0 {
		if ledger.LastReport != nil { // the last report's hint is now stale; keep what is still there
			gone := map[string]bool{}
			for _, k := range removed {
				gone[k] = true
			}
			var left []string
			for _, k := range ledger.LastReport.Prunable {
				if !gone[k] {
					left = append(left, k)
				}
			}
			ledger.LastReport.Prunable = left
		}
		if err := a.SaveLedger(ledger); err != nil {
			return removed, skipped, err
		}
	}
	return removed, skipped, nil
}

// createFromItem makes the workstream for a discovered item.
func (a *App) createFromItem(src string, it source.Item) (*store.Workstream, error) {
	category := it.Category
	if category == "" {
		category = a.CategoryFor("", it.Key)
	}
	desc := strings.TrimSpace(it.Desc)
	if desc == "" {
		desc = it.Key
	}
	w, err := a.Create(CreateOptions{Category: category, ID: it.Key, Desc: desc, Groups: it.Groups})
	if err != nil {
		return nil, err
	}
	r := it.Ref
	r.Closed, r.Updated = it.Closed, time.Now()
	w.Refs = []store.Ref{r}
	w.Source, w.Owner = src, it.Owner
	return w, a.Store.Save(w)
}

// identityRef is the ref a sourced workstream is matched by.
func identityRef(w *store.Workstream) *store.Ref { return w.IdentityRef() }

// refOf finds w's ref of the given type and key (nil when absent).
func refOf(w *store.Workstream, typ, key string) *store.Ref {
	for i := range w.Refs {
		if w.Refs[i].Type == typ && w.Refs[i].Key == key {
			return &w.Refs[i]
		}
	}
	return nil
}

// shortDuration renders a poll interval for people: 5m, 1h, "manual" for 0.
func shortDuration(d time.Duration) string {
	if d == 0 {
		return "manual"
	}
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	s = strings.TrimSuffix(s, "0m")
	return s
}

// uniqueSorted drops empties and duplicates and sorts, for stable reports
// and ledgers.
func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// configSecretFn resolves credential references (config.Secret); a
// variable so tests can reach it without importing config twice.
var configSecretFn = config.Secret

// SyncAll runs every configured source; a failing source is reported, the
// rest still run.
func (a *App) SyncAll(ctx context.Context, opt SyncOptions) ([]*Report, error) {
	srcs, errs := a.Sources()
	var reps []*Report
	var problems []string
	for n, e := range errs {
		problems = append(problems, fmt.Sprintf("%s: %v", n, e))
	}
	for _, s := range srcs {
		rep, err := a.Sync(ctx, s, opt)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", s.Name(), err))
		}
		if rep != nil {
			reps = append(reps, rep)
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return reps, errors.New(strings.Join(problems, "; "))
	}
	return reps, nil
}
