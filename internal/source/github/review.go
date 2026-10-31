package github

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Where a pull request stands with its reviewers is not one field on
// GitHub's side but a handful of facts: the pending review requests, the
// reviews so far (and GitHub's own verdict on them, reviewDecision), and
// the order in which requests, reviews and pushes happened. GraphQL gives
// all of them in one request per pull request — including the recent
// timeline, which is the only place a review REQUEST carries a timestamp
// (the REST pull object lists requested reviewers, not when). One REST
// shortcut is a trap worth naming: `latestReviews` omits a reviewer who
// has a pending request, which is exactly the reviewer a re-review is
// about. So the derivation below works from the reviews and the timeline.

// reviewQuery fetches the review facts of one pull request. Team reviewers
// are reduced to their type: naming a team needs the read:org scope.
const reviewQuery = `query($o:String!,$r:String!,$n:Int!){ repository(owner:$o,name:$r){ pullRequest(number:$n){
  isDraft state reviewDecision updatedAt
  author{login}
  commits(last:1){nodes{commit{oid committedDate}}}
  reviewRequests(first:20){nodes{requestedReviewer{__typename ... on User{login}}}}
  reviews(last:30){nodes{author{login} state submittedAt commit{oid}}}
  timelineItems(last:40, itemTypes:[REVIEW_REQUESTED_EVENT,HEAD_REF_FORCE_PUSHED_EVENT,PULL_REQUEST_COMMIT]){nodes{__typename
    ... on ReviewRequestedEvent{createdAt requestedReviewer{__typename ... on User{login}}}
    ... on HeadRefForcePushedEvent{createdAt}
    ... on PullRequestCommit{commit{oid committedDate}}}}
}}}`

// ReviewFacts is what the derivation reads: the query's answer, flattened.
type ReviewFacts struct {
	Draft     bool
	Open      bool
	Decision  string // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | "" (no protection rule, or unknown)
	UpdatedAt time.Time
	Author    string
	HeadOID   string
	HeadAt    time.Time
	Pending   []string         // logins (or "a team") with a review request outstanding
	Reviews   []ReviewEvent    // every review, oldest first
	Requests  []RequestedEvent // review requests in the recent timeline, oldest first
}

// ReviewEvent is one submitted review.
type ReviewEvent struct {
	By    string
	State string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	At    time.Time
	OID   string // the commit reviewed
}

// RequestedEvent is one "review requested" moment.
type RequestedEvent struct {
	Of string
	At time.Time
}

// ReviewState is the derived answer: Ref.Review / ReviewAt / ReviewBy.
type ReviewState struct {
	State string
	At    time.Time
	By    string
}

// reviewResponse mirrors the query for decoding.
type reviewResponse struct {
	Data struct {
		Repository struct {
			PullRequest *struct {
				IsDraft        bool   `json:"isDraft"`
				State          string `json:"state"`
				ReviewDecision string `json:"reviewDecision"`
				UpdatedAt      time.Time
				Author         struct{ Login string }
				Commits        struct {
					Nodes []struct {
						Commit struct {
							OID           string    `json:"oid"`
							CommittedDate time.Time `json:"committedDate"`
						}
					}
				}
				ReviewRequests struct {
					Nodes []struct {
						RequestedReviewer struct {
							Typename string `json:"__typename"`
							Login    string
						} `json:"requestedReviewer"`
					}
				} `json:"reviewRequests"`
				Reviews struct {
					Nodes []struct {
						Author      struct{ Login string }
						State       string
						SubmittedAt time.Time `json:"submittedAt"`
						Commit      *struct {
							OID string `json:"oid"`
						}
					}
				}
				TimelineItems struct {
					Nodes []struct {
						Typename          string    `json:"__typename"`
						CreatedAt         time.Time `json:"createdAt"`
						RequestedReviewer *struct {
							Typename string `json:"__typename"`
							Login    string
						} `json:"requestedReviewer"`
						Commit *struct {
							OID           string    `json:"oid"`
							CommittedDate time.Time `json:"committedDate"`
						}
					}
				} `json:"timelineItems"`
			} `json:"pullRequest"`
		}
	}
	Errors []struct {
		Message string
		Type    string
	}
}

// reviewer names a requested reviewer: the login, or "a team" when the
// token may not read team names.
func reviewer(typename, login string) string {
	if login != "" {
		return login
	}
	if typename == "Team" {
		return "a team"
	}
	return typename
}

// ReviewFacts runs reviewQuery for one pull request.
func (c *Client) ReviewFacts(ctx context.Context, repo string, n int) (*ReviewFacts, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("%q is not owner/repo", repo)
	}
	var resp reviewResponse
	if err := c.graphql(ctx, reviewQuery, map[string]any{"o": owner, "r": name, "n": n}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("github graphql %s#%d: %s", repo, n, resp.Errors[0].Message)
	}
	pr := resp.Data.Repository.PullRequest
	if pr == nil {
		return nil, fmt.Errorf("github graphql %s#%d: no such pull request", repo, n)
	}
	f := &ReviewFacts{Draft: pr.IsDraft, Open: pr.State == "OPEN", Decision: pr.ReviewDecision, UpdatedAt: pr.UpdatedAt, Author: pr.Author.Login}
	if len(pr.Commits.Nodes) > 0 {
		f.HeadOID, f.HeadAt = pr.Commits.Nodes[0].Commit.OID, pr.Commits.Nodes[0].Commit.CommittedDate
	}
	for _, rr := range pr.ReviewRequests.Nodes {
		f.Pending = append(f.Pending, reviewer(rr.RequestedReviewer.Typename, rr.RequestedReviewer.Login))
	}
	for _, r := range pr.Reviews.Nodes {
		ev := ReviewEvent{By: r.Author.Login, State: r.State, At: r.SubmittedAt}
		if r.Commit != nil {
			ev.OID = r.Commit.OID
		}
		f.Reviews = append(f.Reviews, ev)
	}
	for _, t := range pr.TimelineItems.Nodes {
		switch t.Typename {
		case "ReviewRequestedEvent":
			if t.RequestedReviewer != nil {
				f.Requests = append(f.Requests, RequestedEvent{Of: reviewer(t.RequestedReviewer.Typename, t.RequestedReviewer.Login), At: t.CreatedAt})
			}
		case "PullRequestCommit":
			// the head commit's date comes from commits(last:1); nothing to add
		case "HeadRefForcePushedEvent":
			// a force-push rewrites history; the head commit's date still says when
		}
	}
	return f, nil
}

// graphql posts one query to the GraphQL endpoint and decodes the reply.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	return c.post(ctx, c.GraphQL, body, out)
}

// Derive turns the facts into the one review state the pull request is in
// (see store.ReviewStates for the vocabulary), from most to least
// pressing. The author's own reviews are not reviews. Only open,
// non-draft pull requests have a state.
func Derive(f *ReviewFacts) ReviewState {
	if f == nil || !f.Open || f.Draft {
		return ReviewState{}
	}
	// standing per reviewer: the latest decision-bearing review (approve /
	// request changes); a dismissal clears it; comments change nothing
	standing := map[string]ReviewEvent{}
	reviewed := map[string]ReviewEvent{} // latest review of any kind, per reviewer
	var last ReviewEvent                 // the latest review by anyone
	for _, r := range f.Reviews {
		if r.By == "" || r.By == f.Author || r.State == "PENDING" {
			continue
		}
		reviewed[r.By] = r
		if r.At.After(last.At) {
			last = r
		}
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED":
			standing[r.By] = r
		case "DISMISSED":
			delete(standing, r.By)
		}
	}
	pending := map[string]bool{}
	for _, p := range f.Pending {
		pending[p] = true
	}
	requestedAt := func(who map[string]bool) time.Time {
		var at time.Time
		for _, q := range f.Requests {
			if who[q.Of] && q.At.After(at) {
				at = q.At
			}
		}
		if at.IsZero() {
			at = f.UpdatedAt // the request predates the timeline window
		}
		return at
	}

	// 1. changes requested by someone who is NOT being asked again: blocked
	var blockers, approvers []string
	var blockedAt, approvedAt time.Time
	for by, r := range standing {
		switch {
		case r.State == "CHANGES_REQUESTED" && !pending[by]:
			blockers = append(blockers, by)
			if r.At.After(blockedAt) {
				blockedAt = r.At
			}
		case r.State == "APPROVED":
			approvers = append(approvers, by)
			if r.At.After(approvedAt) {
				approvedAt = r.At
			}
		}
	}
	sort.Strings(blockers)
	sort.Strings(approvers)
	// 2. a pending request to someone who already reviewed: the author asked again
	rere := map[string]bool{}
	var firstTimers []string
	for _, p := range f.Pending {
		if _, ok := reviewed[p]; ok {
			rere[p] = true
		} else {
			firstTimers = append(firstTimers, p)
		}
	}
	switch {
	case len(blockers) > 0 && f.Decision != "APPROVED":
		return ReviewState{State: "changes-requested", At: blockedAt, By: strings.Join(blockers, ", ")}
	case len(rere) > 0:
		return ReviewState{State: "re-review", At: requestedAt(rere), By: strings.Join(sortedKeys(rere), ", ")}
	case f.Decision == "APPROVED" || (f.Decision == "" && len(approvers) > 0):
		return ReviewState{State: "approved", At: approvedAt, By: strings.Join(approvers, ", ")}
	case len(firstTimers) > 0:
		who := map[string]bool{}
		for _, p := range firstTimers {
			who[p] = true
		}
		return ReviewState{State: "review-requested", At: requestedAt(who), By: strings.Join(firstTimers, ", ")}
	case last.By != "" && !f.HeadAt.IsZero() && f.HeadAt.After(last.At) && f.HeadOID != last.OID:
		return ReviewState{State: "pushed", At: f.HeadAt, By: last.By}
	case last.By != "":
		return ReviewState{State: "commented", At: last.At, By: last.By}
	}
	return ReviewState{}
}

// sortedKeys returns m's keys in order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
