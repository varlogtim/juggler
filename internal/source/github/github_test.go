package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/varlogtim/juggler/internal/source"
	"github.com/varlogtim/juggler/internal/store"
)

// fakeGitHub is just enough of the REST API for the connector: /user,
// /repos/{o}/{r}/pulls?head=owner:branch and /repos/{o}/{r}/pulls/{n}. It
// records every path asked.
func fakeGitHub(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var asked []string
	pr := func(n int, title, state string, draft bool, merged string, head string) map[string]any {
		var mergedAt any
		if merged != "" {
			mergedAt = merged
		}
		return map[string]any{"number": n, "title": title, "state": state, "draft": draft, "merged_at": mergedAt,
			"html_url": "https://ghes.example.com/acme/mono/pull/" + itoa(n), "head": map[string]any{"ref": head}, "base": map[string]any{"ref": "develop"}, "user": map[string]any{"login": "me"}}
	}
	byBranch := map[string][]map[string]any{
		"acme:feat/one":  {pr(7, "feature one", "open", false, "", "feat/one")},
		"acme:feat/two":  {pr(9, "two, second try", "open", true, "", "feat/two"), pr(8, "two, first try", "closed", false, "", "feat/two")},
		"acme:feat/none": {},
	}
	byNumber := map[string]map[string]any{
		"/repos/acme/mono/pulls/7":   pr(7, "feature one", "open", false, "", "feat/one"),
		"/repos/acme/other/pulls/42": pr(42, "backport", "closed", false, "2026-10-01T10:00:00Z", "backport/x"),
	}
	byNumber["/repos/acme/other/pulls/42"]["html_url"] = "https://ghes.example.com/acme/other/pull/42"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("Authorization") != "token tok-gh" {
			w.WriteHeader(401)
			w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/user":
			json.NewEncoder(w).Encode(map[string]any{"login": "me"})
		case r.URL.Path == "/api/v3/repos/acme/mono/pulls":
			if r.URL.Query().Get("state") != "all" {
				t.Errorf("list without state=all: %s", r.URL)
			}
			list, ok := byBranch[r.URL.Query().Get("head")]
			if !ok {
				t.Errorf("unexpected head %q", r.URL.Query().Get("head"))
			}
			if list == nil {
				list = []map[string]any{}
			}
			json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(r.URL.Path, "/api/v3/repos/") && strings.Contains(r.URL.Path, "/pulls/"):
			p, ok := byNumber[strings.TrimPrefix(r.URL.Path, "/api/v3")]
			if !ok {
				w.WriteHeader(404)
				w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			json.NewEncoder(w).Encode(p)
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"no route"}`))
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &asked
}

func itoa(n int) string { return strconv.Itoa(n) }

func openTestSource(t *testing.T, ts *httptest.Server, extra map[string]any) source.Source {
	t.Helper()
	cfg := map[string]any{"kind": "github", "base_url": "https://ghes.example.com", "api_url": ts.URL + "/api/v3", "token": "tok-gh"}
	for k, v := range extra {
		cfg[k] = v
	}
	decode := func(into any) error {
		c := into.(*Config)
		for k, v := range cfg {
			switch k {
			case "base_url":
				c.BaseURL = v.(string)
			case "api_url":
				c.APIURL = v.(string)
			case "token":
				c.Token = v.(string)
			case "poll":
				c.Poll = v.(string)
			case "repos":
				c.Repos = v.([]string)
			}
		}
		return nil
	}
	src, err := source.Open("github", "gh", decode, source.Env{Secret: func(ref string) (string, error) { return ref, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func view(name, id, branch, remote, def string, refs ...store.Ref) source.WorkstreamView {
	return source.WorkstreamView{Name: name, ID: id, Branch: branch, Remote: remote, DefaultBranch: def, Refs: refs}
}

func TestPullRefs(t *testing.T) {
	ts, asked := fakeGitHub(t)
	src := openTestSource(t, ts, nil)
	inv := source.Inventory{Workstreams: []source.WorkstreamView{
		// its branch has one PR: refreshed from the list, no per-number call
		view("work_A-1_one", "A-1", "feat/one", "git@ghes.example.com:acme/mono.git", "develop",
			store.Ref{Type: "pr", Key: "acme/mono#7", URL: "https://ghes.example.com/acme/mono/pull/7", Status: "draft"}),
		// no pr ref yet: both PRs of the branch are discovered
		view("work_A-2_two", "A-2", "feat/two", "ssh://git@ghes.example.com/acme/mono.git", "develop"),
		// a shared main checkout on the default branch: not looked up
		view("work_A-3_main", "A-3", "develop", "https://ghes.example.com/acme/mono", "develop"),
		// default branch unknown, usual name: not looked up either
		view("personal_x_notes", "x", "main", "https://ghes.example.com/acme/mono", ""),
		// a checkout on another host: ignored, but its ref on this host is refreshed by number
		view("work_A-4_else", "A-4", "feat/else", "git@github.com:someone/else.git", "",
			store.Ref{Type: "pr", Key: "acme/other#42", URL: "https://ghes.example.com/acme/other/pull/42", Status: "open"},
			store.Ref{Type: "pr", Key: "someone/else#1", URL: "https://github.com/someone/else/pull/1", Status: "open"}),
		// branch with no PR: nothing
		view("work_A-5_none", "A-5", "feat/none", "git@ghes.example.com:acme/mono.git", "develop"),
	}}
	snap, err := src.Pull(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]source.RefUpdate{}
	for _, r := range snap.Refs {
		got[r.Workstream+" "+r.Ref.Key] = r
	}
	if len(snap.Refs) != 4 {
		t.Fatalf("refs: %+v", snap.Refs)
	}
	if r := got["work_A-1_one acme/mono#7"]; r.Ref.Status != "open" || r.Ref.Title != "feature one" || r.Closed || r.Ref.URL != "https://ghes.example.com/acme/mono/pull/7" {
		t.Fatalf("A-1: %+v", r)
	}
	if r := got["work_A-2_two acme/mono#9"]; r.Ref.Status != "draft" || r.Closed {
		t.Fatalf("A-2 #9: %+v", r)
	}
	if r := got["work_A-2_two acme/mono#8"]; r.Ref.Status != "closed" || !r.Closed {
		t.Fatalf("A-2 #8: %+v", r)
	}
	if r := got["work_A-4_else acme/other#42"]; r.Ref.Status != "merged" || !r.Closed || r.Ref.Title != "backport" {
		t.Fatalf("A-4: %+v", r)
	}
	if len(snap.Items) != 0 || len(snap.Groups) != 0 {
		t.Fatalf("a ref connector produced items/groups: %+v", snap)
	}
	// request discipline: one list per branch looked up, one GET per uncovered ref, no GET for #7
	paths := strings.Join(*asked, "\n")
	for _, want := range []string{"/api/v3/user?", "pulls?", "head=acme%3Afeat%2Fone", "head=acme%3Afeat%2Ftwo", "head=acme%3Afeat%2Fnone", "/repos/acme/other/pulls/42?"} {
		if !strings.Contains(paths, want) {
			t.Errorf("missing request %q in:\n%s", want, paths)
		}
	}
	for _, dont := range []string{"head=acme%3Adevelop", "head=acme%3Amain", "/repos/acme/mono/pulls/7?", "someone"} {
		if strings.Contains(paths, dont) {
			t.Errorf("unwanted request %q in:\n%s", dont, paths)
		}
	}
	if !strings.Contains(snap.Notes[0], "as me: 3 pull request(s) for 3 branch(es), 1 by ref") {
		t.Fatalf("note: %q", snap.Notes[0])
	}
	if d := src.Describe(); !strings.Contains(d, "ghes.example.com") || !strings.Contains(d, "pr refs") {
		t.Fatalf("describe: %q", d)
	}

	// repos = [...] narrows the branch lookups; a 404 ref is skipped with a note, not an error
	src = openTestSource(t, ts, map[string]any{"repos": []string{"acme/other"}})
	inv.Workstreams = append(inv.Workstreams, view("work_A-6_gone", "A-6", "feat/gone", "git@ghes.example.com:acme/mono.git", "develop",
		store.Ref{Type: "pr", Key: "acme/mono#404", URL: "https://ghes.example.com/acme/mono/pull/404"}))
	*asked = nil
	snap, err = src.Pull(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(*asked, "\n"), "head=") {
		t.Fatalf("branch lookups despite repos filter: %v", *asked)
	}
	if len(snap.Refs) != 2 { // #7 and #42 by ref
		t.Fatalf("refs with filter: %+v", snap.Refs)
	}
	if n := strings.Join(snap.Notes, "\n"); !strings.Contains(n, "skipped A-6 (acme/mono#404): github /repos/acme/mono/pulls/404: HTTP 404") {
		t.Fatalf("notes: %s", n)
	}
}

func TestOpenValidation(t *testing.T) {
	decode := func(c Config) func(any) error {
		return func(into any) error { *(into.(*Config)) = c; return nil }
	}
	env := source.Env{Secret: func(s string) (string, error) { return s, nil }}
	if _, err := source.Open("github", "x", decode(Config{}), env); err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Fatalf("empty config: %v", err)
	}
	if _, err := source.Open("github", "x", decode(Config{Token: "t", BaseURL: "ghes", Repos: []string{"nope"}, Poll: "soon"}), env); err == nil ||
		!strings.Contains(err.Error(), "base_url") || !strings.Contains(err.Error(), `"nope" is not owner/repo`) || !strings.Contains(err.Error(), "poll") {
		t.Fatalf("bad config: %v", err)
	}
	s, err := source.Open("github", "x", decode(Config{Token: "t", BaseURL: "https://github.com"}), env)
	if err != nil {
		t.Fatal(err)
	}
	if gs := s.(*Source); gs.client.Base != "https://api.github.com" || gs.host != "github.com" || gs.Poll().Hours() != 1 {
		t.Fatalf("github.com defaults: base=%s host=%s poll=%s", gs.client.Base, gs.host, gs.Poll())
	}
	s, _ = source.Open("github", "x", decode(Config{Token: "t", BaseURL: "https://ghes.example.com/"}), env)
	if gs := s.(*Source); gs.client.Base != "https://ghes.example.com/api/v3" {
		t.Fatalf("ghes api base: %s", gs.client.Base)
	}
	// remote URL forms
	gs := s.(*Source)
	for remote, want := range map[string]string{
		"git@ghes.example.com:acme/mono.git":       "acme/mono",
		"ssh://git@ghes.example.com/acme/mono.git": "acme/mono",
		"https://ghes.example.com/acme/mono":       "acme/mono",
		"https://ghes.example.com/acme/mono.git/":  "acme/mono",
		"git@github.com:acme/mono.git":             "",
		"https://ghes.example.com/acme/mono/sub":   "",
		"/local/path/mono":                         "",
		"":                                         "",
		"ghes.example.com:acme/mono.git":           "acme/mono",
	} {
		if got := gs.repoOf(remote); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", remote, got, want)
		}
	}
}
