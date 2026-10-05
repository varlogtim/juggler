package sway

import (
	"regexp"
	"strconv"
	"strings"
)

// Node is one node of the layout tree (root, output, workspace, container
// or view). Only the fields juggler needs are decoded.
type Node struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Type           string   `json:"type"` // root, output, workspace, con, floating_con
	Layout         string   `json:"layout"`
	Num            int      `json:"num"` // workspaces only
	Rect           Rect     `json:"rect"`
	Focused        bool     `json:"focused"`
	Focus          []int64  `json:"focus"` // focus order of children, most recent first
	Marks          []string `json:"marks"`
	AppID          string   `json:"app_id"`
	PID            int      `json:"pid"`
	Shell          string   `json:"shell"`
	Visible        *bool    `json:"visible"`
	Nodes          []*Node  `json:"nodes"`
	FloatingNodes  []*Node  `json:"floating_nodes"`
	Representation string   `json:"representation"`
	WindowProps    *struct {
		Class    string `json:"class"`
		Instance string `json:"instance"`
	} `json:"window_properties"`
}

// Rect is a node geometry.
type Rect struct {
	X, Y, Width, Height int
}

// IsView reports whether the node is a window (has a client), as opposed to
// a split/stack container.
func (n *Node) IsView() bool { return n.PID != 0 || n.AppID != "" || n.Shell != "" }

// IsWorkspace reports whether the node is a workspace.
func (n *Node) IsWorkspace() bool { return n.Type == "workspace" }

// HasMark reports whether the node carries mark m.
func (n *Node) HasMark(m string) bool {
	for _, x := range n.Marks {
		if x == m {
			return true
		}
	}
	return false
}

// Walk calls f for n and every descendant (tiling and floating), pre-order.
// Walking stops when f returns false.
func (n *Node) Walk(f func(*Node) bool) bool {
	if n == nil {
		return true
	}
	if !f(n) {
		return false
	}
	for _, c := range n.Nodes {
		if !c.Walk(f) {
			return false
		}
	}
	for _, c := range n.FloatingNodes {
		if !c.Walk(f) {
			return false
		}
	}
	return true
}

// Find returns the first node (pre-order) for which pred is true.
func (n *Node) Find(pred func(*Node) bool) *Node {
	var found *Node
	n.Walk(func(x *Node) bool {
		if pred(x) {
			found = x
			return false
		}
		return true
	})
	return found
}

// FindAll returns every node for which pred is true.
func (n *Node) FindAll(pred func(*Node) bool) []*Node {
	var out []*Node
	n.Walk(func(x *Node) bool {
		if pred(x) {
			out = append(out, x)
		}
		return true
	})
	return out
}

// ByID finds a node by id.
func (n *Node) ByID(id int64) *Node { return n.Find(func(x *Node) bool { return x.ID == id }) }

// ByMark finds the node carrying mark m.
func (n *Node) ByMark(m string) *Node {
	return n.Find(func(x *Node) bool { return x.HasMark(m) })
}

// ByAppID finds the first view with the given app_id.
func (n *Node) ByAppID(appID string) *Node {
	return n.Find(func(x *Node) bool { return x.AppID == appID })
}

// WorkspaceNamed finds a workspace by exact name.
func (n *Node) WorkspaceNamed(name string) *Node {
	return n.Find(func(x *Node) bool { return x.IsWorkspace() && x.Name == name })
}

// WorkspaceNum finds a workspace by number (the "N" of "N:name").
func (n *Node) WorkspaceNum(num int) *Node {
	return n.Find(func(x *Node) bool { return x.IsWorkspace() && x.Num == num && x.Name != "__i3_scratch" })
}

// Scratchpad returns the hidden scratchpad workspace node.
func (n *Node) Scratchpad() *Node { return n.WorkspaceNamed("__i3_scratch") }

// FocusedNode returns the node that has keyboard focus (a view, a
// container, or a workspace when it is focused directly).
func (n *Node) FocusedNode() *Node { return n.Find(func(x *Node) bool { return x.Focused }) }

// WorkspaceOf returns the workspace node that contains the node with id, or
// nil (e.g. when it is parked in the scratchpad).
func (n *Node) WorkspaceOf(id int64) *Node {
	var ws *Node
	for _, out := range n.Nodes {
		for _, w := range out.Nodes {
			if !w.IsWorkspace() {
				continue
			}
			if w.ByID(id) != nil {
				ws = w
			}
		}
	}
	return ws
}

// ParentOf returns the parent node of id, or nil.
func (n *Node) ParentOf(id int64) *Node {
	var parent *Node
	n.Walk(func(x *Node) bool {
		for _, c := range x.Nodes {
			if c.ID == id {
				parent = x
				return false
			}
		}
		for _, c := range x.FloatingNodes {
			if c.ID == id {
				parent = x
				return false
			}
		}
		return true
	})
	return parent
}

// InScratchpad reports whether id lives under the scratchpad workspace.
func (n *Node) InScratchpad(id int64) bool {
	sp := n.Scratchpad()
	return sp != nil && sp.ByID(id) != nil
}

// Views returns every view (window) under n.
func (n *Node) Views() []*Node { return n.FindAll(func(x *Node) bool { return x.IsView() }) }

// Shape renders a compact description of a subtree, e.g.
// "splith[stacked[view view] stacked[view]]" — used by `jug doctor` and tests.
func (n *Node) Shape() string {
	if n.IsView() {
		return "view"
	}
	var parts []string
	for _, c := range n.Nodes {
		parts = append(parts, c.Shape())
	}
	return n.Layout + "[" + strings.Join(parts, " ") + "]"
}

// QuoteRegex escapes s for use inside a sway criteria regex such as
// [con_mark="^...$"].
func QuoteRegex(s string) string { return regexp.QuoteMeta(s) }

// Criteria builders — keep every criteria string in one place.

// MarkCrit returns a criteria matching exactly mark m.
func MarkCrit(m string) string { return `[con_mark="^` + QuoteRegex(m) + `$"]` }

// IDCrit returns a criteria matching node id.
func IDCrit(id int64) string { return `[con_id=` + strconv.FormatInt(id, 10) + `]` }

// AppIDCrit returns a criteria matching exactly app_id a.
func AppIDCrit(a string) string { return `[app_id="^` + QuoteRegex(a) + `$"]` }

// Quote wraps s in double quotes for a sway command argument (workspace
// names, marks), escaping embedded quotes and backslashes.
func Quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
