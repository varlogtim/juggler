// Package github is the GitHub connector: it keeps workstreams' `pr` refs
// current — title, state (open | draft | merged | closed) — and finds the
// pull request for a workstream's branch when none is attached yet. It
// never creates workstreams: a pull request is a fact about work, not the
// work's identity (a ticket can have a fix and a backport).
//
// Config (`[sources.<name>]`):
//
//	kind     = "github"
//	base_url = "https://github.hpe.com"     # the web host; API: <base>/api/v3 (+ /api/graphql), or api.github.com
//	token    = "file:~/secrets/github.token" # or "env:VAR", or the value
//	repos    = []                            # owner/repo to look branches up in; default: every repo a
//	                                         # workstream's checkout points at on this host
//	poll     = "1h"                          # scheduler interval; "0" = manual only
//
// Per pull: for every workstream, the PRs for its branch in its repo (one
// list request; skipped on the repo's default branch and for checkouts on
// another host), then one request per `pr` ref on this host the lists did
// not cover, then one GraphQL request per OPEN pull request for where it
// stands with its reviewers (review.go). Nothing is written by the
// connector; the sync applies the snapshot's ref updates like `jug ref
// add` would.
package github

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/varlogtim/juggler/internal/source"
	"github.com/varlogtim/juggler/internal/store"
)

func init() { source.Register(connector{}) }

// connector is the "github" kind.
type connector struct{}

func (connector) Kind() string { return "github" }

// Config is the connector's table in config.toml.
type Config struct {
	Kind    string   `toml:"kind"`
	BaseURL string   `toml:"base_url"`
	APIURL  string   `toml:"api_url"`     // override when the REST API is not where base_url implies
	GQLURL  string   `toml:"graphql_url"` // override when GraphQL is not where base_url implies
	Token   string   `toml:"token"`
	Repos   []string `toml:"repos"`
	Poll    string   `toml:"poll"`
}

// Source is a configured GitHub source.
type Source struct {
	name   string
	cfg    Config
	host   string // the web host refs on this source carry in their URL
	poll   time.Duration
	client *Client
	me     *User
}

// Open implements source.Connector.
func (connector) Open(name string, decode func(any) error, env source.Env) (source.Source, error) {
	cfg := Config{BaseURL: "https://github.com", Poll: "1h"}
	if err := decode(&cfg); err != nil {
		return nil, fmt.Errorf("source %s: %w", name, err)
	}
	var errs []string
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || u.Host == "" || u.Scheme == "" {
		errs = append(errs, fmt.Sprintf("base_url %q must be an https URL (the web host)", cfg.BaseURL))
	}
	if cfg.Token == "" {
		errs = append(errs, `token is required ("file:<path>", "env:<VAR>" or the value)`)
	}
	for _, r := range cfg.Repos {
		if strings.Count(r, "/") != 1 || strings.HasPrefix(r, "/") || strings.HasSuffix(r, "/") {
			errs = append(errs, fmt.Sprintf("repos: %q is not owner/repo", r))
		}
	}
	poll := time.Hour
	if cfg.Poll != "" {
		d, err := time.ParseDuration(cfg.Poll)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Sprintf("poll %q is not a duration (10m, 1h, 0)", cfg.Poll))
		} else {
			poll = d
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("source %s (github): %s", name, strings.Join(errs, "; "))
	}
	token, err := env.Secret(cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("source %s: token: %w", name, err)
	}
	api, gql := cfg.APIURL, cfg.GQLURL
	if api == "" {
		if u.Host == "github.com" {
			api = "https://api.github.com"
		} else {
			api = u.Scheme + "://" + u.Host + "/api/v3"
		}
	}
	if gql == "" {
		if u.Host == "github.com" {
			gql = "https://api.github.com/graphql"
		} else {
			gql = u.Scheme + "://" + u.Host + "/api/graphql"
		}
	}
	return &Source{name: name, cfg: cfg, host: u.Host, poll: poll, client: &Client{Base: api, GraphQL: gql, Token: token}}, nil
}

// Name, Kind, Poll and Describe implement source.Source.
func (s *Source) Name() string        { return s.name }
func (s *Source) Kind() string        { return "github" }
func (s *Source) Poll() time.Duration { return s.poll }
func (s *Source) Describe() string {
	repos := "repos from the workstreams' checkouts"
	if len(s.cfg.Repos) > 0 {
		repos = "repos " + strings.Join(s.cfg.Repos, ", ")
	}
	return fmt.Sprintf("%s, %s; pr refs + review state", s.host, repos)
}

// prKey is owner/repo#N, as app.PRKey makes it from a PR URL.
var prKey = regexp.MustCompile(`^([^/#\s]+/[^/#\s]+)#(\d+)$`)

// Pull implements source.Source.
func (s *Source) Pull(ctx context.Context, inv source.Inventory) (*source.Snapshot, error) {
	snap := &source.Snapshot{Source: s.name, Taken: time.Now(), Groups: []source.Group{}, Items: []source.Item{}}
	if s.me == nil {
		me, err := s.client.Me(ctx)
		if err != nil {
			return nil, err
		}
		s.me = me
	}
	watched := map[string]bool{}
	for _, r := range s.cfg.Repos {
		watched[r] = true
	}
	covered := map[string]bool{} // "ws\x00key" already produced
	var fromLists, fromKeys, branches, reviewed int
	var skipped []string
	type pending struct {
		idx  int // into snap.Refs
		repo string
		pr   PR
		prev *store.Ref // the workstream's current ref, when it has one
	}
	var open []pending
	update := func(v source.WorkstreamView, repo string, pr PR) {
		key := repo + "#" + strconv.Itoa(pr.Number)
		if covered[v.Name+"\x00"+key] {
			return
		}
		covered[v.Name+"\x00"+key] = true
		snap.Refs = append(snap.Refs, source.RefUpdate{
			Workstream: v.Name,
			Ref:        store.Ref{Type: "pr", Key: key, URL: pr.HTMLURL, Title: pr.Title, Status: pr.Status()},
			Closed:     pr.Closed(),
		})
		if pr.Status() == "open" {
			open = append(open, pending{idx: len(snap.Refs) - 1, repo: repo, pr: pr, prev: v.Ref("pr", key)})
		}
	}

	// 1. the PRs of each workstream's branch
	for _, v := range inv.Workstreams {
		repo := s.repoOf(v.Remote)
		switch {
		case repo == "" || v.Branch == "":
			continue
		case len(watched) > 0 && !watched[repo]:
			continue
		case v.Branch == v.DefaultBranch || (v.DefaultBranch == "" && isDefaultName(v.Branch)):
			continue // a shared main checkout: its branch is nobody's PR
		}
		owner := repo[:strings.Index(repo, "/")]
		prs, err := s.client.PullsForBranch(ctx, repo, owner, v.Branch)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%s %s): %v", v.ID, repo, v.Branch, err))
			continue
		}
		branches++
		for _, pr := range prs {
			update(v, repo, pr)
			fromLists++
		}
	}

	// 2. pr refs on this host the lists did not reach (another branch, a
	//    backport in another repo, a checkout elsewhere)
	for _, v := range inv.Workstreams {
		for _, r := range v.Refs {
			if r.Type != "pr" || !s.onHost(r.URL) {
				continue
			}
			m := prKey.FindStringSubmatch(r.Key)
			if m == nil {
				continue
			}
			if covered[v.Name+"\x00"+r.Key] {
				continue
			}
			n, _ := strconv.Atoi(m[2])
			pr, err := s.client.PullRequest(ctx, m[1], n)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s (%s): %v", v.ID, r.Key, err))
				continue
			}
			update(v, m[1], *pr)
			fromKeys++
		}
	}
	// 3. where each OPEN pull request stands with its reviewers. A failed
	//    lookup keeps the state last recorded rather than blanking it.
	for _, p := range open {
		ref := &snap.Refs[p.idx].Ref
		facts, err := s.client.ReviewFacts(ctx, p.repo, p.pr.Number)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("review state of %s: %v", ref.Key, err))
			if p.prev != nil {
				ref.Review, ref.ReviewAt, ref.ReviewBy = p.prev.Review, p.prev.ReviewAt, p.prev.ReviewBy
			}
			continue
		}
		st := Derive(facts)
		ref.Review, ref.ReviewAt, ref.ReviewBy = st.State, st.At, st.By
		reviewed++
	}
	sort.Slice(snap.Refs, func(i, j int) bool {
		if snap.Refs[i].Workstream != snap.Refs[j].Workstream {
			return snap.Refs[i].Workstream < snap.Refs[j].Workstream
		}
		return snap.Refs[i].Ref.Key < snap.Refs[j].Ref.Key
	})
	snap.Notes = append(snap.Notes, fmt.Sprintf("%s as %s: %d pull request(s) for %d branch(es), %d by ref, review state of %d open", s.host, s.me.Login, fromLists, branches, fromKeys, reviewed))
	for _, sk := range skipped {
		snap.Notes = append(snap.Notes, "skipped "+sk)
	}
	return snap, nil
}

// repoOf maps a checkout's remote URL to owner/repo when it lives on this
// source's host: git@host:owner/repo.git, ssh://git@host/owner/repo.git,
// https://host/owner/repo(.git). "" otherwise.
func (s *Source) repoOf(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	var host, path string
	if i := strings.Index(remote, "://"); i >= 0 {
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		host, path = u.Hostname(), u.Path
	} else if at, colon := strings.Index(remote, "@"), strings.Index(remote, ":"); colon > at {
		// scp-like: [user@]host:path
		host, path = remote[at+1:colon], remote[colon+1:]
	} else {
		return ""
	}
	if !strings.EqualFold(host, s.host) {
		return ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if strings.Count(path, "/") != 1 {
		return ""
	}
	return path
}

// onHost reports whether a ref URL points at this source's host.
func (s *Source) onHost(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Hostname(), s.host)
}

// isDefaultName is the fallback when a checkout's default branch is not
// known: the usual names for one.
func isDefaultName(b string) bool {
	switch b {
	case "main", "master", "develop", "trunk":
		return true
	}
	return false
}
