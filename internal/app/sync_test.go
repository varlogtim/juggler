package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/source"
	_ "github.com/varlogtim/juggler/internal/source/jira" // registers the kind for the config-driven test
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/sway"
)

// fakeSource returns a canned snapshot and records what it was asked.
type fakeSource struct {
	name  string
	snap  *source.Snapshot
	known []string
	inv   source.Inventory
	err   error
}

func (f *fakeSource) Name() string        { return f.name }
func (f *fakeSource) Kind() string        { return "fake" }
func (f *fakeSource) Poll() time.Duration { return 0 }
func (f *fakeSource) Describe() string    { return "fake" }
func (f *fakeSource) Pull(_ context.Context, inv source.Inventory) (*source.Snapshot, error) {
	f.known = inv.Known
	f.inv = inv
	return f.snap, f.err
}

func jref(key, title, status string) store.Ref {
	return store.Ref{Type: "jira", Key: key, URL: "https://jira.example.com/browse/" + key, Title: title, Status: status}
}

func item(key, desc, status string, groups []string, owner string, discovered bool) source.Item {
	return source.Item{Key: key, Ref: jref(key, desc, status), Desc: desc, Category: "work", Owner: owner, Groups: groups, Discovered: discovered, Closed: status == "Closed"}
}

func sprintGroup(name, start, end string) source.Group {
	return source.Group{Group: store.Group{Name: name, Kind: "sprint", Start: start, End: end, URL: "https://jira.example.com/boards/1?sprint=" + name}, State: "active"}
}

func TestSyncCreatesAdoptsRefreshesRegroups(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	// a hand-made workstream with a jira ref, tagged by hand into a user group
	// and into the (hand-rolled) sprint group
	hand, _ := a.Create(CreateOptions{Desc: "my hand made one", Jira: "T-1", Groups: []string{"proj-x", "S20"}})
	if _, err := a.SetGroup("S20", GroupPatch{Kind: str("sprint"), Desc: str("hand rolled")}); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{name: "nebula", snap: &source.Snapshot{
		Source: "nebula",
		Groups: []source.Group{sprintGroup("S20", "2026-09-23", "2026-10-06"), {Group: store.Group{Name: "backlog", Kind: "bucket"}}},
		Items: []source.Item{
			item("T-1", "T-1 summary from jira", "In Progress", []string{"S20"}, "", true),
			item("T-2", "a new ticket", "New", []string{"backlog"}, "", true),
			item("T-3", "teammate's ticket", "Code Review", []string{"S20"}, "Pat Teammate", true),
		},
		Notes: []string{"hello from the fake"},
	}}

	// dry run: reports, writes nothing
	rep, err := a.Sync(ctx, src, SyncOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || strings.Join(rep.Created, ",") != "T-2,T-3" || strings.Join(rep.Adopted, ",") != "T-1" || len(rep.GroupsRegistered) != 1 || len(rep.GroupsUpdated) != 1 {
		t.Fatalf("dry run report: %+v", rep)
	}
	if all, _ := a.Store.List(); len(all) != 1 {
		t.Fatal("dry run wrote workstreams")
	}
	if l, _ := a.LoadLedger("nebula"); !l.LastRun.IsZero() || len(l.Owned) != 0 {
		t.Fatalf("dry run touched the ledger: %+v", l)
	}

	// real run
	rep, err = a.Sync(ctx, src, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Created, ",") != "T-2,T-3" || strings.Join(rep.Adopted, ",") != "T-1" || len(rep.Updated) != 0 || len(rep.Errors) != 0 || rep.Summary() == "" {
		t.Fatalf("report: %+v", rep)
	}
	if !strings.Contains(strings.Join(rep.Notes, " "), "hello from the fake") {
		t.Fatalf("notes not carried: %v", rep.Notes)
	}
	// known keys passed to the source: none yet on the first pull (nothing was sourced)
	if len(src.known) != 0 {
		t.Fatalf("known on first pull: %v", src.known)
	}
	// adopted: source set, desc untouched, ref refreshed, user tag kept, sprint tag kept
	w1, _ := a.Resolve("T-1")
	if w1.Source != "nebula" || w1.Desc != "my hand made one" || w1.Ref("jira").Status != "In Progress" || w1.Ref("jira").Title != "T-1 summary from jira" {
		t.Fatalf("adopted: %+v %+v", w1, w1.Refs)
	}
	if strings.Join(w1.Groups, ",") != "proj-x,S20" {
		t.Fatalf("adopted groups: %v", w1.Groups)
	}
	// created: id = key, desc = summary, category, owner, groups, source, ref
	w3, _ := a.Resolve("T-3")
	if w3.Name() != "work_T-3_teammate-s-ticket" || w3.Owner != "Pat Teammate" || w3.Source != "nebula" || strings.Join(w3.Groups, ",") != "S20" || w3.Ref("jira").Status != "Code Review" {
		t.Fatalf("created: %+v", w3)
	}
	if _, err := os.Stat(filepath.Join(w3.Dir, "TODO.md")); err != nil {
		t.Fatal("created workstream has no TODO.md")
	}
	// registry: S20 taken over (source, dates, url), backlog registered, hand desc replaced by the source's
	gs, _ := a.Store.LoadGroups()
	var s20 *store.Group
	for i := range gs {
		if gs[i].Name == "S20" {
			s20 = &gs[i]
		}
	}
	if s20 == nil || s20.Source != "nebula" || s20.Start != "2026-09-23" || s20.URL == "" {
		t.Fatalf("S20 registry: %+v", s20)
	}
	l, _ := a.LoadLedger("nebula")
	if len(l.Owned) != 3 || l.LastError != "" || l.LastOK.IsZero() || l.LastReport == nil {
		t.Fatalf("ledger: %+v", l)
	}

	// second pull: T-1 moved to S21 (new sprint), T-2 unchanged, T-3 no longer
	// discovered (refresh only) but closed now
	src.snap = &source.Snapshot{Source: "nebula",
		Groups: []source.Group{sprintGroup("S20", "2026-09-23", "2026-10-06"), sprintGroup("S21", "2026-10-07", "2026-10-20"), {Group: store.Group{Name: "backlog", Kind: "bucket"}}},
		Items: []source.Item{
			item("T-1", "T-1 summary from jira", "In Progress", []string{"S21"}, "", true),
			item("T-2", "a new ticket", "New", []string{"backlog"}, "", true),
			item("T-3", "teammate's ticket", "Closed", nil, "Pat Teammate", false),
		}}
	rep, err = a.Sync(ctx, src, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(src.known, ",") != "T-1,T-2,T-3" {
		t.Fatalf("known on second pull: %v", src.known)
	}
	if strings.Join(rep.Regrouped, ",") != "T-1" || strings.Join(rep.Updated, ",") != "T-3" || rep.Unchanged != 1 || strings.Join(rep.GroupsRegistered, ",") != "S21" {
		t.Fatalf("second report: %+v", rep)
	}
	w1, _ = a.Resolve("T-1")
	if strings.Join(w1.Groups, ",") != "proj-x,S21" { // owned S20 replaced by S21, user's proj-x kept
		t.Fatalf("regrouped: %v", w1.Groups)
	}
	w3, _ = a.Resolve("T-3")
	if w3.Ref("jira").Status != "Closed" || strings.Join(w3.Groups, ",") != "S20" { // refresh-only keeps its sprint
		t.Fatalf("refreshed: %+v %v", w3.Refs, w3.Groups)
	}

	// user removes T-2: next sync tombstones it instead of recreating
	w2, _ := a.Resolve("T-2")
	if err := a.Remove(nil, w2, false); err != nil {
		t.Fatal(err)
	}
	l, _ = a.LoadLedger("nebula")
	if _, dead := l.Tombstones["T-2"]; !dead {
		t.Fatalf("Remove did not tombstone: %+v", l.Tombstones)
	}
	rep, _ = a.Sync(ctx, src, SyncOptions{})
	if len(rep.Created) != 0 {
		t.Fatalf("tombstoned key recreated: %+v", rep)
	}
	if _, err := a.Resolve("T-2"); err == nil {
		t.Fatal("T-2 came back")
	}
	// rm -rf behind our back: the ledger still knows we owned it
	w1, _ = a.Resolve("T-1")
	os.RemoveAll(w1.Dir)
	rep, _ = a.Sync(ctx, src, SyncOptions{})
	if strings.Join(rep.Tombstone, ",") != "T-1" || len(rep.Created) != 0 {
		t.Fatalf("inferred tombstone: %+v", rep)
	}
	// forgive: comes back on the next run
	if err := a.Forgive("nebula", "T-2"); err != nil {
		t.Fatal(err)
	}
	if err := a.Forgive("nebula", "T-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double forgive: %v", err)
	}
	rep, _ = a.Sync(ctx, src, SyncOptions{})
	if strings.Join(rep.Created, ",") != "T-2" {
		t.Fatalf("forgiven not recreated: %+v", rep)
	}
	_ = hand
}

func TestSyncErrorsAndOtherSources(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	// a workstream owned by another source is left alone
	other, _ := a.Create(CreateOptions{Desc: "theirs", Jira: "T-9"})
	other.Source = "elsewhere"
	a.Store.Save(other)
	src := &fakeSource{name: "nebula", snap: &source.Snapshot{Source: "nebula", Items: []source.Item{
		item("T-9", "theirs renamed", "Done", []string{"S20"}, "", true),
		{Key: "", Ref: store.Ref{}, Discovered: true}, // bad item
	}, Groups: []source.Group{{Group: store.Group{Name: "bad/name"}}}}}
	rep, err := a.Sync(ctx, src, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Errors) != 2 || len(rep.Created) != 0 || len(rep.Updated) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	w, _ := a.Resolve("T-9")
	if w.Source != "elsewhere" || w.Ref("jira").Status != "" {
		t.Fatalf("touched another source's workstream: %+v", w)
	}
	// a pull failure is recorded in the ledger
	src.err = errors.New("jira is down")
	if _, err := a.Sync(ctx, src, SyncOptions{}); err == nil {
		t.Fatal("pull error swallowed")
	}
	l, _ := a.LoadLedger("nebula")
	if l.LastError != "jira is down" || l.LastOK.IsZero() {
		t.Fatalf("ledger after error: %+v", l)
	}
	st := a.SourceStatuses()
	if len(st) != 0 { // none configured in the test config
		t.Fatalf("statuses: %+v", st)
	}
}

func TestSecretAndSourceKinds(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	os.WriteFile(tok, []byte("s3cret\nsecond line\n"), 0o600)
	a := newApp(t)
	_ = a
	if v, err := configSecret("file:" + tok); err != nil || v != "s3cret" {
		t.Fatalf("file secret: %q %v", v, err)
	}
	if _, err := configSecret("file:" + filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
	t.Setenv("JUG_TEST_TOKEN", "fromenv")
	if v, _ := configSecret("env:JUG_TEST_TOKEN"); v != "fromenv" {
		t.Fatal(v)
	}
	if v, _ := configSecret("plain"); v != "plain" {
		t.Fatal(v)
	}
}

func configSecret(ref string) (string, error) { return configSecretFn(ref) }

// Ref updates annotate existing workstreams and never create one; the
// inventory a connector gets describes every workstream.
func TestSyncRefUpdates(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	w, _ := a.Create(CreateOptions{Desc: "fix", Jira: "T-1"})
	a.AddRef(w, "pr", "https://gh.example.com/o/r/pull/7", "old title", "open")
	w2, _ := a.Create(CreateOptions{Desc: "notes only"})
	pr := func(ws, key, url, title, status string, closed bool) source.RefUpdate {
		return source.RefUpdate{Workstream: ws, Ref: store.Ref{Type: "pr", Key: key, URL: url, Title: title, Status: status}, Closed: closed}
	}
	src := &fakeSource{name: "gh", snap: &source.Snapshot{Source: "gh", Refs: []source.RefUpdate{
		pr(w.Name(), "o/r#7", "https://gh.example.com/o/r/pull/7", "fix the thing", "merged", true), // replace by key
		pr(w2.Name(), "o/r#8", "https://gh.example.com/o/r/pull/8", "new one", "draft", false),      // append
		pr("nope", "o/r#9", "https://gh.example.com/o/r/pull/9", "", "open", false),                 // unknown workstream
	}}}
	rep, err := a.Sync(ctx, src, SyncOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Refs, ",") != w.ID+" o/r#7,"+w2.ID+" o/r#8" || len(rep.Errors) != 1 || len(rep.Created) != 0 {
		t.Fatalf("dry run: %+v", rep)
	}
	if got, _ := a.Resolve("T-1"); got.Ref("pr").Status != "open" {
		t.Fatal("dry run wrote")
	}
	// the inventory carried every workstream with its refs
	if len(src.inv.Workstreams) != 2 || src.inv.Workstreams[0].Ref("pr", "o/r#7") == nil && src.inv.Workstreams[1].Ref("pr", "o/r#7") == nil {
		t.Fatalf("inventory: %+v", src.inv.Workstreams)
	}
	rep, _ = a.Sync(ctx, src, SyncOptions{})
	if len(rep.Refs) != 2 || !strings.Contains(rep.Summary(), "2 refs refreshed") {
		t.Fatalf("real run: %+v", rep)
	}
	w, _ = a.Resolve("T-1")
	r := w.Ref("pr")
	if r.Title != "fix the thing" || r.Status != "merged" || !r.Closed || r.Updated.IsZero() || len(w.Refs) != 2 {
		t.Fatalf("replaced ref: %+v", w.Refs)
	}
	if w.Source != "" || w.Finished() { // a ref source does not own the workstream, and a merged PR is not "finished"
		t.Fatalf("source=%q finished=%v", w.Source, w.Finished())
	}
	w2, _ = a.Load(w2.Name())
	if r := w2.Ref("pr"); r == nil || r.Key != "o/r#8" || r.Status != "draft" {
		t.Fatalf("appended ref: %+v", w2.Refs)
	}
	// same facts again: nothing to do
	rep, _ = a.Sync(ctx, src, SyncOptions{})
	if len(rep.Refs) != 0 || rep.Unchanged != 2 {
		t.Fatalf("idempotent: %+v", rep)
	}
	// a ref source never creates workstreams and never owns any
	l, _ := a.LoadLedger("gh")
	if len(l.Owned) != 0 {
		t.Fatalf("ledger owned: %v", l.Owned)
	}
}

// An unreadable ledger must stop the sync (it is the source's memory of what
// it owns) and be visible in the source's status, not read as "owns nothing".
func TestUnreadableLedgerIsAnError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	os.WriteFile(cfgPath, []byte(`
root = "`+filepath.Join(dir, "ws")+`"
state_dir = "`+filepath.Join(dir, "st")+`"
jira_base_url = "https://jira.example.com"
[sources.nebula]
kind = "jira"
email = "me@example.com"
token = "t"
board = 1
`), 0o600)
	t.Setenv("JUGGLER_CONFIG", cfgPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a := New(cfg)
	a.DialFunc = func() (*sway.Conn, error) { return nil, errors.New("no sway") }
	os.MkdirAll(filepath.Dir(a.ledgerPath("nebula")), 0o755)
	os.WriteFile(a.ledgerPath("nebula"), []byte(`{"source":"nebula","owned":"not a map"}`), 0o600)

	sts := a.SourceStatuses()
	if len(sts) != 1 || !strings.HasPrefix(sts[0].Error, "ledger: ") || sts[0].Owned != 0 {
		t.Fatalf("status: %+v", sts)
	}
	src, err := a.SourceByName("nebula")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Sync(context.Background(), src, SyncOptions{Snapshot: &source.Snapshot{Source: "nebula"}}); err == nil || !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("sync ran on an unreadable ledger: %v", err)
	}
	// the file is left alone for the user to fix
	if b, _ := os.ReadFile(a.ledgerPath("nebula")); !strings.Contains(string(b), "not a map") {
		t.Fatal("ledger rewritten")
	}
}

func TestSyncFinishedNeverCreatesAndPrune(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	src := &fakeSource{name: "nebula", snap: &source.Snapshot{Source: "nebula",
		Groups: []source.Group{sprintGroup("S20", "2026-09-23", "2026-10-06")},
		Items: []source.Item{
			item("T-1", "open and mine", "In Progress", []string{"S20"}, "", true),
			item("T-2", "already closed", "Closed", []string{"S20"}, "", true), // finished before we saw it
			item("T-3", "teammate's", "New", []string{"S20"}, "Pat", true),
		}}}
	rep, err := a.Sync(ctx, src, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Created, ",") != "T-1,T-3" || strings.Join(rep.Finished, ",") != "T-2" || len(rep.Prunable) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if _, err := a.Resolve("T-2"); err == nil {
		t.Fatal("finished item got a workstream")
	}
	if !strings.Contains(rep.Summary(), "1 finished skipped") {
		t.Fatalf("summary: %s", rep.Summary())
	}
	// T-1 gets touched (a note); T-3 stays untouched
	w1, _ := a.Resolve("T-1")
	os.WriteFile(filepath.Join(w1.NotesDir(), "n.md"), []byte("x"), 0o644)
	// config flipped: the source now selects only T-1 (T-3 vanishes from the queries),
	// and T-1 later finishes in Jira
	src.snap = &source.Snapshot{Source: "nebula", Groups: []source.Group{sprintGroup("S20", "2026-09-23", "2026-10-06")},
		Items: []source.Item{item("T-1", "open and mine", "Closed", []string{"S20"}, "", true)}}
	rep, _ = a.Sync(ctx, src, SyncOptions{})
	// T-3: untouched and no longer selected -> prunable; T-1: finished but touched -> kept
	if strings.Join(rep.Prunable, ",") != "T-3" || !strings.Contains(rep.Summary(), "1 prunable") {
		t.Fatalf("prunable: %+v", rep)
	}
	l, _ := a.LoadLedger("nebula")
	if len(l.LastSelected) != 0 { // T-1 is closed now, so it is not "selected" for prune purposes
		t.Fatalf("last selected: %v", l.LastSelected)
	}
	// dry prune lists, real prune removes, ledger forgets ownership (no tombstone)
	would, skipped, err := a.Prune(nil, "nebula", true)
	if err != nil || strings.Join(would, ",") != "T-3" || len(skipped) != 0 {
		t.Fatalf("dry prune: %v %v %v", would, skipped, err)
	}
	if _, err := a.Resolve("T-3"); err != nil {
		t.Fatal("dry prune removed something")
	}
	removed, _, err := a.Prune(nil, "nebula", false)
	if err != nil || strings.Join(removed, ",") != "T-3" {
		t.Fatalf("prune: %v %v", removed, err)
	}
	if _, err := a.Resolve("T-3"); err == nil {
		t.Fatal("pruned workstream still there")
	}
	l, _ = a.LoadLedger("nebula")
	if _, owned := l.Owned["T-3"]; owned {
		t.Fatal("pruned key still owned")
	}
	if _, dead := l.Tombstones["T-3"]; dead {
		t.Fatal("prune must not tombstone")
	}
	if l.LastReport == nil || len(l.LastReport.Prunable) != 0 {
		t.Fatalf("last report still hints at the pruned key: %+v", l.LastReport)
	}
	// selected again later -> recreated (not tombstoned)
	src.snap.Items = append(src.snap.Items, item("T-3", "teammate's", "New", []string{"S20"}, "Pat", true))
	rep, _ = a.Sync(ctx, src, SyncOptions{})
	if strings.Join(rep.Created, ",") != "T-3" {
		t.Fatalf("pruned then reselected: %+v", rep)
	}
	// a finished-by-ticket workstream the user touched is never prunable; complete/reopen
	w1, _ = a.Resolve("T-1")
	if !w1.Finished() || !w1.TicketClosed() || w1.Untouched() {
		t.Fatalf("T-1: finished=%v closed=%v untouched=%v", w1.Finished(), w1.TicketClosed(), w1.Untouched())
	}
	w3, _ := a.Resolve("T-3")
	if err := a.Complete(w3); err != nil {
		t.Fatal(err)
	}
	in := a.InfoOf(w3, nil, nil)
	if !in.Finished || in.TicketClosed || in.Completed == "" {
		t.Fatalf("completed info: %+v", in)
	}
	rep, _ = a.Sync(ctx, src, SyncOptions{}) // the source must not clear it
	w3, _ = a.Resolve("T-3")
	if w3.Completed.IsZero() || !w3.Finished() {
		t.Fatal("sync cleared the completion")
	}
	if len(rep.Prunable) != 0 { // completed = touched: not prunable
		t.Fatalf("completed is prunable: %v", rep.Prunable)
	}
	if err := a.Reopen(w3); err != nil || w3.Finished() {
		t.Fatalf("reopen: %v %v", err, w3.Finished())
	}
}
