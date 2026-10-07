package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/varlogtim/juggler/internal/store"
)

// fakeTUI stands in for a workstream's opencode TUI with --port: it answers
// the few routes Say and SessionState use, in opencode's shapes, and
// records what it was asked.
type fakeTUI struct {
	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
	status string // "" = idle, else busy | retry
	perms  []map[string]any
	port   int
}

func newFakeTUI(t *testing.T) *fakeTUI {
	t.Helper()
	f := &fakeTUI{}
	m := http.NewServeMux()
	rec := func(r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.bodies = append(f.bodies, b)
	}
	m.HandleFunc("GET /global/health", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_, _ = w.Write([]byte(`{"healthy":true,"version":"test"}`))
	})
	m.HandleFunc("GET /session/status", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		f.mu.Lock()
		s := f.status
		f.mu.Unlock()
		if s == "" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ses_pinned": map[string]any{"type": s, "attempt": 3, "message": "rate limited", "next": 1}})
	})
	m.HandleFunc("GET /permission", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		f.mu.Lock()
		p := f.perms
		f.mu.Unlock()
		if p == nil {
			p = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(p)
	})
	m.HandleFunc("POST /session/{id}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		w.WriteHeader(204)
	})
	m.HandleFunc("POST /session/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_, _ = w.Write([]byte(`{"info":{"id":"msg","cost":0.5},"parts":[{"type":"text","text":"done"}]}`))
	})
	m.HandleFunc("POST /tui/append-prompt", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_, _ = w.Write([]byte(`true`))
	})
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	f.port, _ = strconv.Atoi(u.Port())
	return f
}

func (f *fakeTUI) last() (string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return "", nil
	}
	return f.calls[len(f.calls)-1], f.bodies[len(f.bodies)-1]
}

func (f *fakeTUI) setStatus(s string) {
	f.mu.Lock()
	f.status = s
	f.mu.Unlock()
}

func sayApp(t *testing.T) (*App, *store.Workstream) {
	t.Helper()
	a := newApp(t)
	a.Cfg.OpencodeSessions = true
	w, err := a.Create(CreateOptions{Desc: "story one", Jira: "PROJ-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUG_WORKSTREAM", "")
	return a, w
}

func TestSessionState(t *testing.T) {
	a, w := sayApp(t)
	ctx := context.Background()
	if st := a.SessionState(ctx, w); st.Status != "none" || st.Live() {
		t.Fatalf("no session: %+v", st)
	}
	w.OpencodeSession = "ses_pinned"
	if st := a.SessionState(ctx, w); st.Status != "no-port" || !strings.Contains(st.Detail, "jug relaunch PROJ-1") {
		t.Fatalf("no port: %+v", st)
	}
	free, _ := freePortForTest()
	w.OpencodePort = free
	if st := a.SessionState(ctx, w); st.Status != "closed" || st.Live() || !strings.Contains(st.Detail, strconv.Itoa(free)) {
		t.Fatalf("closed: %+v", st)
	}
	f := newFakeTUI(t)
	w.OpencodePort = f.port
	st := a.SessionState(ctx, w)
	if st.Status != "idle" || !st.Live() || st.Version != "test" || st.Port != f.port || len(st.Pending) != 0 {
		t.Fatalf("idle: %+v", st)
	}
	f.setStatus("retry")
	if st := a.SessionState(ctx, w); st.Status != "retry" || st.Detail != "attempt 3: rate limited" {
		t.Fatalf("retry: %+v", st)
	}
	f.setStatus("busy")
	f.mu.Lock()
	f.perms = []map[string]any{
		{"id": "p1", "sessionID": "ses_pinned", "permission": "bash", "patterns": []string{"git push"}, "metadata": map[string]any{}, "always": []string{}},
		{"id": "p2", "sessionID": "ses_other", "permission": "edit", "patterns": []string{}, "metadata": map[string]any{}, "always": []string{}},
	}
	f.mu.Unlock()
	st = a.SessionState(ctx, w)
	if st.Status != "busy" || len(st.Pending) != 1 || st.Pending[0] != "bash git push" {
		t.Fatalf("busy with a prompt: %+v", st)
	}
	if l := st.Line(); !strings.Contains(l, "busy") || !strings.Contains(l, "permission pending: bash git push") || !strings.Contains(l, "127.0.0.1:"+strconv.Itoa(f.port)) {
		t.Fatalf("line: %q", l)
	}
	// every pinned workstream, in name order; unpinned ones are skipped
	w2, _ := a.Create(CreateOptions{Desc: "another", Jira: "PROJ-2"})
	w2.OpencodeSession, w2.OpencodePort = "ses_2", free
	_ = a.Store.Save(w2)
	_ = a.Store.Save(w)
	_, _ = a.Create(CreateOptions{Desc: "no session", Jira: "PROJ-3"})
	all, err := a.SessionStates(ctx)
	if err != nil || len(all) != 2 || all[0].ID != "PROJ-1" || all[1].ID != "PROJ-2" || all[0].Status != "busy" || all[1].Status != "closed" {
		t.Fatalf("all: %v %+v", err, all)
	}
}

func TestSayViaTUI(t *testing.T) {
	a, w := sayApp(t)
	ctx := context.Background()
	f := newFakeTUI(t)
	w.OpencodeSession, w.OpencodePort = "ses_pinned", f.port

	// async: prompt_async with the text, back at once
	res, err := a.Say(ctx, w, "  start on the story  ", SayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Via != "tui" || res.Mode != SayAsync || res.Port != f.port || res.Reply != "" {
		t.Fatalf("async result: %+v", res)
	}
	call, body := f.last()
	if call != "POST /session/ses_pinned/prompt_async" {
		t.Fatalf("call %q", call)
	}
	if p := body["parts"].([]any)[0].(map[string]any); p["text"] != "start on the story" {
		t.Fatalf("text not trimmed/sent: %v", body)
	}

	// draft: append-prompt, even while busy
	f.setStatus("busy")
	if _, err := a.Say(ctx, w, "a draft", SayOptions{Mode: SayDraft}); err != nil {
		t.Fatal(err)
	}
	if call, body := f.last(); call != "POST /tui/append-prompt" || body["text"] != "a draft" {
		t.Fatalf("draft: %q %v", call, body)
	}

	// busy: refused by default, with the ways out named
	_, err = a.Say(ctx, w, "more", SayOptions{})
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "--queue") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("busy: %v", err)
	}
	if call, _ := f.last(); call != "GET /permission" && call != "GET /session/status" {
		t.Fatalf("busy must not prompt; last call %q", call)
	}
	// busy + force: sent
	if res, err := a.Say(ctx, w, "more", SayOptions{Force: true}); err != nil || res.Via != "tui" {
		t.Fatalf("force: %v %+v", err, res)
	}
	if call, _ := f.last(); call != "POST /session/ses_pinned/prompt_async" {
		t.Fatalf("force: %q", call)
	}
	// busy + queue: waits for idle, then sends
	go func() { time.Sleep(2500 * time.Millisecond); f.setStatus("") }()
	res, err = a.Say(ctx, w, "queued", SayOptions{Queue: true, Timeout: 10 * time.Second})
	if err != nil || res.Waited == "" {
		t.Fatalf("queue: %v %+v", err, res)
	}
	// busy + queue + short timeout: gives up, says so
	f.setStatus("busy")
	_, err = a.Say(ctx, w, "late", SayOptions{Queue: true, Timeout: 100 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("queue timeout: %v", err)
	}
	f.setStatus("")

	// wait: /message, reply text and cost
	res, err = a.Say(ctx, w, "and wait", SayOptions{Mode: SayWait})
	if err != nil || res.Reply != "done" || res.Cost != 0.5 || res.Via != "tui" {
		t.Fatalf("wait: %v %+v", err, res)
	}
	if call, _ := f.last(); call != "POST /session/ses_pinned/message" {
		t.Fatalf("wait: %q", call)
	}
}

func TestSayRefusals(t *testing.T) {
	a, w := sayApp(t)
	ctx := context.Background()
	if _, err := a.Say(ctx, w, "", SayOptions{}); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, err := a.Say(ctx, w, "x", SayOptions{}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("no session: %v", err)
	}
	w.OpencodeSession = "ses_pinned"
	// the workstream this command runs in
	t.Setenv("JUG_WORKSTREAM", w.Name())
	if _, err := a.Say(ctx, w, "x", SayOptions{}); err == nil || !strings.Contains(err.Error(), "pass --ws") {
		t.Fatalf("self: %v", err)
	}
	t.Setenv("JUG_WORKSTREAM", "")
	// no port, no window (no sway in tests): async needs a window
	_, err := a.Say(ctx, w, "x", SayOptions{})
	if !errors.Is(err, ErrNoLiveTUI) || !strings.Contains(err.Error(), "jug show PROJ-1") || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("no tui async: %v", err)
	}
	if _, err := a.Say(ctx, w, "x", SayOptions{Mode: SayDraft}); !errors.Is(err, ErrNoLiveTUI) {
		t.Fatalf("no tui draft: %v", err)
	}
	// a port nobody answers on: same
	free, _ := freePortForTest()
	w.OpencodePort = free
	if _, err := a.Say(ctx, w, "x", SayOptions{}); !errors.Is(err, ErrNoLiveTUI) {
		t.Fatalf("closed async: %v", err)
	}
	// sessions off
	a.Cfg.OpencodeSessions = false
	if _, err := a.Say(ctx, w, "x", SayOptions{}); err == nil || !strings.Contains(err.Error(), "opencode_sessions") {
		t.Fatalf("sessions off: %v", err)
	}
}

func TestSayHeadlessRunsOpencodeRun(t *testing.T) {
	a, w := sayApp(t)
	ctx := context.Background()
	w.OpencodeSession = "ses_pinned"
	// a stand-in `opencode` that prints its arguments and the JUG_* env
	dir := t.TempDir()
	script := dir + "/opencode"
	if err := writeExecutable(script, "#!/bin/sh\necho \"args: $*\"\necho \"ws: $JUG_WORKSTREAM_ID in $(pwd)\"\n"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.Opencode = script
	res, err := a.Say(ctx, w, "do it", SayOptions{Mode: SayWait})
	if err != nil {
		t.Fatal(err)
	}
	if res.Via != "run" || res.Port != 0 {
		t.Fatalf("headless: %+v", res)
	}
	want := "args: run -s ses_pinned --dir " + w.ResolvedCodeDir() + " do it"
	if !strings.Contains(res.Reply, want) || !strings.Contains(res.Reply, "ws: PROJ-1 in "+w.ResolvedCodeDir()) {
		t.Fatalf("headless reply:\n%s\nwant %q", res.Reply, want)
	}
	// a failing run is an error, output kept
	if err := writeExecutable(script, "#!/bin/sh\necho nope >&2\nexit 3\n"); err != nil {
		t.Fatal(err)
	}
	res, err = a.Say(ctx, w, "do it", SayOptions{Mode: SayWait})
	if err == nil || !strings.Contains(err.Error(), "opencode run") || res.Reply != "nope" {
		t.Fatalf("failing run: %v %+v", err, res)
	}
}

func freePortForTest() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func writeExecutable(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o755)
}
