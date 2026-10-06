package app

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/varlogtim/juggler/internal/store"
)

// GroupInfo is a group as the CLI, the API and the UI see it: the registry
// entry (if any) plus what the tags say.
type GroupInfo struct {
	store.Group
	// Registered: has an entry in groups.toml (dates/kind/desc).
	Registered bool `json:"registered"`
	// Current: today is within [start, end].
	Current bool `json:"current"`
	// DaysLeft until end (inclusive of today); nil without an end date.
	DaysLeft *int `json:"days_left,omitempty"`
	// Members are the tagged workstreams' canonical names, in store order.
	Members []string `json:"members"`
	Count   int      `json:"count"`
}

// GroupPatch sets the fields that are non-nil.
type GroupPatch struct {
	Kind, Desc, Start, End *string
}

// Groups returns every group that has members, decorated from the
// registry, in group order; with includeEmpty, registered groups without
// members too.
func (a *App) Groups(includeEmpty bool) ([]GroupInfo, error) {
	reg, err := a.Store.LoadGroups()
	if err != nil {
		return nil, err
	}
	all, err := a.Store.List()
	if err != nil {
		return nil, err
	}
	byName := map[string]*GroupInfo{}
	var out []*GroupInfo
	add := func(g store.Group, registered bool) *GroupInfo {
		if gi, ok := byName[g.Name]; ok {
			return gi
		}
		gi := &GroupInfo{Group: g, Registered: registered, Members: []string{}}
		byName[g.Name] = gi
		out = append(out, gi)
		return gi
	}
	for _, g := range reg {
		add(g, true)
	}
	for _, w := range all {
		for _, name := range w.Groups {
			gi := add(store.Group{Name: name}, false)
			gi.Members = append(gi.Members, w.Name())
		}
	}
	now := time.Now()
	var res []GroupInfo
	for _, gi := range out {
		gi.Count = len(gi.Members)
		if gi.Count == 0 && !includeEmpty {
			continue
		}
		gi.Current = gi.IsCurrent(now)
		if end, ok := gi.EndTime(); ok {
			day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
			d := int(end.Sub(day).Hours()/24) + 1
			gi.DaysLeft = &d
		}
		res = append(res, *gi)
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].Less(res[j].Group) })
	return res, nil
}

// Group returns one group (registered or merely in use).
func (a *App) Group(name string) (*GroupInfo, error) {
	name, err := store.NormalizeGroupName(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	gs, err := a.Groups(true)
	if err != nil {
		return nil, err
	}
	for i := range gs {
		if gs[i].Name == name {
			return &gs[i], nil
		}
	}
	return nil, fmt.Errorf("%w: no group %q", ErrNotFound, name)
}

// KnownGroupNames lists registered and in-use groups, in group order.
func (a *App) KnownGroupNames() []string {
	gs, err := a.Groups(true)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(gs))
	for _, g := range gs {
		names = append(names, g.Name)
	}
	return names
}

// SetGroup creates or updates a registry entry (fields in p that are nil
// are left alone; set a field to "" to clear it).
func (a *App) SetGroup(name string, p GroupPatch) (store.Group, error) {
	name, err := store.NormalizeGroupName(name)
	if err != nil {
		return store.Group{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	reg, err := a.Store.LoadGroups()
	if err != nil {
		return store.Group{}, err
	}
	idx := -1
	for i := range reg {
		if reg[i].Name == name {
			idx = i
		}
	}
	if idx < 0 {
		reg = append(reg, store.Group{Name: name})
		idx = len(reg) - 1
	}
	g := &reg[idx]
	if p.Kind != nil {
		g.Kind = strings.TrimSpace(*p.Kind)
	}
	if p.Desc != nil {
		g.Desc = strings.TrimSpace(*p.Desc)
	}
	if p.Start != nil {
		g.Start = strings.TrimSpace(*p.Start)
	}
	if p.End != nil {
		g.End = strings.TrimSpace(*p.End)
	}
	if err := g.Validate(); err != nil {
		return store.Group{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	out := *g
	return out, a.Store.SaveGroups(reg)
}

// RemoveGroup drops the registry entry; with untag it also removes the tag
// from every member. It returns how many workstreams were untagged.
func (a *App) RemoveGroup(name string, untag bool) (int, error) {
	name, err := store.NormalizeGroupName(name)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	reg, err := a.Store.LoadGroups()
	if err != nil {
		return 0, err
	}
	kept := reg[:0]
	registered := false
	for _, g := range reg {
		if g.Name == name {
			registered = true
			continue
		}
		kept = append(kept, g)
	}
	n := 0
	if untag {
		all, err := a.Store.List()
		if err != nil {
			return 0, err
		}
		for _, w := range all {
			if w.InGroup(name) {
				if err := a.Untag(w, name); err != nil {
					return n, err
				}
				n++
			}
		}
	}
	if !registered && n == 0 {
		return 0, fmt.Errorf("%w: no group %q", ErrNotFound, name)
	}
	if registered {
		return n, a.Store.SaveGroups(kept)
	}
	return n, nil
}

// Tag adds w to the named groups (names are validated; unknown groups are
// fine — tagging is what brings a group into existence).
func (a *App) Tag(w *store.Workstream, names ...string) error {
	for _, n := range names {
		n, err := store.NormalizeGroupName(n)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if !w.InGroup(n) {
			w.Groups = append(w.Groups, n)
		}
	}
	return a.Store.Save(w)
}

// Untag removes w from the named groups.
func (a *App) Untag(w *store.Workstream, names ...string) error {
	drop := map[string]bool{}
	for _, n := range names {
		drop[strings.TrimSpace(n)] = true
	}
	kept := w.Groups[:0]
	for _, g := range w.Groups {
		if !drop[g] {
			kept = append(kept, g)
		}
	}
	w.Groups = kept
	return a.Store.Save(w)
}

// SetGroups replaces w's tags.
func (a *App) SetGroups(w *store.Workstream, names []string) error {
	var clean []string
	for _, n := range store.NormalizeGroups(names) {
		n, err := store.NormalizeGroupName(n)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		clean = append(clean, n)
	}
	w.Groups = clean
	return a.Store.Save(w)
}
