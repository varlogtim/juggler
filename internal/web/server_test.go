package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varlogtim/juggler/internal/app"
	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/sway"
)

// newTestServer returns a server over a temp store with NO compositor:
// every sway-backed endpoint must answer 503, everything else must work.
func newTestServer(t *testing.T) (*httptest.Server, *app.App) {
	t.Helper()
	cfg := config.Default()
	cfg.Root = filepath.Join(t.TempDir(), "workstreams")
	cfg.StateDir = filepath.Join(t.TempDir(), "state")
	cfg.IDPrefix = "tst"
	cfg.JiraBaseURL = "https://jira.example.com"
	cfg.Repos = nil
	a := app.New(cfg)
	a.DialFunc = func() (*sway.Conn, error) { return nil, errors.New("no sway in tests") }
	srv := New(a)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, a
}

type resp struct {
	code int
	body map[string]any
	list []map[string]any
	raw  string
}

func call(t *testing.T, ts *httptest.Server, method, path string, body any) resp {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(res.Body)
	r := resp{code: res.StatusCode, raw: buf.String()}
	trim := strings.TrimSpace(r.raw)
	if strings.HasPrefix(trim, "{") {
		_ = json.Unmarshal(buf.Bytes(), &r.body)
	} else if strings.HasPrefix(trim, "[") {
		_ = json.Unmarshal(buf.Bytes(), &r.list)
	}
	return r
}

func want(t *testing.T, r resp, code int) resp {
	t.Helper()
	if r.code != code {
		t.Fatalf("want %d, got %d: %s", code, r.code, r.raw)
	}
	return r
}

func TestCRUDWithoutSway(t *testing.T) {
	ts, _ := newTestServer(t)

	// health / status / config / repos / doctor work without sway
	want(t, call(t, ts, "GET", "/api/v1/health", nil), 200)
	st := want(t, call(t, ts, "GET", "/api/v1/status", nil), 200)
	if st.body["sway"] != false {
		t.Fatalf("status.sway should be false: %v", st.body)
	}
	cfg := want(t, call(t, ts, "GET", "/api/v1/config", nil), 200)
	if cfg.body["id_prefix"] != "tst" {
		t.Fatalf("config: %v", cfg.body)
	}
	want(t, call(t, ts, "GET", "/api/v1/repos", nil), 200)
	want(t, call(t, ts, "GET", "/api/v1/doctor", nil), 200)

	// empty list
	if r := want(t, call(t, ts, "GET", "/api/v1/workstreams", nil), 200); len(r.list) != 0 {
		t.Fatalf("expected empty list, got %s", r.raw)
	}

	// create: ticket => work, id = key, jira ref
	r := want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": "first thing", "jira": "abc-12"}), 201)
	ws := r.body["workstream"].(map[string]any)
	if ws["name"] != "work_ABC-12_first-thing" || ws["category"] != "work" || ws["id"] != "ABC-12" || ws["state"] != "none" {
		t.Fatalf("created: %v", ws)
	}
	refs := ws["refs"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["url"] != "https://jira.example.com/browse/ABC-12" {
		t.Fatalf("refs: %v", refs)
	}
	// create: plain => personal, counter id
	r = want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": "Just notes"}), 201)
	if n := r.body["workstream"].(map[string]any)["name"]; n != "personal_tst-0001_just-notes" {
		t.Fatalf("name %v", n)
	}
	// create: bad input
	want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": ""}), 400)
	want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": "dup", "jira": "ABC-12"}), 400) // id/dir exists
	want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": "x", "code_dir": "/nonexistent/dir"}), 400)
	want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": "x", "code_dir": "/tmp", "repo": "r"}), 400)
	want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": "x", "subdir": "components/x"}), 400) // subdir without repo
	// subdir on a workstream with no checkout to move within
	want(t, call(t, ts, "PATCH", "/api/v1/workstreams/ABC-12", map[string]any{"subdir": "components/x"}), 400)

	// list + get (by name, by id, by prefix); 404 for unknown
	if r := want(t, call(t, ts, "GET", "/api/v1/workstreams", nil), 200); len(r.list) != 2 {
		t.Fatalf("list: %s", r.raw)
	}
	want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12", nil), 200)
	want(t, call(t, ts, "GET", "/api/v1/workstreams/work_ABC-12_first-thing", nil), 200)
	want(t, call(t, ts, "GET", "/api/v1/workstreams/tst-00", nil), 200)
	want(t, call(t, ts, "GET", "/api/v1/workstreams/nope", nil), 404)

	// refs
	r = want(t, call(t, ts, "POST", "/api/v1/workstreams/ABC-12/refs", map[string]any{"type": "pr", "value": "https://github.example.com/o/r/pull/7", "status": "open"}), 201)
	if r.body["key"] != "o/r#7" {
		t.Fatalf("pr key: %v", r.body)
	}
	want(t, call(t, ts, "POST", "/api/v1/workstreams/ABC-12/refs", map[string]any{"type": "url", "value": "not a url"}), 400)
	if r := want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12/refs", nil), 200); len(r.list) != 2 {
		t.Fatalf("refs: %s", r.raw)
	}
	want(t, call(t, ts, "DELETE", "/api/v1/workstreams/ABC-12/refs/pr?key=o/r%237", nil), 200)
	want(t, call(t, ts, "DELETE", "/api/v1/workstreams/ABC-12/refs/pr", nil), 404)
	if r := want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12/refs", nil), 200); len(r.list) != 1 {
		t.Fatalf("refs after delete: %s", r.raw)
	}

	// todo
	r = want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12/todo", nil), 200)
	if r.body["open"].(float64) != 0 || !strings.HasPrefix(r.body["text"].(string), "# ABC-12 — ") {
		t.Fatalf("default todo should be a heading with no items: %v", r.body)
	}
	want(t, call(t, ts, "PUT", "/api/v1/workstreams/ABC-12/todo", map[string]any{"text": "- [x] done\n- [ ] one left\n"}), 200)
	r = want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12/todo", nil), 200)
	if r.body["open"].(float64) != 1 || !strings.Contains(r.body["text"].(string), "one left") {
		t.Fatalf("todo after put: %v", r.body)
	}
	want(t, call(t, ts, "PUT", "/api/v1/workstreams/ABC-12/todo", map[string]any{}), 400)

	// env, plan
	r = want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12/env", nil), 200)
	if r.body["JUG_WORKSTREAM_ID"] != "ABC-12" {
		t.Fatalf("env: %v", r.body)
	}
	r = want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12/plan", nil), 200)
	if r.body["has_worktree"] != false {
		t.Fatalf("plan: %v", r.body)
	}

	// set (rename) works without sway: the directory moves
	r = want(t, call(t, ts, "PATCH", "/api/v1/workstreams/tst-0001", map[string]any{"desc": "renamed notes", "category": "work"}), 200)
	res := r.body["result"].(map[string]any)
	if res["moved"] != true || res["name"] != "work_tst-0001_renamed-notes" {
		t.Fatalf("set: %v", res)
	}
	want(t, call(t, ts, "GET", "/api/v1/workstreams/work_tst-0001_renamed-notes", nil), 200)
	// set: attach a ticket => id follows
	r = want(t, call(t, ts, "PATCH", "/api/v1/workstreams/tst-0001", map[string]any{"jira": "xyz-9"}), 200)
	if r.body["workstream"].(map[string]any)["id"] != "XYZ-9" {
		t.Fatalf("set jira: %v", r.body)
	}
	want(t, call(t, ts, "PATCH", "/api/v1/workstreams/XYZ-9", map[string]any{"desc": "first thing", "id": "ABC-12", "category": "work"}), 400) // target exists

	// sway-backed endpoints: 503 with a code, never 500
	for _, p := range []string{"/api/v1/workstreams/ABC-12/show", "/api/v1/workstreams/ABC-12/term", "/api/v1/workstreams/ABC-12/focus", "/api/v1/workstreams/ABC-12/close", "/api/v1/slot/toggle", "/api/v1/slot/park", "/api/v1/lot"} {
		r := call(t, ts, "POST", p, map[string]any{"what": "jira"})
		if r.code != 503 || r.body["code"] != "sway_unavailable" {
			t.Fatalf("%s: want 503 sway_unavailable, got %d %s", p, r.code, r.raw)
		}
	}
	r = call(t, ts, "POST", "/api/v1/workstreams/ABC-12/open", map[string]any{})
	want(t, r, 400)

	// session: no pin => 200 with pinned ""
	r = want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12/session", nil), 200)
	if r.body["pinned"] != "" {
		t.Fatalf("session: %v", r.body)
	}
	want(t, call(t, ts, "PUT", "/api/v1/workstreams/ABC-12/session", map[string]any{}), 400)
	want(t, call(t, ts, "PUT", "/api/v1/workstreams/ABC-12/session", map[string]any{"id": "ses_abc"}), 200)
	r = want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12", nil), 200)
	if r.body["opencode_session"] != "ses_abc" {
		t.Fatalf("pin: %v", r.body["opencode_session"])
	}
	want(t, call(t, ts, "DELETE", "/api/v1/workstreams/ABC-12/session", nil), 200)

	// complete / reopen (juggler-only finished mark)
	r = want(t, call(t, ts, "POST", "/api/v1/workstreams/ABC-12/complete", nil), 200)
	if r.body["finished"] != true || r.body["completed"] == "" || r.body["ticket_closed"] != false {
		t.Fatalf("complete: %s", r.raw)
	}
	r = want(t, call(t, ts, "POST", "/api/v1/workstreams/ABC-12/reopen", nil), 200)
	if r.body["finished"] != false {
		t.Fatalf("reopen: %s", r.raw)
	}
	want(t, call(t, ts, "POST", "/api/v1/workstreams/nope/complete", nil), 404)

	// delete: without sway the files still go
	want(t, call(t, ts, "DELETE", "/api/v1/workstreams/ABC-12", nil), 200)
	want(t, call(t, ts, "GET", "/api/v1/workstreams/ABC-12", nil), 404)
	if r := want(t, call(t, ts, "GET", "/api/v1/workstreams", nil), 200); len(r.list) != 1 {
		t.Fatalf("list after delete: %s", r.raw)
	}
}

func TestGroups(t *testing.T) {
	ts, _ := newTestServer(t)
	want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": "a", "jira": "G-1", "groups": []string{"s20"}}), 201)
	want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": "b", "jira": "G-2"}), 201)
	want(t, call(t, ts, "POST", "/api/v1/workstreams", map[string]any{"desc": "c", "groups": []string{"bad/name"}}), 400)

	// a tagged-but-unregistered group exists
	r := want(t, call(t, ts, "GET", "/api/v1/groups", nil), 200)
	if len(r.list) != 1 || r.list[0]["name"] != "s20" || r.list[0]["registered"] != false || r.list[0]["count"].(float64) != 1 {
		t.Fatalf("groups: %s", r.raw)
	}
	// register metadata; bad dates rejected
	want(t, call(t, ts, "PUT", "/api/v1/groups/s20", map[string]any{"kind": "sprint", "start": "2026-09-23", "end": "2026-10-06"}), 200)
	want(t, call(t, ts, "PUT", "/api/v1/groups/s20", map[string]any{"end": "nope"}), 400)
	want(t, call(t, ts, "PUT", "/api/v1/groups/bad%2Fname", map[string]any{}), 400)
	// registered, empty: hidden unless all=true
	want(t, call(t, ts, "PUT", "/api/v1/groups/s21", map[string]any{"start": "2026-10-07", "end": "2026-10-20"}), 200)
	if r := want(t, call(t, ts, "GET", "/api/v1/groups", nil), 200); len(r.list) != 1 {
		t.Fatalf("empty group shown: %s", r.raw)
	}
	if r := want(t, call(t, ts, "GET", "/api/v1/groups?all=true", nil), 200); len(r.list) != 2 || r.list[0]["name"] != "s20" || r.list[1]["name"] != "s21" {
		t.Fatalf("all groups / order: %s", r.raw)
	}
	// membership endpoints
	r = want(t, call(t, ts, "POST", "/api/v1/groups/s21/members", map[string]any{"workstreams": []string{"G-2", "G-1"}}), 200)
	if r.body["count"].(float64) != 2 {
		t.Fatalf("add members: %s", r.raw)
	}
	want(t, call(t, ts, "POST", "/api/v1/groups/s21/members", map[string]any{"workstreams": []string{"nope"}}), 404)
	want(t, call(t, ts, "POST", "/api/v1/groups/s21/members", map[string]any{}), 400)
	want(t, call(t, ts, "DELETE", "/api/v1/groups/s21/members/G-2", nil), 200)
	want(t, call(t, ts, "DELETE", "/api/v1/groups/s21/members/G-2", nil), 404)
	if r := want(t, call(t, ts, "GET", "/api/v1/workstreams?group=s21", nil), 200); len(r.list) != 1 || r.list[0]["id"] != "G-1" {
		t.Fatalf("filter: %s", r.raw)
	}
	// PATCH groups replaces tags; other fields untouched
	r = want(t, call(t, ts, "PATCH", "/api/v1/workstreams/G-1", map[string]any{"groups": []string{"backlog"}}), 200)
	if g := r.body["workstream"].(map[string]any)["groups"].([]any); len(g) != 1 || g[0] != "backlog" {
		t.Fatalf("patch groups: %s", r.raw)
	}
	if r := want(t, call(t, ts, "GET", "/api/v1/groups/s21", nil), 200); r.body["count"].(float64) != 0 {
		t.Fatalf("s21 should be empty now: %s", r.raw)
	}
	want(t, call(t, ts, "GET", "/api/v1/groups/nope", nil), 404)
	// delete: registry only, then with untag
	want(t, call(t, ts, "DELETE", "/api/v1/groups/s21", nil), 200)
	want(t, call(t, ts, "DELETE", "/api/v1/groups/s21", nil), 404)
	r = want(t, call(t, ts, "DELETE", "/api/v1/groups/backlog?untag=true", nil), 200)
	if r.body["untagged"].(float64) != 1 {
		t.Fatalf("untag: %s", r.raw)
	}
	if r := want(t, call(t, ts, "GET", "/api/v1/workstreams/G-1", nil), 200); len(r.body["groups"].([]any)) != 0 {
		t.Fatalf("G-1 still tagged: %s", r.raw)
	}
}

func TestSourcesWithoutAnyConfigured(t *testing.T) {
	ts, _ := newTestServer(t)
	if r := want(t, call(t, ts, "GET", "/api/v1/sources", nil), 200); len(r.list) != 0 {
		t.Fatalf("sources: %s", r.raw)
	}
	want(t, call(t, ts, "GET", "/api/v1/sources/nope", nil), 404)
	want(t, call(t, ts, "POST", "/api/v1/sources/nope/sync", nil), 404)
	r := want(t, call(t, ts, "POST", "/api/v1/sync", nil), 200)
	if len(r.body["reports"].([]any)) != 0 {
		t.Fatalf("sync all with no sources: %s", r.raw)
	}
	want(t, call(t, ts, "POST", "/api/v1/sources/nope/prune?dry_run=true", nil), 404)
	// relaunching opencode windows needs sway
	want(t, call(t, ts, "POST", "/api/v1/relaunch", nil), 503)
	want(t, call(t, ts, "POST", "/api/v1/relaunch", map[string]any{"workstreams": []string{"ABC-12"}}), 503)
	want(t, call(t, ts, "POST", "/api/v1/sources/nope/forgive", map[string]any{}), 400)
	want(t, call(t, ts, "POST", "/api/v1/sources/nope/forgive", map[string]any{"keys": []string{"X-1"}}), 404)
	// group url round-trips through the API
	want(t, call(t, ts, "PUT", "/api/v1/groups/s1", map[string]any{"url": "https://jira.example.com/boards/1"}), 200)
	want(t, call(t, ts, "PUT", "/api/v1/groups/s1", map[string]any{"url": "nope"}), 400)
	if r := want(t, call(t, ts, "GET", "/api/v1/groups/s1", nil), 200); r.body["url"] != "https://jira.example.com/boards/1" {
		t.Fatalf("group url: %s", r.raw)
	}
}

func TestUIAndEvents(t *testing.T) {
	ts, _ := newTestServer(t)
	r := want(t, call(t, ts, "GET", "/", nil), 200)
	if !strings.Contains(r.raw, "<title>juggler</title>") || !strings.Contains(r.raw, "/ui/app.js") {
		t.Fatalf("index.html not served: %.200s", r.raw)
	}
	want(t, call(t, ts, "GET", "/ui/app.js", nil), 200)
	want(t, call(t, ts, "GET", "/ui/style.css", nil), 200)
	want(t, call(t, ts, "GET", "/ui/nope.js", nil), 404)
	want(t, call(t, ts, "GET", "/api/v1/nope", nil), 404)

	// SSE: the hello event arrives immediately
	res, err := http.Get(ts.URL + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	buf := make([]byte, 256)
	n, _ := res.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "event: hello") {
		t.Fatalf("no hello event: %q", buf[:n])
	}
}

func TestRefuseNonLoopback(t *testing.T) {
	_, a := newTestServer(t)
	os.Unsetenv("JUG_SERVE_ANY")
	err := New(a).ListenAndServe("0.0.0.0:0")
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected refusal, got %v", err)
	}
}
