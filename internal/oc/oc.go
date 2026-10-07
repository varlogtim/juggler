// Package oc talks to opencode through its own HTTP API — the one every
// opencode process serves (`opencode serve`, and a TUI started with
// `--port`). That keeps juggler off opencode's database and on its public
// surface.
//
// Two ways in:
//
//   - Server: a short-lived `opencode serve` on a random port, for session
//     bookkeeping (create, rename, list) when no opencode of the workstream
//     need be running.
//   - Client: an existing server, normally the embedded one of a
//     workstream's live TUI (`opencode -s <id> --port <p>`). Prompting the
//     session THROUGH that process is what makes the message show up in
//     the window and run the model turn there; writing to the session
//     from another server would be invisible to the TUI (two writers).
//
// Why juggler manages sessions at all: `opencode --continue` resumes the
// newest session of the *project*, and opencode identifies a project by the
// git repository — every clone/worktree of the same repo is one project, so
// `--continue` in ~/src/repo-A would happily resume a session from
// ~/src/repo-B. A workstream wants one session of its own, so juggler
// creates it (titled after the workstream) and launches `opencode -s <id>`.
package oc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Session is the subset of opencode's session record juggler uses.
type Session struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	Title     string `json:"title"`
	ParentID  string `json:"parentID,omitempty"`
	Time      struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

// Updated returns the last-updated time.
func (s Session) Updated() time.Time { return time.UnixMilli(s.Time.Updated) }

// ---------------------------------------------------------------- client

// Client speaks to one running opencode server. URL has no trailing slash.
type Client struct {
	URL  string
	auth string // basic auth password, if OPENCODE_SERVER_PASSWORD is set
}

// NewClient returns a client for the server at url (basic auth from
// OPENCODE_SERVER_PASSWORD, as opencode itself expects).
func NewClient(url string) *Client {
	return &Client{URL: strings.TrimRight(url, "/"), auth: os.Getenv("OPENCODE_SERVER_PASSWORD")}
}

// Loopback returns a client for the server on 127.0.0.1:port.
func Loopback(port int) *Client { return NewClient("http://127.0.0.1:" + strconv.Itoa(port)) }

// do issues one request; a 4xx/5xx is an error carrying the body.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := c.URL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.auth != "" {
		req.SetBasicAuth("opencode", c.auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("opencode %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// dirQuery scopes a request to the project of dir ("" = the server's own
// working directory — for a TUI, the instance it shows).
func dirQuery(dir string) url.Values {
	if dir == "" {
		return nil
	}
	return url.Values{"directory": {dir}}
}

// Health asks GET /global/health and returns the server's version. The
// cheapest "is anything listening there, and is it opencode" probe.
func (c *Client) Health(ctx context.Context) (string, error) {
	var out struct {
		Healthy bool   `json:"healthy"`
		Version string `json:"version"`
	}
	if err := c.do(ctx, "GET", "/global/health", nil, nil, &out); err != nil {
		return "", err
	}
	if !out.Healthy {
		return out.Version, errors.New("opencode reports unhealthy")
	}
	return out.Version, nil
}

// Create makes an empty session in dir with the given title.
func (c *Client) Create(dir, title string) (Session, error) {
	var out Session
	err := c.do(context.Background(), "POST", "/session", dirQuery(dir), map[string]any{"title": title}, &out)
	return out, err
}

// Rename sets a session's title.
func (c *Client) Rename(dir, id, title string) error {
	return c.do(context.Background(), "PATCH", "/session/"+url.PathEscape(id), dirQuery(dir), map[string]any{"title": title}, nil)
}

// Get fetches one session, as seen from dir (any project is visible).
func (c *Client) Get(dir, id string) (Session, error) {
	var out Session
	err := c.do(context.Background(), "GET", "/session/"+url.PathEscape(id), dirQuery(dir), nil, &out)
	return out, err
}

// List returns top-level sessions of dir's project, newest first. With
// search, titles are filtered server-side (substring).
func (c *Client) List(dir string, search string, limit int) ([]Session, error) {
	q := url.Values{"directory": {dir}, "scope": {"project"}, "roots": {"true"}, "limit": {strconv.Itoa(limit)}}
	if search != "" {
		q.Set("search", search)
	}
	var out []Session
	err := c.do(context.Background(), "GET", "/session", q, nil, &out)
	return out, err
}

// Status is a session's activity as opencode reports it.
type Status struct {
	Type    string `json:"type"`              // idle | busy | retry
	Attempt int    `json:"attempt,omitempty"` // retry
	Next    int64  `json:"next,omitempty"`    // retry: when, ms
	Message string `json:"message,omitempty"` // retry: why
}

// Statuses asks GET /session/status: every session the server knows that
// is not idle, by id. An absent session is idle.
func (c *Client) Statuses(ctx context.Context) (map[string]Status, error) {
	out := map[string]Status{}
	err := c.do(ctx, "GET", "/session/status", nil, nil, &out)
	return out, err
}

// Permission is a pending permission request — a tool call waiting for a
// human to allow it in that process's UI.
type Permission struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"sessionID"`
	Permission string         `json:"permission"` // bash | edit | external_directory | …
	Tool       string         `json:"tool,omitempty"`
	Patterns   []string       `json:"patterns"`
	Metadata   map[string]any `json:"metadata"`
	Always     []string       `json:"always"`
}

// Summary is one line for a human: the permission and what it is about.
func (p Permission) Summary() string {
	s := p.Permission
	if s == "" {
		s = p.Tool
	}
	switch {
	case len(p.Patterns) > 0:
		s += " " + strings.Join(p.Patterns, " ")
	case p.Metadata["command"] != nil:
		s += fmt.Sprintf(" %v", p.Metadata["command"])
	}
	return s
}

// Permissions asks GET /permission: the requests pending in that process.
func (c *Client) Permissions(ctx context.Context) ([]Permission, error) {
	var out []Permission
	err := c.do(ctx, "GET", "/permission", nil, nil, &out)
	return out, err
}

func textParts(text string) map[string]any {
	return map[string]any{"parts": []map[string]any{{"type": "text", "text": text}}}
}

// PromptAsync sends text to the session as a user message and returns as
// soon as the server accepted it (204); the model turn runs in that
// process. For a TUI the message appears in its window like a typed one.
func (c *Client) PromptAsync(ctx context.Context, sid, text string) error {
	return c.do(ctx, "POST", "/session/"+url.PathEscape(sid)+"/prompt_async", nil, textParts(text), nil)
}

// Reply is the assistant's answer to a Message call.
type Reply struct {
	Info struct {
		ID    string         `json:"id"`
		Error map[string]any `json:"error,omitempty"`
		Cost  float64        `json:"cost"`
	} `json:"info"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"parts"`
}

// Text joins the reply's text parts.
func (r Reply) Text() string {
	var b strings.Builder
	for _, p := range r.Parts {
		if p.Type == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Err returns the assistant message's error, if it ended in one.
func (r Reply) Err() error {
	if len(r.Info.Error) == 0 {
		return nil
	}
	if m, ok := r.Info.Error["data"].(map[string]any); ok && m["message"] != nil {
		return fmt.Errorf("%v: %v", r.Info.Error["name"], m["message"])
	}
	return fmt.Errorf("%v", r.Info.Error["name"])
}

// Message sends text to the session and waits for the assistant's reply —
// a whole model turn, however long it takes (bound it with ctx). A
// permission prompt raised in that process waits for a human there, and so
// does this call.
func (c *Client) Message(ctx context.Context, sid, text string) (Reply, error) {
	var out Reply
	err := c.do(ctx, "POST", "/session/"+url.PathEscape(sid)+"/message", nil, textParts(text), &out)
	return out, err
}

// AppendPrompt types text into the TUI's prompt box without submitting it
// (POST /tui/append-prompt). Only a TUI answers this.
func (c *Client) AppendPrompt(ctx context.Context, text string) error {
	return c.do(ctx, "POST", "/tui/append-prompt", nil, map[string]any{"text": text}, nil)
}

// Toast shows a transient notice in the TUI. variant: info | success |
// warning | error.
func (c *Client) Toast(ctx context.Context, title, message, variant string) error {
	body := map[string]any{"message": message, "variant": variant}
	if title != "" {
		body["title"] = title
	}
	return c.do(ctx, "POST", "/tui/show-toast", nil, body, nil)
}

// ---------------------------------------------------------------- transient server

// Server is a running `opencode serve` started by juggler.
type Server struct {
	*Client
	cmd *exec.Cmd
}

var listening = regexp.MustCompile(`https?://[^\s]+`)

// Start launches `opencode serve` in dir and waits until it listens.
func Start(ctx context.Context, opencode string, dir string) (*Server, error) {
	port, err := FreePort()
	if err != nil {
		return nil, err
	}
	args := []string{"serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"}
	cmd := exec.CommandContext(ctx, opencode, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("opencode serve: %w", err)
	}
	s := &Server{cmd: cmd}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m := listening.FindString(out.String()); m != "" && strings.Contains(out.String(), "listening") {
			s.Client = NewClient(m)
			return s, nil
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Stop()
	return nil, fmt.Errorf("opencode serve did not come up: %s", strings.TrimSpace(out.String()))
}

// Stop terminates the server.
func (s *Server) Stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// FreePort asks the kernel for an unused loopback port. The port is
// released again before this returns, so a caller must bind it soon.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// PortFree reports whether 127.0.0.1:port can be bound right now — i.e.
// nothing is listening there.
func PortFree(port int) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// ErrNoOpencode is returned when the opencode binary is missing.
var ErrNoOpencode = errors.New("opencode not found in PATH")

// Binary returns the opencode executable from a configured command line
// (the first word), checking it exists.
func Binary(command string) (string, error) {
	f := strings.Fields(command)
	if len(f) == 0 {
		return "", ErrNoOpencode
	}
	p, err := exec.LookPath(f[0])
	if err != nil {
		return "", ErrNoOpencode
	}
	return p, nil
}
