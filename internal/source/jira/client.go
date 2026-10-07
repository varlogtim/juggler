package jira

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

// Client is the small slice of the Jira Cloud REST API the connector
// needs: search (REST v2, the /search/jql endpoint — the old /search was
// removed), and the Agile API for boards and sprints. Basic auth with an
// API token.
type Client struct {
	Base  string // https://site.atlassian.net
	Email string
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
	return fmt.Sprintf("jira %s: HTTP %d: %s", e.Path, e.Status, e.Body)
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
	req.SetBasicAuth(c.Email, c.Token)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("jira %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		body := strings.TrimSpace(string(b))
		if len(body) > 300 {
			body = body[:300] + "…"
		}
		return &Error{Status: resp.StatusCode, Path: path, Body: body}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("jira %s: bad JSON: %w", path, err)
	}
	return nil
}

// User is /rest/api/2/myself.
type User struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Email       string `json:"emailAddress"`
}

// Myself returns the authenticated user.
func (c *Client) Myself(ctx context.Context) (User, error) {
	var u User
	return u, c.get(ctx, "/rest/api/2/myself", nil, &u)
}

// Field is one entry of /rest/api/2/field.
type Field struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Schema struct {
		Custom string `json:"custom"`
	} `json:"schema"`
}

// SprintFieldID finds the custom field that holds sprint membership
// (schema com.pyxis.greenhopper.jira:gh-sprint). Instances differ.
func (c *Client) SprintFieldID(ctx context.Context) (string, error) {
	var fields []Field
	if err := c.get(ctx, "/rest/api/2/field", nil, &fields); err != nil {
		return "", err
	}
	for _, f := range fields {
		if f.Schema.Custom == "com.pyxis.greenhopper.jira:gh-sprint" {
			return f.ID, nil
		}
	}
	for _, f := range fields {
		if f.Name == "Sprint" {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("jira: no Sprint field on this instance")
}

// Board is /rest/agile/1.0/board/{id}.
type Board struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // scrum | kanban
	Location struct {
		ProjectKey string `json:"projectKey"`
		Name       string `json:"name"`
	} `json:"location"`
}

// GetBoard fetches a board.
func (c *Client) GetBoard(ctx context.Context, id int) (Board, error) {
	var b Board
	return b, c.get(ctx, "/rest/agile/1.0/board/"+strconv.Itoa(id), nil, &b)
}

// Sprint is one entry of /rest/agile/1.0/board/{id}/sprint (and the sprint
// objects inside an issue's sprint field).
type Sprint struct {
	ID            int    `json:"id"`
	State         string `json:"state"` // future | active | closed
	Name          string `json:"name"`
	StartDate     string `json:"startDate"` // RFC3339 (UTC)
	EndDate       string `json:"endDate"`
	CompleteDate  string `json:"completeDate"`
	Goal          string `json:"goal"`
	OriginBoardID int    `json:"originBoardId"`
	BoardID       int    `json:"boardId"` // on issue sprint fields
}

// LocalDate converts a Jira RFC3339 timestamp to a local YYYY-MM-DD; "" in,
// "" out.
func LocalDate(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		if t, err = time.Parse("2006-01-02T15:04:05.000-0700", ts); err != nil {
			return ""
		}
	}
	return t.In(time.Local).Format("2006-01-02")
}

// Sprints lists a board's sprints in the given states (comma-separated:
// "active,future"), all pages.
func (c *Client) Sprints(ctx context.Context, board int, states string) ([]Sprint, error) {
	var all []Sprint
	start := 0
	for {
		var page struct {
			Values []Sprint `json:"values"`
			IsLast bool     `json:"isLast"`
		}
		q := url.Values{"state": {states}, "startAt": {strconv.Itoa(start)}, "maxResults": {"50"}}
		if err := c.get(ctx, "/rest/agile/1.0/board/"+strconv.Itoa(board)+"/sprint", q, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Values...)
		if page.IsLast || len(page.Values) == 0 {
			return all, nil
		}
		start += len(page.Values)
	}
}

// Issue is the subset of an issue the connector reads. Sprints is decoded
// from the instance's sprint custom field (see SprintFieldID).
type Issue struct {
	Key      string
	Summary  string
	Status   string
	Category string // statusCategory key: new | indeterminate | done
	Assignee User
	Updated  string
	Type     string
	Priority string
	Sprints  []Sprint
}

// Search runs a JQL query (all pages) and decodes the fields the connector
// needs. sprintField is the custom field id for sprints.
func (c *Client) Search(ctx context.Context, jql, sprintField string) ([]Issue, error) {
	fields := "summary,status,assignee,updated,issuetype,priority"
	if sprintField != "" {
		fields += "," + sprintField
	}
	var all []Issue
	token := ""
	for {
		q := url.Values{"jql": {jql}, "fields": {fields}, "maxResults": {"100"}}
		if token != "" {
			q.Set("nextPageToken", token)
		}
		var page struct {
			Issues []struct {
				Key    string                     `json:"key"`
				Fields map[string]json.RawMessage `json:"fields"`
			} `json:"issues"`
			IsLast        bool   `json:"isLast"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := c.get(ctx, "/rest/api/2/search/jql", q, &page); err != nil {
			return nil, err
		}
		for _, raw := range page.Issues {
			is := Issue{Key: raw.Key}
			unmarshal(raw.Fields["summary"], &is.Summary)
			var st struct {
				Name string `json:"name"`
				Cat  struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			}
			unmarshal(raw.Fields["status"], &st)
			is.Status, is.Category = st.Name, st.Cat.Key
			unmarshal(raw.Fields["assignee"], &is.Assignee)
			unmarshal(raw.Fields["updated"], &is.Updated)
			var named struct {
				Name string `json:"name"`
			}
			unmarshal(raw.Fields["issuetype"], &named)
			is.Type = named.Name
			named.Name = ""
			unmarshal(raw.Fields["priority"], &named)
			is.Priority = named.Name
			if sprintField != "" {
				unmarshal(raw.Fields[sprintField], &is.Sprints)
			}
			all = append(all, is)
		}
		if page.IsLast || page.NextPageToken == "" || len(page.Issues) == 0 {
			return all, nil
		}
		token = page.NextPageToken
	}
}

// unmarshal decodes an optional field, treating absent/null/malformed as
// empty: a custom field that is not there is not an error.
func unmarshal(raw json.RawMessage, into any) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	_ = json.Unmarshal(raw, into)
}

// JQLKeys formats keys for `key in (...)`.
func JQLKeys(keys []string) string {
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = `"` + strings.ReplaceAll(k, `"`, ``) + `"`
	}
	return strings.Join(quoted, ", ")
}
