// Package source defines where workstreams come from.
//
// Two nouns, deliberately separate:
//
//   - A Connector is a KIND of source — the code that knows how to talk to
//     one system (Jira, GitHub issues, a calendar…). It is registered once,
//     by kind name, and knows how to read its own configuration.
//   - A Source is a configured INSTANCE of a connector — "my team's Jira
//     board", "my personal GitHub issues" — named by the user in config
//     (`[sources.<name>]`). One connector can back many sources.
//
// A source does one thing: Pull. Given an Inventory of what the store
// already has (the keys it knows from this source, and a view of every
// workstream), it returns a Snapshot — the groups it manages (a sprint,
// with dates and the link to its board), the items it found (tickets, with
// their identity ref, state and group membership), and ref updates for
// workstreams that already exist (a pull request's state, matched to the
// workstream on that branch). The source never touches the store; turning
// a snapshot into workstreams is the sync's job (internal/app, Sync), which
// also owns the rules about what may be overwritten. That split keeps
// connectors small and testable with a fake server, and the reconciliation
// rules in one place for every connector.
//
// Two kinds of content, deliberately distinct: an Item is an IDENTITY — a
// ticket — and may become a workstream; a RefUpdate is a FACT ABOUT an
// existing workstream — its PR is merged — and never creates one. A ticket
// can have several PRs (a fix and its backport), so a PR is not an
// identity.
package source

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/varlogtim/juggler/internal/store"
)

// Connector is a kind of source.
type Connector interface {
	// Kind is the name used in config: kind = "jira".
	Kind() string
	// Open builds a Source named name from its config table. decode fills a
	// connector-specific struct from that table (the connector owns its
	// schema); env carries the shared defaults a connector may fall back on.
	Open(name string, decode func(into any) error, env Env) (Source, error)
}

// Env is what every connector gets from the global configuration.
type Env struct {
	JiraBaseURL     string
	DefaultCategory string
	// Secret resolves a credential reference ("file:~/x/token", or a bare
	// value) to its value.
	Secret func(ref string) (string, error)
}

// Source is a configured connector instance.
type Source interface {
	// Name is the config name: [sources.<name>].
	Name() string
	// Kind is the connector kind.
	Kind() string
	// Poll is how often the scheduler pulls; 0 disables scheduling.
	Poll() time.Duration
	// Describe is one line for `jug source ls`: what this source watches.
	Describe() string
	// Pull fetches the current picture. inv.Known lists the identity keys
	// the store already attributes to this source; the source should
	// include their current state even when they no longer match its
	// queries, so finished work keeps its final status. Items for known
	// keys that the queries no longer select are returned with
	// Discovered=false. inv.Workstreams is for connectors that annotate
	// existing workstreams (pull requests for their branches).
	Pull(ctx context.Context, inv Inventory) (*Snapshot, error)
}

// Inventory is what the store already has, as a source may need it.
type Inventory struct {
	// Known: identity keys the store attributes to this source (its items'
	// keys, and keys in its ledger).
	Known []string
	// Workstreams: every workstream, read-only, as much as a connector needs
	// to match external facts to it.
	Workstreams []WorkstreamView
}

// WorkstreamView is a workstream as a connector sees it.
type WorkstreamView struct {
	Name string      `json:"name"`
	ID   string      `json:"id"`
	Refs []store.Ref `json:"refs"`
	// Branch, Remote and DefaultBranch describe its checkout: the branch
	// checked out, the URL of the main checkout's remote, and that remote's
	// default branch — all "" when it has no code, or none in git. A
	// workstream on the default branch (a shared main checkout) has no
	// branch of its own for a connector to look up.
	Branch        string `json:"branch,omitempty"`
	Remote        string `json:"remote,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	// Live: it has windows or a session — work that is being looked at.
	Live bool `json:"live"`
}

// Ref returns the view's ref of the given type with key, or nil.
func (v WorkstreamView) Ref(typ, key string) *store.Ref {
	for i := range v.Refs {
		if v.Refs[i].Type == typ && v.Refs[i].Key == key {
			return &v.Refs[i]
		}
	}
	return nil
}

// Snapshot is one pull's result.
type Snapshot struct {
	Source string    `json:"source"`
	Taken  time.Time `json:"taken"`
	// Groups the source manages: metadata (dates, link) for the buckets its
	// items are tagged into. The sync registers them and treats their names
	// as owned by this source when (re)tagging.
	Groups []Group `json:"groups"`
	Items  []Item  `json:"items"`
	// Refs are facts about existing workstreams: a ref to set or refresh
	// on a named workstream. The sync replaces a ref of the same type and
	// key (or URL) and never creates a workstream for one.
	Refs []RefUpdate `json:"refs,omitempty"`
	// Notes are human-readable remarks about the pull (what was queried,
	// what was skipped) for `jug sync` output and the log.
	Notes []string `json:"notes,omitempty"`
}

// RefUpdate sets or refreshes one ref on an existing workstream.
type RefUpdate struct {
	// Workstream is the canonical name (WorkstreamView.Name) to apply to.
	Workstream string `json:"workstream"`
	// Ref carries type, key, URL, title and status; the sync stamps Updated.
	Ref store.Ref `json:"ref"`
	// Closed: the external thing is finished (a PR merged or closed).
	Closed bool `json:"closed"`
}

// Group is a managed group with its metadata.
type Group struct {
	store.Group
	// State is the source's own word for where the group is in its life
	// (sprint: future | active | closed). Informational.
	State string `json:"state,omitempty"`
}

// Item is one external work item as a proposed workstream.
type Item struct {
	// Key is the external identity (AISW-123); it becomes the workstream id
	// when the sync creates one, and is how existing workstreams are matched
	// (by their ref of type Ref.Type with this key).
	Key string `json:"key"`
	// Ref is the identity ref the workstream carries for this item, with
	// the item's current title and status filled in. (Its Closed flag is
	// set by the sync from Closed below; connectors need not fill it.)
	Ref store.Ref `json:"ref"`
	// Desc is the description used when a workstream is created (the item's
	// summary). Never applied to an existing workstream: its description —
	// and so its directory name — is the user's.
	Desc     string `json:"desc"`
	Category string `json:"category"`
	// Owner is who the item is assigned to when that is not you; "" = you.
	Owner string `json:"owner,omitempty"`
	// Groups are the managed group names this item belongs to right now.
	Groups []string `json:"groups"`
	// Closed: the external item is finished (resolved, done, cancelled).
	Closed bool `json:"closed"`
	// Discovered: the item matched the source's queries, so a workstream may
	// be created for it. False for known keys that were only refreshed.
	Discovered bool `json:"discovered"`
}

// ---------------------------------------------------------------- registry

// connectors is the process-wide registry, kind -> connector, filled by
// Register from each connector package's init.
var (
	regMu      sync.RWMutex
	connectors = map[string]Connector{}
)

// Register makes a connector available by kind. Connectors register
// themselves in init(); importing the package is enough.
func Register(c Connector) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := connectors[c.Kind()]; dup {
		panic("source: connector kind registered twice: " + c.Kind())
	}
	connectors[c.Kind()] = c
}

// Kinds lists the registered connector kinds.
func Kinds() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(connectors))
	for k := range connectors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrUnknownKind is returned for a kind no connector claims.
var ErrUnknownKind = errors.New("unknown source kind")

// Open builds a source of the given kind.
func Open(kind, name string, decode func(into any) error, env Env) (Source, error) {
	regMu.RLock()
	c, ok := connectors[kind]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q for source %q (have: %v)", ErrUnknownKind, kind, name, Kinds())
	}
	return c.Open(name, decode, env)
}
