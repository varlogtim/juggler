package picker

import (
	"regexp"
	"strings"
)

// Spec is what the user typed into the "new workstream" prompt:
//
//	[category:] [TICKET-123] description
//
// e.g. "AISW-53350 e2etest smart routing regression", "personal: fix my
// dotfiles", "work: refactor the thing". The category prefix is only
// recognised when it is a known category (so "note: …" stays a
// description).
type Spec struct {
	Category string // "" = not given
	Key      string // ticket key, "" = none
	Desc     string
}

var (
	specCategory = regexp.MustCompile(`^([a-z][a-z0-9-]*):\s*`)
	specKey      = regexp.MustCompile(`^([A-Z][A-Z0-9]+-[0-9]+)\b[\s:.\-–—]*`)
)

// ParseSpec parses s. known lists the categories a prefix may name.
func ParseSpec(s string, known map[string]bool) Spec {
	var sp Spec
	s = strings.TrimSpace(s)
	if m := specCategory.FindStringSubmatch(s); m != nil && known[m[1]] {
		sp.Category = m[1]
		s = s[len(m[0]):]
	}
	if m := specKey.FindStringSubmatch(s); m != nil {
		sp.Key = m[1]
		s = s[len(m[0]):]
	}
	sp.Desc = strings.TrimSpace(s)
	return sp
}
