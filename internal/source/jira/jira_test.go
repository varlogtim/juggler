package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/varlogtim/juggler/internal/source"
)

// fakeJira is just enough of Jira Cloud for the connector: myself, field,
// board, board sprints, and /search/jql with one-page-token pagination.
func fakeJira(t *testing.T) *httptest.Server {
	t.Helper()
	me := map[string]any{"accountId": "acc-me", "displayName": "Me Myself", "emailAddress": "me@example.com"}
	other := map[string]any{"accountId": "acc-other", "displayName": "Pat Teammate"}
	sprintActive := map[string]any{"id": 20, "state": "active", "name": "PCFS-S20-Nebula", "startDate": "2026-09-23T18:00:03.000Z", "endDate": "2026-10-06T17:00:00.000Z", "goal": "ship it"}
	sprintFuture := map[string]any{"id": 21, "state": "future", "name": "PCFS-S21-Nebula", "startDate": "2026-10-07T04:00:00.000Z", "endDate": "2026-10-20T17:00:00.000Z"}
	sprintClosed := map[string]any{"id": 19, "state": "closed", "name": "PCFS-S19-Nebula", "startDate": "2026-09-09T04:00:00.000Z", "endDate": "2026-09-22T17:00:00.000Z"}
	otherTeam := map[string]any{"id": 77, "state": "active", "name": "PCFS-S20-CosmicLeap"}
	issue := func(key, summary, status, cat string, assignee map[string]any, sprints ...map[string]any) map[string]any {
		f := map[string]any{"summary": summary, "status": map[string]any{"name": status, "statusCategory": map[string]any{"key": cat}}, "assignee": assignee, "updated": "2026-10-01T09:28:00.007-0700", "issuetype": map[string]any{"name": "Bug"}, "priority": map[string]any{"name": "P2"}}
		if sprints != nil {
			f["customfield_10020"] = sprints
		}
		return map[string]any{"key": key, "fields": f}
	}
	mine := []any{issue("P-1", "my sprint ticket", "In Progress", "indeterminate", me, sprintActive), issue("P-2", "my backlog ticket", "New", "new", me), issue("P-3", "carried from s19", "Blocked", "indeterminate", me, sprintClosed)}
	sprint := []any{issue("P-1", "my sprint ticket", "In Progress", "indeterminate", me, sprintActive), issue("P-4", "pat's ticket", "Code Review", "indeterminate", other, sprintActive), issue("P-5", "done in sprint", "Closed", "done", other, sprintActive), issue("P-6", "future planned", "New", "new", me, sprintFuture, sprintActive)}
	known := map[string]any{"P-9": issue("P-9", "old finished one", "Closed", "done", me, sprintClosed)}
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/2/myself", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "me@example.com" || p != "tok-123" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(me)
	})
	mux.HandleFunc("/rest/api/2/field", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{map[string]any{"id": "customfield_99", "name": "Rank"}, map[string]any{"id": "customfield_10020", "name": "Sprint", "schema": map[string]any{"custom": "com.pyxis.greenhopper.jira:gh-sprint"}}})
	})
	mux.HandleFunc("/rest/agile/1.0/board/2112", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": 2112, "name": "Team Nebula Board", "type": "scrum", "location": map[string]any{"projectKey": "AISW"}})
	})
	mux.HandleFunc("/rest/agile/1.0/board/2112/sprint", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "active,future" {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"values": []any{sprintActive, sprintFuture, otherTeam}, "isLast": true})
	})
	mux.HandleFunc("/rest/api/2/search/jql", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		jql := q.Get("jql")
		if !strings.Contains(q.Get("fields"), "customfield_10020") {
			w.WriteHeader(400)
			w.Write([]byte("fields missing sprint field"))
			return
		}
		var page []any
		var out map[string]any
		onlyMine := strings.Contains(jql, "assignee = currentUser()")
		filterMine := func(in []any) []any {
			if !onlyMine {
				return in
			}
			var out []any
			for _, i := range in {
				if a, _ := i.(map[string]any)["fields"].(map[string]any)["assignee"].(map[string]any); a != nil && a["accountId"] == "acc-me" {
					out = append(out, i)
				}
			}
			return out
		}
		switch {
		case strings.HasPrefix(jql, "resolution = Unresolved"):
			// two pages, to exercise nextPageToken
			if q.Get("nextPageToken") == "" {
				out = map[string]any{"issues": filterMine(mine[:2]), "isLast": false, "nextPageToken": "tok2"}
			} else {
				out = map[string]any{"issues": filterMine(mine[2:]), "isLast": true}
			}
		case strings.HasPrefix(jql, "sprint = 20"):
			out = map[string]any{"issues": filterMine(sprint), "isLast": true}
		case strings.HasPrefix(jql, "key in ("):
			for k, v := range known {
				if strings.Contains(jql, `"`+k+`"`) {
					page = append(page, v)
				}
			}
			out = map[string]any{"issues": page, "isLast": true}
		default:
			w.WriteHeader(400)
			w.Write([]byte("unexpected jql: " + jql))
			return
		}
		json.NewEncoder(w).Encode(out)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func openTestSource(t *testing.T, ts *httptest.Server, extra map[string]any) source.Source {
	t.Helper()
	cfg := map[string]any{"kind": "jira", "base_url": ts.URL, "email": "me@example.com", "token": "tok-123", "board": 2112, "sprint_match": "Nebula"}
	for k, v := range extra {
		cfg[k] = v
	}
	decode := func(into any) error {
		b, _ := json.Marshal(cfg)
		return jsonInto(b, into)
	}
	src, err := source.Open("jira", "nebula", decode, source.Env{DefaultCategory: "personal", Secret: func(ref string) (string, error) { return ref, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// jsonInto decodes JSON through the toml tags by matching keys (test-only
// shim: the real decoder is TOML).
func jsonInto(b []byte, into any) error {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	c := into.(*Config)
	for k, v := range m {
		switch k {
		case "base_url":
			c.BaseURL = v.(string)
		case "email":
			c.Email = v.(string)
		case "token":
			c.Token = v.(string)
		case "board":
			c.Board = int(v.(float64))
		case "sprint_match":
			c.SprintMatch = v.(string)
		case "scope":
			c.Scope = nil
			for _, s := range v.([]any) {
				c.Scope = append(c.Scope, s.(string))
			}
		case "backlog_group":
			c.BacklogGroup = v.(string)
		case "poll":
			c.Poll = v.(string)
		case "jql":
			c.JQL = v.(string)
		case "assignee":
			c.Assignee = v.(string)
		}
	}
	return nil
}

func TestOpenValidation(t *testing.T) {
	decode := func(into any) error { return nil }
	if _, err := source.Open("jira", "x", decode, source.Env{Secret: func(s string) (string, error) { return s, nil }}); err == nil || !strings.Contains(err.Error(), "email is required") || !strings.Contains(err.Error(), "board") {
		t.Fatalf("validation: %v", err)
	}
	if _, err := source.Open("nope", "x", decode, source.Env{}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	ts := fakeJira(t)
	if _, err := source.Open("jira", "x", func(into any) error {
		return jsonInto([]byte(`{"base_url":"`+ts.URL+`","email":"e","token":"t","board":1,"scope":["open","weird"],"poll":"soon"}`), into)
	}, source.Env{Secret: func(s string) (string, error) { return s, nil }}); err == nil || !strings.Contains(err.Error(), `scope "weird"`) || !strings.Contains(err.Error(), "poll") {
		t.Fatalf("scope/poll validation: %v", err)
	}
}

func TestPull(t *testing.T) {
	ts := fakeJira(t)
	src := openTestSource(t, ts, nil)
	if src.Poll() != time.Hour || src.Kind() != "jira" || src.Name() != "nebula" {
		t.Fatalf("defaults: %v %s %s", src.Poll(), src.Kind(), src.Name())
	}
	// assignee = "any": the old behaviour, everything in the sprint
	src = openTestSource(t, ts, map[string]any{"assignee": "any"})
	snap, err := src.Pull(context.Background(), source.Inventory{Known: []string{"P-1", "P-9", "P-404"}})
	if err != nil {
		t.Fatal(err)
	}
	// groups: the two Nebula sprints (local dates) + backlog; CosmicLeap excluded
	if len(snap.Groups) != 3 {
		t.Fatalf("groups: %+v", snap.Groups)
	}
	s20 := snap.Groups[0]
	if s20.Name != "PCFS-S20-Nebula" || s20.Kind != "sprint" || s20.State != "active" || s20.Source != "nebula" || s20.Desc != "ship it" {
		t.Fatalf("s20: %+v", s20)
	}
	if s20.Start != LocalDate("2026-09-23T18:00:03.000Z") || s20.End != LocalDate("2026-10-06T17:00:00.000Z") {
		t.Fatalf("s20 dates: %s %s", s20.Start, s20.End)
	}
	wantURL := ts.URL + "/jira/software/c/projects/AISW/boards/2112?sprint=20"
	if s20.URL != wantURL {
		t.Fatalf("s20 url %q, want %q", s20.URL, wantURL)
	}
	if !strings.Contains(snap.Groups[1].URL, "/backlog?sprint=21") || snap.Groups[2].Name != "backlog" || snap.Groups[2].Kind != "bucket" {
		t.Fatalf("groups 1/2: %+v %+v", snap.Groups[1], snap.Groups[2])
	}

	byKey := map[string]source.Item{}
	for _, it := range snap.Items {
		byKey[it.Key] = it
	}
	if len(byKey) != 7 { // P-1..P-6 discovered (P-1 deduped), P-9 refreshed; P-404 unknown
		t.Fatalf("items: %d %+v", len(byKey), keys(byKey))
	}
	check := func(key string, groups string, owner string, closed, discovered bool, status string) {
		t.Helper()
		it, ok := byKey[key]
		if !ok {
			t.Fatalf("%s missing", key)
		}
		if strings.Join(it.Groups, ",") != groups || it.Owner != owner || it.Closed != closed || it.Discovered != discovered || it.Ref.Status != status || it.Ref.Type != "jira" || it.Ref.Key != key || !strings.HasSuffix(it.Ref.URL, "/browse/"+key) || it.Category != "work" {
			t.Fatalf("%s: %+v", key, it)
		}
	}
	check("P-1", "PCFS-S20-Nebula", "", false, true, "In Progress")
	check("P-2", "backlog", "", false, true, "New")     // no sprint -> backlog
	check("P-3", "backlog", "", false, true, "Blocked") // only a closed sprint -> backlog
	check("P-4", "PCFS-S20-Nebula", "Pat Teammate", false, true, "Code Review")
	check("P-5", "PCFS-S20-Nebula", "Pat Teammate", true, true, "Closed") // closed, stays in its sprint, no backlog
	check("P-6", "PCFS-S21-Nebula,PCFS-S20-Nebula", "", false, true, "New")
	check("P-9", "", "", true, false, "Closed") // refreshed only; closed -> no backlog
	if byKey["P-1"].Desc != "my sprint ticket" || byKey["P-1"].Ref.Title != "my sprint ticket" {
		t.Fatalf("desc/title: %+v", byKey["P-1"])
	}
	joined := strings.Join(snap.Notes, "\n")
	if !strings.Contains(joined, "current sprint: PCFS-S20-Nebula") || !strings.Contains(joined, "open: 3 issue(s)") || !strings.Contains(joined, "sprint: 4 issue(s)") || !strings.Contains(joined, "refreshed 2 known") {
		t.Fatalf("notes: %s", joined)
	}
	if strings.Contains(joined, "assignee") {
		t.Fatalf("assignee=any must add no assignee clause: %s", joined)
	}

	// default assignee = "me": every scope is restricted to your tickets
	src = openTestSource(t, ts, nil)
	snap, err = src.Pull(context.Background(), source.Inventory{})
	if err != nil {
		t.Fatal(err)
	}
	var ks []string
	for _, it := range snap.Items {
		ks = append(ks, it.Key)
		if it.Owner != "" {
			t.Fatalf("teammate's ticket pulled with assignee=me: %+v", it)
		}
	}
	if strings.Join(ks, ",") != "P-1,P-2,P-3,P-6" {
		t.Fatalf("assignee=me keys: %v", ks)
	}
	if !strings.Contains(strings.Join(snap.Notes, "\n"), "sprint = 20 AND assignee = currentUser()") {
		t.Fatalf("assignee clause missing from the sprint scope: %v", snap.Notes)
	}
	// a named assignee becomes a JQL value; a clause is kept as is
	for in, want := range map[string]string{"pat@example.com": `assignee = "pat@example.com"`, "assignee in (a, b)": "(assignee in (a, b))"} {
		s2 := openTestSource(t, ts, map[string]any{"assignee": in}).(*Source)
		if got := s2.assigneeJQL(); !strings.Contains(got, want) {
			t.Fatalf("assigneeJQL(%q) = %q", in, got)
		}
	}
}

func TestPullScopeMineOnlyAndJQL(t *testing.T) {
	ts := fakeJira(t)
	src := openTestSource(t, ts, map[string]any{"scope": []any{"open"}, "backlog_group": "", "jql": "project = P"})
	// the fake only answers JQL prefixes it knows; the extra filter is appended after
	snap, err := src.Pull(context.Background(), source.Inventory{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 3 {
		t.Fatalf("open only: %d", len(snap.Items))
	}
	for _, it := range snap.Items {
		if it.Key == "P-2" && len(it.Groups) != 0 {
			t.Fatalf("backlog disabled but tagged: %+v", it)
		}
	}
	if len(snap.Groups) != 2 { // no backlog group registered
		t.Fatalf("groups: %+v", snap.Groups)
	}
	if !strings.Contains(strings.Join(snap.Notes, "\n"), "AND (project = P)") {
		t.Fatalf("jql filter not applied: %v", snap.Notes)
	}
}

func TestLocalDate(t *testing.T) {
	if got := LocalDate(""); got != "" {
		t.Fatal(got)
	}
	if got := LocalDate("garbage"); got != "" {
		t.Fatal(got)
	}
	// a date is a date in the local zone, whatever the UTC clock says
	in := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local).UTC().Format(time.RFC3339Nano)
	if got := LocalDate(in); got != "2026-10-06" {
		t.Fatalf("LocalDate(%s) = %s", in, got)
	}
}

func TestJQLKeys(t *testing.T) {
	if got := JQLKeys([]string{"A-1", `B-"2`}); got != `"A-1", "B-2"` {
		t.Fatal(got)
	}
	u, _ := url.Parse("http://x/?a=1")
	_ = u
}

func keys(m map[string]source.Item) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
