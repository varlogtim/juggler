package github

import (
	"testing"
	"time"
)

func TestDerive(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	rev := func(by, state string, at time.Time, oid string) ReviewEvent {
		return ReviewEvent{By: by, State: state, At: at, OID: oid}
	}
	req := func(of string, at time.Time) RequestedEvent { return RequestedEvent{Of: of, At: at} }
	base := func() *ReviewFacts {
		return &ReviewFacts{Open: true, Author: "tim", Decision: "REVIEW_REQUIRED", UpdatedAt: h(9), HeadOID: "c3", HeadAt: h(8)}
	}
	cases := []struct {
		name string
		f    func() *ReviewFacts
		want ReviewState
	}{
		{"merged: nothing", func() *ReviewFacts { f := base(); f.Open = false; f.Pending = []string{"jerry"}; return f }, ReviewState{}},
		{"draft: nothing", func() *ReviewFacts { f := base(); f.Draft = true; f.Pending = []string{"jerry"}; return f }, ReviewState{}},
		{"no reviewers yet: nothing", func() *ReviewFacts { return base() }, ReviewState{}},
		{"first request outstanding", func() *ReviewFacts {
			f := base()
			f.Pending = []string{"jerry"}
			f.Requests = []RequestedEvent{req("jerry", h(1))}
			return f
		}, ReviewState{"review-requested", h(1), "jerry"}},
		{"request older than the timeline window: updatedAt stands in", func() *ReviewFacts {
			f := base()
			f.Pending = []string{"jerry"}
			return f
		}, ReviewState{"review-requested", h(9), "jerry"}},
		{"commented, nothing pending", func() *ReviewFacts {
			f := base()
			f.Reviews = []ReviewEvent{rev("jerry", "COMMENTED", h(2), "c3")}
			return f
		}, ReviewState{"commented", h(2), "jerry"}},
		{"the author's own review is not a review", func() *ReviewFacts {
			f := base()
			f.Reviews = []ReviewEvent{rev("tim", "COMMENTED", h(2), "c3")}
			return f
		}, ReviewState{}},
		{"pushed after a comment review, no re-request", func() *ReviewFacts {
			f := base()
			f.Reviews = []ReviewEvent{rev("jerry", "COMMENTED", h(2), "c1")}
			f.HeadOID, f.HeadAt = "c3", h(5)
			return f
		}, ReviewState{"pushed", h(5), "jerry"}},
		{"re-review: pushed and asked a prior reviewer again (PR 1378 as seen live)", func() *ReviewFacts {
			f := base()
			f.Reviews = []ReviewEvent{rev("jerry", "COMMENTED", h(2), "c1"), rev("jerry", "COMMENTED", h(4), "c2")}
			f.Pending = []string{"jerry"}
			f.Requests = []RequestedEvent{req("jerry", h(0)), req("jerry", h(3)), req("jerry", h(7))}
			return f
		}, ReviewState{"re-review", h(7), "jerry"}},
		{"changes requested, not asked again: blocked", func() *ReviewFacts {
			f := base()
			f.Decision = "CHANGES_REQUESTED"
			f.Reviews = []ReviewEvent{rev("jerry", "CHANGES_REQUESTED", h(2), "c1")}
			f.HeadAt = h(5)
			return f
		}, ReviewState{"changes-requested", h(2), "jerry"}},
		{"changes requested, then re-requested: re-review wins", func() *ReviewFacts {
			f := base()
			f.Decision = "CHANGES_REQUESTED"
			f.Reviews = []ReviewEvent{rev("jerry", "CHANGES_REQUESTED", h(2), "c1")}
			f.Pending = []string{"jerry"}
			f.Requests = []RequestedEvent{req("jerry", h(6))}
			return f
		}, ReviewState{"re-review", h(6), "jerry"}},
		{"A asked for changes, B is pending a first review: still blocked by A", func() *ReviewFacts {
			f := base()
			f.Decision = "CHANGES_REQUESTED"
			f.Reviews = []ReviewEvent{rev("ann", "CHANGES_REQUESTED", h(2), "c1")}
			f.Pending = []string{"bob"}
			f.Requests = []RequestedEvent{req("bob", h(3))}
			return f
		}, ReviewState{"changes-requested", h(2), "ann"}},
		{"approved: waiting on merge", func() *ReviewFacts {
			f := base()
			f.Decision = "APPROVED"
			f.Reviews = []ReviewEvent{rev("jerry", "COMMENTED", h(2), "c1"), rev("jerry", "APPROVED", h(6), "c3")}
			return f
		}, ReviewState{"approved", h(6), "jerry"}},
		{"approved, then pushed again: GitHub still says approved", func() *ReviewFacts {
			f := base()
			f.Decision = "APPROVED"
			f.Reviews = []ReviewEvent{rev("jerry", "APPROVED", h(6), "c2")}
			f.HeadOID, f.HeadAt = "c3", h(8)
			return f
		}, ReviewState{"approved", h(6), "jerry"}},
		{"approval dismissed (stale review): back to pushed", func() *ReviewFacts {
			f := base()
			f.Reviews = []ReviewEvent{rev("jerry", "APPROVED", h(6), "c2"), rev("jerry", "DISMISSED", h(7), "c2")}
			f.HeadOID, f.HeadAt = "c3", h(8)
			return f
		}, ReviewState{"pushed", h(8), "jerry"}},
		{"two approvers: both named, latest time", func() *ReviewFacts {
			f := base()
			f.Decision = "APPROVED"
			f.Reviews = []ReviewEvent{rev("bob", "APPROVED", h(5), "c3"), rev("ann", "APPROVED", h(6), "c3")}
			return f
		}, ReviewState{"approved", h(6), "ann, bob"}},
		{"no protection rule (decision empty): an approval still counts", func() *ReviewFacts {
			f := base()
			f.Decision = ""
			f.Reviews = []ReviewEvent{rev("jerry", "APPROVED", h(6), "c3")}
			return f
		}, ReviewState{"approved", h(6), "jerry"}},
		{"a team requested: named as a team", func() *ReviewFacts {
			f := base()
			f.Pending = []string{"a team"}
			f.Requests = []RequestedEvent{req("a team", h(1))}
			return f
		}, ReviewState{"review-requested", h(1), "a team"}},
	}
	for _, c := range cases {
		got := Derive(c.f())
		if got.State != c.want.State || got.By != c.want.By || !got.At.Equal(c.want.At) {
			t.Errorf("%s:\n  got  %s %q at %v\n  want %s %q at %v", c.name, got.State, got.By, got.At, c.want.State, c.want.By, c.want.At)
		}
	}
}
