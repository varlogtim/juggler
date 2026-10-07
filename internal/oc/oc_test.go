package oc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fake is a stand-in for an opencode server: records what it was asked and
// answers in the shapes the real one uses (from its /doc OpenAPI spec).
type fake struct {
	mu    sync.Mutex
	calls []string // "METHOD path"
	body  map[string]any
	busy  bool
	perms []Permission
}

func (f *fake) handler() http.Handler {
	m := http.NewServeMux()
	rec := func(r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.body = nil
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&f.body)
		}
	}
	m.HandleFunc("GET /global/health", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_, _ = w.Write([]byte(`{"healthy":true,"version":"1.18.35"}`))
	})
	m.HandleFunc("GET /session/status", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		if f.busy {
			_, _ = w.Write([]byte(`{"ses_1":{"type":"busy"},"ses_2":{"type":"retry","attempt":2,"message":"rate limited","next":1700000000000}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	m.HandleFunc("GET /permission", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_ = json.NewEncoder(w).Encode(f.perms)
	})
	m.HandleFunc("POST /session/{id}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("POST /session/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_, _ = w.Write([]byte(`{"info":{"id":"msg_1","role":"assistant","cost":0.01},"parts":[{"type":"step-start"},{"type":"text","text":"hello"},{"type":"text","text":"world"}]}`))
	})
	m.HandleFunc("POST /tui/append-prompt", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_, _ = w.Write([]byte(`true`))
	})
	m.HandleFunc("POST /tui/show-toast", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_, _ = w.Write([]byte(`true`))
	})
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		http.Error(w, `{"name":"NotFoundError"}`, 404)
	})
	return m
}

func (f *fake) last() (string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return "", nil
	}
	return f.calls[len(f.calls)-1], f.body
}

func newFake(t *testing.T) (*fake, *Client) {
	t.Helper()
	f := &fake{}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	return f, NewClient(ts.URL + "/")
}

func TestClientHealthAndStatus(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	v, err := c.Health(ctx)
	if err != nil || v != "1.18.35" {
		t.Fatalf("health: %v %q", err, v)
	}
	st, err := c.Statuses(ctx)
	if err != nil || len(st) != 0 {
		t.Fatalf("idle statuses: %v %v", err, st)
	}
	f.busy = true
	st, err = c.Statuses(ctx)
	if err != nil || st["ses_1"].Type != "busy" || st["ses_2"].Type != "retry" || st["ses_2"].Message != "rate limited" || st["ses_2"].Attempt != 2 {
		t.Fatalf("busy statuses: %v %+v", err, st)
	}
	if call, _ := f.last(); call != "GET /session/status" {
		t.Fatalf("call %q", call)
	}
}

func TestClientPrompting(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	if err := c.PromptAsync(ctx, "ses_1", "do the thing"); err != nil {
		t.Fatal(err)
	}
	call, body := f.last()
	if call != "POST /session/ses_1/prompt_async" {
		t.Fatalf("call %q", call)
	}
	parts := body["parts"].([]any)
	if p := parts[0].(map[string]any); len(parts) != 1 || p["type"] != "text" || p["text"] != "do the thing" {
		t.Fatalf("body %v", body)
	}
	rep, err := c.Message(ctx, "ses_1", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Text() != "hello\nworld" || rep.Info.ID != "msg_1" || rep.Err() != nil {
		t.Fatalf("reply %+v", rep)
	}
	if call, _ := f.last(); call != "POST /session/ses_1/message" {
		t.Fatalf("call %q", call)
	}
	if err := c.AppendPrompt(ctx, "draft"); err != nil {
		t.Fatal(err)
	}
	if call, body := f.last(); call != "POST /tui/append-prompt" || body["text"] != "draft" {
		t.Fatalf("append: %q %v", call, body)
	}
	if err := c.Toast(ctx, "juggler", "msg", "info"); err != nil {
		t.Fatal(err)
	}
	if call, body := f.last(); call != "POST /tui/show-toast" || body["variant"] != "info" || body["title"] != "juggler" {
		t.Fatalf("toast: %q %v", call, body)
	}
	// an HTTP error carries the body
	if err := c.do(ctx, "GET", "/nope", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "NotFoundError") {
		t.Fatalf("error shape: %v", err)
	}
}

func TestPermissionsAndSummary(t *testing.T) {
	f, c := newFake(t)
	f.perms = []Permission{
		{ID: "per_1", SessionID: "ses_1", Permission: "bash", Patterns: []string{"git push *"}},
		{ID: "per_2", SessionID: "ses_2", Permission: "external_directory", Metadata: map[string]any{"command": "cat ~/x"}},
		{ID: "per_3", SessionID: "ses_3", Tool: "edit"},
	}
	got, err := c.Permissions(context.Background())
	if err != nil || len(got) != 3 {
		t.Fatalf("permissions: %v %v", err, got)
	}
	for i, want := range []string{"bash git push *", "external_directory cat ~/x", "edit"} {
		if s := got[i].Summary(); s != want {
			t.Errorf("summary %d = %q, want %q", i, s, want)
		}
	}
}

func TestReplyErr(t *testing.T) {
	var r Reply
	_ = json.Unmarshal([]byte(`{"info":{"id":"m","error":{"name":"MessageAbortedError","data":{"message":"aborted"}}},"parts":[]}`), &r)
	if err := r.Err(); err == nil || err.Error() != "MessageAbortedError: aborted" {
		t.Fatalf("err %v", err)
	}
}

func TestPorts(t *testing.T) {
	p, err := FreePort()
	if err != nil || p <= 0 {
		t.Fatal(err, p)
	}
	if !PortFree(p) {
		t.Fatalf("port %d should be free right after FreePort", p)
	}
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	bound, _ := strconv.Atoi(u.Port())
	if bound <= 0 || PortFree(bound) {
		t.Fatalf("port %d is bound by the test server and must not read as free", bound)
	}
}
