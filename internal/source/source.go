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
// A source does one thing: Pull. Given the keys the store already knows
// from it, it returns a Snapshot — the groups it manages (a sprint, with
// dates and the link to its board) and the items it found (tickets, with
// their identity ref, state and group membership). The source never touches
// the store; turning a snapshot into workstreams is the sync's job
// (internal/app, Sync), which also owns the rules about what may be
// overwritten. That split keeps connectors small and testable with a fake
// server, and the reconciliation rules in one place for every connector.
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
	// Pull fetches the current picture. known lists the identity keys the
	// store already attributes to this source; the source should include
	// their current state even when they no longer match its queries, so
	// finished work keeps its final status. Items for known keys that the
	// queries no longer select are returned with Discovered=false.
	Pull(ctx context.Context, known []string) (*Snapshot, error)
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
	// Notes are human-readable remarks about the pull (what was queried,
	// what was skipped) for `jug sync` output and the log.
	Notes []string `json:"notes,omitempty"`
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
