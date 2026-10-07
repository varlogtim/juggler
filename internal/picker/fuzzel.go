// Package picker renders the workstream list for a dmenu-style launcher
// (fuzzel) and maps the selection back.
package picker

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/varlogtim/juggler/internal/store"
)

// Entry is one picker row.
type Entry struct {
	W         *store.Workstream
	Displayed bool
	Live      bool // has windows (displayed or parked)
	Shown     time.Time
}

// NewMarker is the sentinel row that creates a workstream.
const NewMarker = "+ new workstream…"

// Visible drops finished workstreams that have no windows: nothing to go
// to there. (A finished one that still has windows stays, so you can reach
// them.)
func Visible(entries []Entry) []Entry {
	out := entries[:0:0]
	for _, e := range entries {
		if e.W.Finished() && !e.Live && !e.Displayed {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Lines renders entries as aligned columns. Displayed first, then live
// (most recently shown first), then the rest (yours, then teammates';
// newest first).
func Lines(entries []Entry) []string {
	entries = Visible(entries)
	// stable partition
	var displayed, live, rest []Entry
	for _, e := range entries {
		switch {
		case e.Displayed:
			displayed = append(displayed, e)
		case e.Live:
			live = append(live, e)
		default:
			rest = append(rest, e)
		}
	}
	sortBy(live, func(a, b Entry) bool { return a.Shown.After(b.Shown) })
	sortBy(rest, restLess)
	ordered := append(append(displayed, live...), rest...)

	rows := make([][]string, 0, len(ordered)+1)
	for _, e := range ordered {
		rows = append(rows, row(e))
	}
	widths := make([]int, 8)
	for _, r := range rows {
		for i, c := range r {
			if n := utf8.RuneCountInString(c); n > widths[i] {
				widths[i] = n
			}
		}
	}
	var out []string
	for _, r := range rows {
		var b strings.Builder
		for i, c := range r {
			b.WriteString(c)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)+2))
			}
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return append(out, NewMarker)
}

func row(e Entry) []string {
	glyph := "○"
	switch {
	case e.Displayed:
		glyph = "●"
	case e.Live:
		glyph = "◐"
	}
	if e.W.Finished() {
		glyph = "✓" // only ever seen on a finished workstream that still has windows
	}
	w := e.W
	jira, pr := "", ""
	if r := w.Ref("jira"); r != nil {
		jira = r.Status
		if jira == "" {
			jira = "jira"
		}
	}
	if r := w.Ref("pr"); r != nil {
		pr = prLabel(r)
	}
	todo := ""
	if n := w.OpenTodos(); n > 0 {
		todo = fmt.Sprintf("todo:%d", n)
	}
	if w.CodeDir == "" {
		todo = strings.TrimSpace(todo + " no-code")
	}
	desc := w.Desc
	if r := []rune(desc); len(r) > 60 {
		desc = string(r[:59]) + "…"
	}
	owner := ""
	if w.Owner != "" {
		owner = "@" + w.Owner
	}
	return []string{glyph + " " + w.ID, desc, jira, pr, todo, w.Category, owner, strings.Join(w.Groups, ",")}
}

func prLabel(r *store.Ref) string {
	key := r.Key
	if i := strings.LastIndex(key, "#"); i >= 0 {
		key = "PR" + key[i:]
	} else if key == "" {
		key = "PR"
	}
	if r.Status != "" {
		return key + " " + r.Status
	}
	return key
}

// Ordered returns the entries in the same order Lines renders them, so the
// selected index maps back to an entry.
func Ordered(entries []Entry) []Entry {
	entries = Visible(entries)
	var displayed, live, rest []Entry
	for _, e := range entries {
		switch {
		case e.Displayed:
			displayed = append(displayed, e)
		case e.Live:
			live = append(live, e)
		default:
			rest = append(rest, e)
		}
	}
	sortBy(live, func(a, b Entry) bool { return a.Shown.After(b.Shown) })
	sortBy(rest, restLess)
	return append(append(displayed, live...), rest...)
}

// restLess orders workstreams that have no windows: yours before
// teammates' (those with an owner), then newest first.
func restLess(a, b Entry) bool {
	if (a.W.Owner == "") != (b.W.Owner == "") {
		return a.W.Owner == ""
	}
	return a.W.Created.After(b.W.Created)
}

func sortBy(es []Entry, less func(a, b Entry) bool) {
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && less(es[j], es[j-1]); j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}
}

// ErrCancelled is returned when the user dismissed the picker.
var ErrCancelled = errors.New("cancelled")

// Fuzzel shows lines in fuzzel's dmenu mode and returns the selected index.
func Fuzzel(prompt string, lines []string) (int, error) {
	cmd := exec.Command("fuzzel", "--dmenu", "--index", "--prompt", prompt,
		"--width", "120", "--lines", strconv.Itoa(min(len(lines), 15)))
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return -1, ErrCancelled
		}
		return -1, err
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return -1, ErrCancelled
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return -1, fmt.Errorf("fuzzel returned %q", s)
	}
	return i, nil
}

// Prompt asks for a free-text line.
func Prompt(prompt string) (string, error) {
	cmd := exec.Command("fuzzel", "--dmenu", "--prompt", prompt, "--width", "80", "--lines", "0")
	cmd.Stdin = strings.NewReader("")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", ErrCancelled
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return "", ErrCancelled
	}
	return s, nil
}
