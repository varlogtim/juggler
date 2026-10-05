package picker

import "testing"

func TestParseSpec(t *testing.T) {
	known := map[string]bool{"work": true, "personal": true}
	cases := []struct {
		in   string
		want Spec
	}{
		{"AISW-53350 E2etest SmartRouting Regression", Spec{"", "AISW-53350", "E2etest SmartRouting Regression"}},
		{"AISW-53350: fix it", Spec{"", "AISW-53350", "fix it"}},
		{"AISW-53350 - fix it", Spec{"", "AISW-53350", "fix it"}},
		{"work: refactor thing", Spec{"work", "", "refactor thing"}},
		{"personal: AISW-1 odd but allowed", Spec{"personal", "AISW-1", "odd but allowed"}},
		{"note: remember this", Spec{"", "", "note: remember this"}},
		{"  plain description  ", Spec{"", "", "plain description"}},
		{"AISW-53350", Spec{"", "AISW-53350", ""}},
		{"lowercase-1 is not a key", Spec{"", "", "lowercase-1 is not a key"}},
	}
	for _, c := range cases {
		if got := ParseSpec(c.in, known); got != c.want {
			t.Errorf("ParseSpec(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}
