// Package oc talks to opencode about sessions through its own HTTP API,
// using a short-lived `opencode serve` on a random port. That keeps
// juggler off opencode's database and on its public surface.
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

// Server is a running `opencode serve`.
type Server struct {
	URL  string
	cmd  *exec.Cmd
	auth string // basic auth password, if OPENCODE_SERVER_PASSWORD is set
}

var listening = regexp.MustCompile(`https?://[^\s]+`)

// Start launches `opencode serve` in dir and waits until it listens.
func Start(ctx context.Context, opencode string, dir string) (*Server, error) {
	port, err := freePort()
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
	s := &Server{cmd: cmd, auth: os.Getenv("OPENCODE_SERVER_PASSWORD")}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m := listening.FindString(out.String()); m != "" && strings.Contains(out.String(), "listening") {
			s.URL = strings.TrimRight(m, "/")
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

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (s *Server) do(method, path string, q url.Values, body any, out any) error {
	u := s.URL + path
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
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.auth != "" {
		req.SetBasicAuth("opencode", s.auth)
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
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Create makes an empty session in dir with the given title.
func (s *Server) Create(dir, title string) (Session, error) {
	var out Session
	err := s.do("POST", "/session", url.Values{"directory": {dir}}, map[string]any{"title": title}, &out)
	return out, err
}

// Rename sets a session's title.
func (s *Server) Rename(dir, id, title string) error {
	return s.do("PATCH", "/session/"+url.PathEscape(id), url.Values{"directory": {dir}}, map[string]any{"title": title}, nil)
}

// Get fetches one session, as seen from dir (any project is visible).
func (s *Server) Get(dir, id string) (Session, error) {
	var out Session
	err := s.do("GET", "/session/"+url.PathEscape(id), url.Values{"directory": {dir}}, nil, &out)
	return out, err
}

// List returns top-level sessions of dir's project, newest first. With
// search, titles are filtered server-side (substring).
func (s *Server) List(dir string, search string, limit int) ([]Session, error) {
	q := url.Values{"directory": {dir}, "scope": {"project"}, "roots": {"true"}, "limit": {strconv.Itoa(limit)}}
	if search != "" {
		q.Set("search", search)
	}
	var out []Session
	err := s.do("GET", "/session", q, nil, &out)
	return out, err
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
