package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a minimal GitHub REST client (github.com or GitHub Enterprise
// Server, which serves the same v3 API under /api/v3).
type Client struct {
	Base  string // API base: https://api.github.com or https://ghes.example.com/api/v3
	Token string
	HTTP  *http.Client
}

// Error is a non-2xx answer.
type Error struct {
	Status int
	Path   string
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("github %s: HTTP %d: %s", e.Path, e.Status, e.Body)
}

// get performs one authenticated GET and decodes the JSON body into out; a
// non-2xx status becomes an *Error carrying the (truncated) body.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := strings.TrimRight(c.Base, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "token "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body := strings.TrimSpace(string(b))
		if len(body) > 300 {
			body = body[:300] + "…"
		}
		return &Error{Status: resp.StatusCode, Path: path, Body: body}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// User is who the token belongs to.
type User struct {
	Login string `json:"login"`
}

// Me returns the authenticated user.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var u User
	if err := c.get(ctx, "/user", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// PR is a pull request, the fields juggler cares about. The list endpoint
// omits `merged`; MergedAt is set either way, so Merged() reads that.
type PR struct {
	Number   int        `json:"number"`
	State    string     `json:"state"` // open | closed
	Draft    bool       `json:"draft"`
	Title    string     `json:"title"`
	HTMLURL  string     `json:"html_url"`
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// Merged reports whether the PR was merged.
func (p PR) Merged() bool { return p.MergedAt != nil }

// Status is the PR's state in juggler's vocabulary: open | draft | merged | closed.
func (p PR) Status() string {
	switch {
	case p.Merged():
		return "merged"
	case p.State == "closed":
		return "closed"
	case p.Draft:
		return "draft"
	default:
		return "open"
	}
}

// Closed: nothing more will happen on this PR.
func (p PR) Closed() bool { return p.State == "closed" }

// PullRequest fetches one PR by number.
func (c *Client) PullRequest(ctx context.Context, repo string, n int) (*PR, error) {
	var pr PR
	if err := c.get(ctx, "/repos/"+repo+"/pulls/"+strconv.Itoa(n), nil, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// PullsForBranch lists the PRs (any state) whose head is owner:branch in
// repo, newest first. owner is the repo's owner for same-repo branches.
func (c *Client) PullsForBranch(ctx context.Context, repo, owner, branch string) ([]PR, error) {
	var prs []PR
	q := url.Values{"state": {"all"}, "head": {owner + ":" + branch}, "per_page": {"20"}, "sort": {"created"}, "direction": {"desc"}}
	if err := c.get(ctx, "/repos/"+repo+"/pulls", q, &prs); err != nil {
		return nil, err
	}
	return prs, nil
}
