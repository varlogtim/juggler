package layout

import (
	"os/exec"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"ses_abc":           "ses_abc",
		"/home/tim/x":       "/home/tim/x",
		"two words":         "'two words'",
		"opencode -s ses_x": "'opencode -s ses_x'",
		"tim's thing":       `'tim'"'"'s thing'`,
		`say "hi" $x`:       `'say "hi" $x'`,
		`a\b`:               `'a'\\'b'`,
		"":                  "''",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
	inputs := []string{"a'b", "'", "a'b'c", `'\''`, `x"`, `"'`, `a\`, `\'`, `it's "both" $x \ end`, "tabs\tand\nnewlines"}
	for _, in := range inputs {
		got := shellQuote(in)
		// sway trap: an odd run of backslashes right before a quote
		for i := 0; i < len(got); i++ {
			if got[i] == '\'' || got[i] == '"' {
				n := 0
				for j := i - 1; j >= 0 && got[j] == '\\'; j-- {
					n++
				}
				if n%2 == 1 {
					t.Errorf("shellQuote(%q) = %s: odd backslash run before a quote at %d", in, got, i)
				}
			}
		}
		// sh round trip
		out, err := exec.Command("sh", "-c", "printf '%s' "+got).Output()
		if err != nil {
			t.Errorf("sh failed on %s: %v", got, err)
			continue
		}
		if string(out) != in {
			t.Errorf("sh round trip of %q via %s gave %q", in, got, string(out))
		}
		_ = strings.TrimSpace
	}
}
