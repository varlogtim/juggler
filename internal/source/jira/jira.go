// Package jira is the Jira connector: a board's sprints become groups (with
// their dates and a link to the board), and tickets — yours, and everything
// in the current sprint — become workstreams tagged into their sprint or
// into a backlog group.
//
// Config (`[sources.<name>]`):
//
//	kind          = "jira"
//	base_url      = "https://site.atlassian.net"   # default: jira_base_url
//	email         = "you@example.com"
//	token         = "file:~/secrets/jira.token"    # or the token itself
//	board         = 2112                           # the team's scrum board: sprints -> groups
//	sprint_match  = "Nebula"                       # keep sprints whose name contains this ("" = all)
//	assignee      = "me"                           # me: assignee = currentUser(); any: no restriction; else a JQL value
//	scope         = ["open", "sprint"]             # open: unresolved tickets; sprint: tickets in the current sprint (incl. finished)
//	jql           = ""                             # extra filter ANDed to every scope, e.g. "project = AISW AND issuetype != Epic"
//	category      = "work"                         # category for created workstreams
//	backlog_group = "backlog"                      # group for tickets in no (open) sprint; "" = don't tag
//	poll          = "1h"                           # scheduler interval; "0" = manual only
//	sprint_field  = ""                             # custom field id; discovered when empty
//	group_url     = ""                             # template for a sprint's link; see sprintURL
package jira

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/varlogtim/juggler/internal/source"
	"github.com/varlogtim/juggler/internal/store"
)

func init() { source.Register(connector{}) }

// connector is the "jira" kind: it only knows how to open a Source from a
// config table.
type connector struct{}

func (connector) Kind() string { return "jira" }

// Config is the connector's table in config.toml.
type Config struct {
	Kind         string   `toml:"kind"`
	BaseURL      string   `toml:"base_url"`
	Email        string   `toml:"email"`
	Token        string   `toml:"token"`
	Board        int      `toml:"board"`
	SprintMatch  string   `toml:"sprint_match"`
	Assignee     string   `toml:"assignee"`
	Scope        []string `toml:"scope"`
	JQL          string   `toml:"jql"`
	Category     string   `toml:"category"`
	BacklogGroup string   `toml:"backlog_group"`
	Poll         string   `toml:"poll"`
	SprintField  string   `toml:"sprint_field"`
	GroupURL     string   `toml:"group_url"`
}

// Source is a configured Jira source.
type Source struct {
	name   string
	cfg    Config
	poll   time.Duration
	client *Client

	// learned once, cached for the process
	me          *User
	sprintField string
	board       *Board
}

// Open implements source.Connector.
func (connector) Open(name string, decode func(any) error, env source.Env) (source.Source, error) {
	cfg := Config{Scope: []string{"open", "sprint"}, Assignee: "me", Category: env.DefaultCategory, BacklogGroup: "backlog", Poll: "1h"}
	if cfg.Category == "" || cfg.Category == "personal" {
		cfg.Category = "work"
	}
	if err := decode(&cfg); err != nil {
		return nil, fmt.Errorf("source %s: %w", name, err)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = env.JiraBaseURL
	}
	var errs []string
	if cfg.BaseURL == "" {
		errs = append(errs, "base_url (or the global jira_base_url) is required")
	}
	if cfg.Email == "" {
		errs = append(errs, "email is required")
	}
	if cfg.Token == "" {
		errs = append(errs, `token is required ("file:<path>" or the value)`)
	}
	if cfg.Board <= 0 {
		errs = append(errs, "board (a numeric board id) is required")
	}
	for _, s := range cfg.Scope {
		if s != "open" && s != "sprint" {
			errs = append(errs, fmt.Sprintf("scope %q is not open|sprint", s))
		}
	}
	if len(cfg.Scope) == 0 {
		errs = append(errs, "scope must name at least one of open, sprint")
	}
	if strings.TrimSpace(cfg.Assignee) == "" {
		errs = append(errs, `assignee must be "me", "any" or a JQL value`)
	}
	poll := time.Hour
	if cfg.Poll != "" {
		d, err := time.ParseDuration(cfg.Poll)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Sprintf("poll %q is not a duration (5m, 1h, 0)", cfg.Poll))
		} else {
			poll = d
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("source %s (jira): %s", name, strings.Join(errs, "; "))
	}
	token, err := env.Secret(cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("source %s: token: %w", name, err)
	}
	return &Source{
		name: name, cfg: cfg, poll: poll,
		client: &Client{Base: cfg.BaseURL, Email: cfg.Email, Token: token},
	}, nil
}

// Name, Kind, Poll and Describe implement source.Source.
func (s *Source) Name() string        { return s.name }
func (s *Source) Kind() string        { return "jira" }
func (s *Source) Poll() time.Duration { return s.poll }
func (s *Source) Describe() string {
	return fmt.Sprintf("%s board %d, %s, assignee %s — %s", s.cfg.BaseURL, s.cfg.Board, strings.Join(s.cfg.Scope, "+"), s.cfg.Assignee, s.cfg.Email)
}

// assigneeJQL is the clause the assignee knob adds to every scope.
func (s *Source) assigneeJQL() string {
	switch v := strings.TrimSpace(s.cfg.Assignee); strings.ToLower(v) {
	case "me":
		return " AND assignee = currentUser()"
	case "any", "*":
		return ""
	default:
		if strings.ContainsAny(v, "()=") { // already a JQL clause: assignee in (...)
			return " AND (" + v + ")"
		}
		return ` AND assignee = "` + strings.ReplaceAll(v, `"`, ``) + `"`
	}
}

// prepare learns the instance facts once: who we are, the sprint field id,
// the board (for the project key in sprint URLs).
func (s *Source) prepare(ctx context.Context) error {
	if s.me == nil {
		me, err := s.client.Myself(ctx)
		if err != nil {
			return err
		}
		s.me = &me
	}
	if s.sprintField == "" {
		s.sprintField = s.cfg.SprintField
		if s.sprintField == "" {
			id, err := s.client.SprintFieldID(ctx)
			if err != nil {
				return err
			}
			s.sprintField = id
		}
	}
	if s.board == nil {
		b, err := s.client.GetBoard(ctx, s.cfg.Board)
		if err != nil {
			return err
		}
		s.board = &b
	}
	return nil
}

// Pull implements source.Source.
func (s *Source) Pull(ctx context.Context, inv source.Inventory) (*source.Snapshot, error) {
	known := inv.Known
	if err := s.prepare(ctx); err != nil {
		return nil, err
	}
	snap := &source.Snapshot{Source: s.name, Taken: time.Now()}

	// 1. sprints -> groups. Active and future sprints of the board that match.
	sprints, err := s.client.Sprints(ctx, s.cfg.Board, "active,future")
	if err != nil {
		return nil, err
	}
	managed := map[string]Sprint{} // by name
	var current *Sprint
	for _, sp := range sprints {
		if s.cfg.SprintMatch != "" && !strings.Contains(sp.Name, s.cfg.SprintMatch) {
			continue
		}
		managed[sp.Name] = sp
		snap.Groups = append(snap.Groups, source.Group{
			Group: store.Group{
				Name: sp.Name, Kind: "sprint", Desc: sp.Goal,
				Start: LocalDate(sp.StartDate), End: LocalDate(sp.EndDate),
				URL: s.sprintURL(sp), Source: s.name,
			},
			State: sp.State,
		})
		sp := sp
		switch {
		case sp.State == "active" && (current == nil || current.State != "active"):
			current = &sp
		case sp.State == "future" && current == nil:
			current = &sp
		case sp.State == "future" && current.State == "future" && sp.StartDate < current.StartDate:
			current = &sp
		}
	}
	if s.cfg.BacklogGroup != "" {
		snap.Groups = append(snap.Groups, source.Group{Group: store.Group{
			Name: s.cfg.BacklogGroup, Kind: "bucket", Desc: "assigned to you, not in a sprint", Source: s.name,
		}})
	}
	if current != nil {
		snap.Notes = append(snap.Notes, fmt.Sprintf("current sprint: %s (%s, %s → %s)", current.Name, current.State, LocalDate(current.StartDate), LocalDate(current.EndDate)))
	} else {
		snap.Notes = append(snap.Notes, fmt.Sprintf("no active or future sprint on board %d matches %q", s.cfg.Board, s.cfg.SprintMatch))
	}

	// 2. issues. Discovery queries, then a refresh of known keys the queries
	//    did not select.
	extra := s.assigneeJQL()
	if strings.TrimSpace(s.cfg.JQL) != "" {
		extra += " AND (" + strings.TrimSpace(s.cfg.JQL) + ")"
	}
	seen := map[string]bool{}
	add := func(is Issue, discovered bool) {
		if seen[is.Key] {
			return
		}
		seen[is.Key] = true
		snap.Items = append(snap.Items, s.item(is, managed, discovered))
	}
	for _, scope := range s.cfg.Scope {
		var jql string
		switch scope {
		case "open":
			jql = "resolution = Unresolved" + extra
		case "sprint":
			if current == nil {
				continue
			}
			jql = "sprint = " + strconv.Itoa(current.ID) + extra
		}
		issues, err := s.client.Search(ctx, jql+" ORDER BY updated DESC", s.sprintField)
		if err != nil {
			return nil, fmt.Errorf("scope %s: %w", scope, err)
		}
		snap.Notes = append(snap.Notes, fmt.Sprintf("%s: %d issue(s) — %s", scope, len(issues), jql))
		for _, is := range issues {
			add(is, true)
		}
	}
	var refresh []string
	for _, k := range known {
		if !seen[k] {
			refresh = append(refresh, k)
		}
	}
	sort.Strings(refresh)
	for i := 0; i < len(refresh); i += 100 {
		chunk := refresh[i:min(i+100, len(refresh))]
		issues, err := s.client.Search(ctx, "key in ("+JQLKeys(chunk)+")", s.sprintField)
		if err != nil {
			// a deleted or moved ticket makes the whole IN query fail; report, keep going
			snap.Notes = append(snap.Notes, "refresh: "+err.Error())
			continue
		}
		for _, is := range issues {
			add(is, false)
		}
	}
	if len(refresh) > 0 {
		snap.Notes = append(snap.Notes, fmt.Sprintf("refreshed %d known key(s) outside the queries", len(refresh)))
	}
	return snap, nil
}

// item maps an issue to a snapshot item.
func (s *Source) item(is Issue, managed map[string]Sprint, discovered bool) source.Item {
	it := source.Item{
		Key:        is.Key,
		Desc:       is.Summary,
		Category:   s.cfg.Category,
		Closed:     is.Category == "done",
		Discovered: discovered,
		Ref: store.Ref{
			Type: "jira", Key: is.Key,
			URL:    strings.TrimRight(s.cfg.BaseURL, "/") + "/browse/" + is.Key,
			Title:  is.Summary,
			Status: is.Status,
		},
	}
	if is.Assignee.AccountID != "" && s.me != nil && is.Assignee.AccountID != s.me.AccountID {
		it.Owner = is.Assignee.DisplayName
	}
	for _, sp := range is.Sprints {
		if _, ok := managed[sp.Name]; ok && sp.State != "closed" {
			it.Groups = append(it.Groups, sp.Name)
		}
	}
	if len(it.Groups) == 0 && s.cfg.BacklogGroup != "" && !it.Closed {
		it.Groups = []string{s.cfg.BacklogGroup}
	}
	if it.Groups == nil {
		it.Groups = []string{}
	}
	return it
}

// sprintURL is the sprint's external reference. Template placeholders:
// {base} {project} {board} {sprint}. Default: the board for an active
// sprint, the backlog for a future one, the sprint report for a closed one
// (Jira Cloud, company-managed projects).
func (s *Source) sprintURL(sp Sprint) string {
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	project := ""
	if s.board != nil {
		project = s.board.Location.ProjectKey
	}
	tpl := s.cfg.GroupURL
	if tpl == "" {
		if project == "" {
			tpl = "{base}/secure/RapidBoard.jspa?rapidView={board}&sprint={sprint}"
		} else {
			switch sp.State {
			case "future":
				tpl = "{base}/jira/software/c/projects/{project}/boards/{board}/backlog?sprint={sprint}"
			case "closed":
				tpl = "{base}/jira/software/c/projects/{project}/boards/{board}/reports/sprint-retrospective?sprint={sprint}"
			default:
				tpl = "{base}/jira/software/c/projects/{project}/boards/{board}?sprint={sprint}"
			}
		}
	}
	r := strings.NewReplacer("{base}", base, "{project}", project, "{board}", strconv.Itoa(s.cfg.Board), "{sprint}", strconv.Itoa(sp.ID))
	return r.Replace(tpl)
}

// ErrNoSprint is returned by CurrentSprint when none matches.
var ErrNoSprint = errors.New("no current sprint")
