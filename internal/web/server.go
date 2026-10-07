// Package web is juggler's HTTP face: a JSON REST API over internal/app, a
// Server-Sent-Events stream of sway changes, and the embedded single-page
// UI that uses both. Routes are listed in the README ("Web UI and REST API").
//
// The server runs commands as the user with no authentication: bind it to
// loopback (the default) and nothing else.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/varlogtim/juggler/internal/app"
	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/store"
)

//go:embed ui
var ui embed.FS

// Server serves the API and the UI.
type Server struct {
	app    *app.App
	mux    *http.ServeMux
	events *hub
	Log    *log.Logger
}

// New builds a server over a.
func New(a *app.App) *Server {
	s := &Server{app: a, mux: http.NewServeMux(), events: newHub(), Log: log.New(os.Stderr, "jug serve: ", log.LstdFlags)}
	s.routes()
	return s
}

// Handler returns the HTTP handler (for tests and embedding).
func (s *Server) Handler() http.Handler { return s.logging(s.mux) }

// ListenAndServe serves on addr until the process ends. It refuses
// non-loopback addresses unless JUG_SERVE_ANY=1 is set.
func (s *Server) ListenAndServe(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" && os.Getenv("JUG_SERVE_ANY") == "" {
		return fmt.Errorf("refusing to listen on %s: the API runs commands as you with no authentication (set JUG_SERVE_ANY=1 to override)", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.events.watch(ctx, s.app)
	sched := app.NewScheduler(s.app, s.Log)
	sched.OnReport = func(rep *app.Report, err error) {
		if err == nil && rep != nil && (len(rep.Created)+len(rep.Adopted)+len(rep.Updated)+len(rep.Regrouped)+len(rep.GroupsRegistered)+len(rep.GroupsUpdated)) > 0 {
			s.events.publish("changed", "sync")
			return
		}
		s.events.publish("synced", rep.Source) // status only (last run moved)
	}
	go sched.Run(ctx)
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

func (s *Server) routes() {
	m := s.mux
	// UI
	sub, _ := fs.Sub(ui, "ui")
	files := http.FileServer(http.FS(sub))
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, sub, "index.html")
	})
	m.Handle("GET /ui/", http.StripPrefix("/ui/", files))

	// API
	m.HandleFunc("GET /api/v1/health", s.health)
	m.HandleFunc("GET /api/v1/status", s.status)
	m.HandleFunc("GET /api/v1/doctor", s.doctor)
	m.HandleFunc("GET /api/v1/config", s.config)
	m.HandleFunc("GET /api/v1/repos", s.repos)
	m.HandleFunc("GET /api/v1/events", s.events.serve)

	m.HandleFunc("POST /api/v1/slot/toggle", s.slotToggle)
	m.HandleFunc("POST /api/v1/slot/park", s.slotPark)
	m.HandleFunc("POST /api/v1/slot/park-others", s.slotParkOthers)
	m.HandleFunc("POST /api/v1/lot", s.lot)

	m.HandleFunc("GET /api/v1/sources", s.sources)
	m.HandleFunc("GET /api/v1/sources/{name}", s.sourceGet)
	m.HandleFunc("POST /api/v1/sources/{name}/sync", s.sourceSync)
	m.HandleFunc("POST /api/v1/sources/{name}/forgive", s.sourceForgive)
	m.HandleFunc("POST /api/v1/sources/{name}/prune", s.sourcePrune)
	m.HandleFunc("POST /api/v1/sync", s.syncAll)

	m.HandleFunc("GET /api/v1/groups", s.groups)
	m.HandleFunc("GET /api/v1/groups/{name}", s.groupGet)
	m.HandleFunc("PUT /api/v1/groups/{name}", s.groupPut)
	m.HandleFunc("DELETE /api/v1/groups/{name}", s.groupDelete)
	m.HandleFunc("POST /api/v1/groups/{name}/members", s.groupAddMembers)
	m.HandleFunc("DELETE /api/v1/groups/{name}/members/{ws}", s.groupRemoveMember)

	m.HandleFunc("GET /api/v1/workstreams", s.list)
	m.HandleFunc("POST /api/v1/workstreams", s.create)
	m.HandleFunc("GET /api/v1/workstreams/{ws}", s.get)
	m.HandleFunc("PATCH /api/v1/workstreams/{ws}", s.set)
	m.HandleFunc("DELETE /api/v1/workstreams/{ws}", s.remove)
	m.HandleFunc("GET /api/v1/workstreams/{ws}/plan", s.plan)
	m.HandleFunc("GET /api/v1/workstreams/{ws}/env", s.env)
	m.HandleFunc("GET /api/v1/workstreams/{ws}/todo", s.todoGet)
	m.HandleFunc("PUT /api/v1/workstreams/{ws}/todo", s.todoPut)
	m.HandleFunc("GET /api/v1/workstreams/{ws}/refs", s.refs)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/refs", s.refAdd)
	m.HandleFunc("DELETE /api/v1/workstreams/{ws}/refs/{type}", s.refRemove)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/repo", s.repoAdd)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/seed", s.seed)

	m.HandleFunc("POST /api/v1/workstreams/{ws}/complete", s.complete)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/reopen", s.reopen)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/show", s.show)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/close", s.closeWS)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/open", s.open)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/term", s.term)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/notes", s.notes)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/review", s.review)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/focus", s.focus)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/dictate", s.dictate)

	m.HandleFunc("GET /api/v1/workstreams/{ws}/session", s.sessionGet)
	m.HandleFunc("GET /api/v1/workstreams/{ws}/session/candidates", s.sessionCandidates)
	m.HandleFunc("PUT /api/v1/workstreams/{ws}/session", s.sessionPut)
	m.HandleFunc("DELETE /api/v1/workstreams/{ws}/session", s.sessionUnpin)
	m.HandleFunc("POST /api/v1/workstreams/{ws}/session/relaunch", s.sessionRelaunch)
}

// ---------------------------------------------------------------- plumbing

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// fail maps an error to a status: sway unreachable → 503, not found → 404,
// bad input → 400, else 500.
func (s *Server) fail(w http.ResponseWriter, err error) {
	var se *app.SwayError
	switch {
	case errors.As(err, &se):
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: err.Error(), Code: "sway_unavailable"})
	case errors.Is(err, app.ErrNotFound):
		writeJSON(w, http.StatusNotFound, apiError{Error: strings.TrimPrefix(err.Error(), "not found: "), Code: "not_found"})
	case errors.Is(err, app.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, apiError{Error: strings.TrimPrefix(err.Error(), "invalid: "), Code: "bad_request"})
	case isNotFound(err):
		writeJSON(w, http.StatusNotFound, apiError{Error: err.Error(), Code: "not_found"})
	case errors.Is(err, app.ErrDirty):
		writeJSON(w, http.StatusConflict, apiError{Error: err.Error(), Code: "dirty"})
	case errors.Is(err, app.ErrNoSession):
		writeJSON(w, http.StatusNotFound, apiError{Error: err.Error(), Code: "no_session"})
	case errors.Is(err, app.ErrNothingParked):
		writeJSON(w, http.StatusConflict, apiError{Error: err.Error(), Code: "nothing_parked"})
	case isBadInput(err):
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error(), Code: "bad_request"})
	default:
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
	}
}

func isNotFound(err error) bool {
	m := err.Error()
	return strings.HasPrefix(m, "no workstream matches") || strings.Contains(m, "is ambiguous") || strings.Contains(m, "matches ") && strings.Contains(m, "workstreams:")
}

func isBadInput(err error) bool {
	m := err.Error()
	for _, p := range []string{"is required", "already exists", "mutually exclusive", "needs a URL", "has no ", "is not a directory", "not set in", "is neither", "is not a git checkout", "already checked out", "already has its own code dir", "not found (neither", "no seed dir", "bad request"} {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

type badRequest struct{ msg string }

func (b badRequest) Error() string { return "bad request: " + b.msg }

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return badRequest{err.Error()}
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return badRequest{"invalid JSON: " + err.Error()}
	}
	return nil
}

// resolve finds the {ws} path value (canonical name, id or unique prefix).
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) (*store.Workstream, bool) {
	ws, err := s.app.Resolve(r.PathValue("ws"))
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return ws, true
}

// engine runs fn with a fresh, serialized engine and maps failures.
func (s *Server) engine(w http.ResponseWriter, fn func(e *layout.Engine) error) bool {
	if err := s.app.WithEngine(fn); err != nil {
		s.fail(w, err)
		return false
	}
	return true
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ui/") || r.URL.Path == "/api/v1/events" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		if r.Method != http.MethodGet || rw.status >= 400 {
			s.Log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rw.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------- misc

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "version": app.Version, "time": time.Now().Format(time.RFC3339)})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.app.Status()) }

func (s *Server) doctor(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.app.Doctor()) }

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.Cfg
	writeJSON(w, 200, map[string]any{
		"path": configPath(), "root": cfg.Root, "state_dir": cfg.StateDir, "slot_label": cfg.SlotLabel, "lot_label": cfg.LotLabel,
		"terminal": cfg.Terminal, "opencode": cfg.Opencode, "opencode_sessions": cfg.OpencodeSessions, "editor": cfg.Editor,
		"browser": cfg.Browser, "dictator": cfg.Dictator, "left_width_ppt": cfg.LeftWidthPPT, "default_category": cfg.DefaultCategory,
		"id_prefix": cfg.IDPrefix, "jira_base_url": cfg.JiraBaseURL, "code_subdir": cfg.CodeSubdir, "listen": cfg.Listen,
		"categories": keys(s.app.KnownCategories()), "repos": s.app.Repos(),
	})
}

func (s *Server) repos(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.app.Repos()) }

// ---------------------------------------------------------------- slot / lot

func (s *Server) slotToggle(w http.ResponseWriter, r *http.Request) {
	var on bool
	var slot *store.Slot
	if !s.engine(w, func(e *layout.Engine) (err error) { on, slot, err = s.app.Toggle(e); return }) {
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"on": on, "slot": slot})
}

func (s *Server) slotPark(w http.ResponseWriter, r *http.Request) {
	if !s.engine(w, s.app.Park) {
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) slotParkOthers(w http.ResponseWriter, r *http.Request) {
	if !s.engine(w, s.app.ParkOthers) {
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) lot(w http.ResponseWriter, r *http.Request) {
	if !s.engine(w, s.app.Lot) {
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- workstreams: CRUD

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	infos, err := s.app.Infos()
	if err != nil {
		s.fail(w, err)
		return
	}
	if g := r.URL.Query().Get("group"); g != "" {
		kept := []app.WorkstreamInfo{}
		for _, in := range infos {
			for _, x := range in.Groups {
				if x == g {
					kept = append(kept, in)
					break
				}
			}
		}
		infos = kept
	}
	writeJSON(w, 200, infos)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Category string   `json:"category"`
		ID       string   `json:"id"`
		Desc     string   `json:"desc"`
		CodeDir  string   `json:"code_dir"`
		Jira     string   `json:"jira"`
		PR       string   `json:"pr"`
		Repo     string   `json:"repo"`
		Branch   string   `json:"branch"`
		Base     string   `json:"base"`
		NoFetch  bool     `json:"no_fetch"`
		Show     bool     `json:"show"`
		Groups   []string `json:"groups"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if b.CodeDir != "" && b.Repo != "" {
		s.fail(w, badRequest{"code_dir and repo are mutually exclusive"})
		return
	}
	ws, err := s.app.Create(app.CreateOptions{Category: b.Category, ID: b.ID, Desc: b.Desc, CodeDir: b.CodeDir, Jira: b.Jira, PR: b.PR, Groups: b.Groups})
	if err != nil {
		s.fail(w, err)
		return
	}
	var wt *app.WorktreeResult
	var warnings []string
	if b.Repo != "" {
		res, err := s.app.AddWorktree(ws, app.WorktreeOptions{Repo: b.Repo, Branch: b.Branch, Base: b.Base, NoFetch: b.NoFetch})
		if err != nil {
			warnings = append(warnings, "created without code: "+err.Error())
		} else {
			wt = &res
		}
	}
	if b.Show {
		if err := s.app.WithEngine(func(e *layout.Engine) error { return s.app.Show(e, ws, true) }); err != nil {
			warnings = append(warnings, "created but not shown: "+err.Error())
		}
	}
	s.events.changed()
	writeJSON(w, http.StatusCreated, map[string]any{"workstream": s.app.Info(ws), "worktree": wt, "warnings": warnings})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, s.app.Info(ws))
}

func (s *Server) set(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var b struct {
		Category *string   `json:"category"`
		ID       *string   `json:"id"`
		Desc     *string   `json:"desc"`
		Jira     *string   `json:"jira"`
		Groups   *[]string `json:"groups"` // replaces the tags
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if b.Groups != nil {
		if err := s.app.SetGroups(ws, *b.Groups); err != nil {
			s.fail(w, err)
			return
		}
		if b.Category == nil && b.ID == nil && b.Desc == nil && b.Jira == nil {
			s.events.changed()
			writeJSON(w, 200, map[string]any{"result": app.SetResult{OldName: ws.Name(), Name: ws.Name()}, "workstream": s.app.Info(ws)})
			return
		}
	}
	o := app.SetOptions{}
	if b.Category != nil {
		o.Category = *b.Category
	}
	if b.ID != nil {
		o.ID = *b.ID
	}
	if b.Desc != nil {
		o.Desc = *b.Desc
	}
	if b.Jira != nil {
		o.Jira = *b.Jira
	}
	var res app.SetResult
	// windows are handled when sway is reachable; otherwise the rename still happens
	err := s.app.WithEngine(func(e *layout.Engine) (err error) { res, err = s.app.Set(e, ws, o); return })
	var se *app.SwayError
	if errors.As(err, &se) {
		res, err = s.app.Set(nil, ws, o)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"result": res, "workstream": s.app.Info(ws)})
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, s.app.Plan(ws))
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "true"
	err := s.app.WithEngine(func(e *layout.Engine) error { return s.app.Remove(e, ws, force) })
	var se *app.SwayError
	if errors.As(err, &se) {
		err = s.app.Remove(nil, ws, force)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"removed": ws.Name()})
}

func (s *Server) env(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, s.app.Env(ws))
}

func (s *Server) todoGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	text, err := s.app.TodoRead(ws)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": ws.TodoPath(), "text": text, "open": ws.OpenTodos()})
}

func (s *Server) todoPut(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var b struct {
		Text *string `json:"text"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if b.Text == nil {
		s.fail(w, badRequest{"text is required"})
		return
	}
	if err := s.app.TodoWrite(ws, *b.Text); err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"path": ws.TodoPath(), "open": ws.OpenTodos()})
}

// ---------------------------------------------------------------- refs

func (s *Server) refs(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	refs := ws.Refs
	if refs == nil {
		refs = []store.Ref{}
	}
	writeJSON(w, 200, refs)
}

func (s *Server) refAdd(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var b struct {
		Type   string `json:"type"`
		Value  string `json:"value"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	ref, err := s.app.AddRef(ws, b.Type, b.Value, b.Title, b.Status)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, http.StatusCreated, ref)
}

func (s *Server) refRemove(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	ident := r.URL.Query().Get("key")
	if ident == "" {
		ident = r.URL.Query().Get("url")
	}
	if err := s.app.RemoveRef(ws, r.PathValue("type"), ident); err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- worktree / seed

func (s *Server) repoAdd(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var b struct {
		Repo    string `json:"repo"`
		Branch  string `json:"branch"`
		Base    string `json:"base"`
		NoFetch bool   `json:"no_fetch"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if b.Repo == "" {
		s.fail(w, badRequest{"repo is required"})
		return
	}
	res, err := s.app.AddWorktree(ws, app.WorktreeOptions{Repo: b.Repo, Branch: b.Branch, Base: b.Base, NoFetch: b.NoFetch})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, http.StatusCreated, map[string]any{"worktree": res, "workstream": s.app.Info(ws)})
}

func (s *Server) seed(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var b struct {
		Force bool `json:"force"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	written, err := s.app.Seed(ws, b.Force)
	if err != nil {
		s.fail(w, err)
		return
	}
	if written == nil {
		written = []string{}
	}
	writeJSON(w, 200, map[string]any{"seeded": written})
}

// complete and reopen are POST /workstreams/{ws}/complete|reopen: the
// juggler-only finished mark. Store-only, so they work without sway.
func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if err := s.app.Complete(ws); err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, s.app.Info(ws))
}

func (s *Server) reopen(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if err := s.app.Reopen(ws); err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, s.app.Info(ws))
}

// ---------------------------------------------------------------- windows

func (s *Server) show(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var b struct {
		NoSwitch bool `json:"no_switch"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if !s.engine(w, func(e *layout.Engine) error { return s.app.Show(e, ws, b.NoSwitch) }) {
		return
	}
	s.events.changed()
	writeJSON(w, 200, s.app.Info(ws))
}

func (s *Server) closeWS(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if !s.engine(w, func(e *layout.Engine) error { return s.app.Close(e, ws) }) {
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"closed": ws.Name()})
}

func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var b struct {
		What string `json:"what"` // jira | pr | issue | <url>
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if b.What == "" {
		s.fail(w, badRequest{"what is required (jira, pr, issue or a URL)"})
		return
	}
	var queued bool
	if !s.engine(w, func(e *layout.Engine) (err error) { queued, err = s.app.Open(e, ws, b.What); return }) {
		return
	}
	writeJSON(w, 200, map[string]any{"queued": queued})
}

func (s *Server) term(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var b struct {
		Title   string `json:"title"`
		Command string `json:"command"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if !s.engine(w, func(e *layout.Engine) error { return s.app.Term(e, ws, b.Title, b.Command) }) {
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) notes(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if !s.engine(w, func(e *layout.Engine) error { return s.app.Notes(e, ws) }) {
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if !s.engine(w, func(e *layout.Engine) error { return s.app.Review(e, ws) }) {
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) focus(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if !s.engine(w, func(e *layout.Engine) error { return s.app.Focus(e, ws) }) {
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) dictate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if err := s.app.Dictate(ws); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- sessions

func (s *Server) sessionGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	sess, err := s.app.SessionGet(ws)
	if err != nil {
		if errors.Is(err, app.ErrNoSession) {
			writeJSON(w, 200, map[string]any{"pinned": "", "session": nil})
			return
		}
		writeJSON(w, 200, map[string]any{"pinned": ws.OpencodeSession, "session": nil, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"pinned": ws.OpencodeSession, "session": sess})
}

func (s *Server) sessionCandidates(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	all := r.URL.Query().Get("all") == "true"
	cands, err := s.app.SessionCandidates(ws, all)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"pinned": ws.OpencodeSession, "sessions": cands})
}

func (s *Server) sessionPut(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var b struct {
		ID       string `json:"id"`
		New      bool   `json:"new"`
		Relaunch bool   `json:"relaunch"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if b.ID == "" && !b.New {
		s.fail(w, badRequest{"id or new is required"})
		return
	}
	run := func(e *layout.Engine) error {
		if b.New {
			_, err := s.app.SessionNew(e, ws, b.Relaunch)
			return err
		}
		return s.app.SessionPin(e, ws, b.ID, b.Relaunch)
	}
	var err error
	if b.Relaunch {
		err = s.app.WithEngine(run)
	} else {
		err = run(nil)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"pinned": ws.OpencodeSession})
}

func (s *Server) sessionUnpin(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if err := s.app.SessionUnpin(ws); err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"pinned": ""})
}

func (s *Server) sessionRelaunch(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if !s.engine(w, func(e *layout.Engine) error { return s.app.SessionRelaunch(e, ws) }) {
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- groups

func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	gs, err := s.app.Groups(r.URL.Query().Get("all") == "true")
	if err != nil {
		s.fail(w, err)
		return
	}
	if gs == nil {
		gs = []app.GroupInfo{}
	}
	writeJSON(w, 200, gs)
}

func (s *Server) groupGet(w http.ResponseWriter, r *http.Request) {
	g, err := s.app.Group(r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, g)
}

func (s *Server) groupPut(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Kind  *string `json:"kind"`
		Desc  *string `json:"desc"`
		Start *string `json:"start"`
		End   *string `json:"end"`
		URL   *string `json:"url"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	g, err := s.app.SetGroup(r.PathValue("name"), app.GroupPatch{Kind: b.Kind, Desc: b.Desc, Start: b.Start, End: b.End, URL: b.URL})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	gi, _ := s.app.Group(g.Name)
	writeJSON(w, 200, gi)
}

func (s *Server) groupDelete(w http.ResponseWriter, r *http.Request) {
	n, err := s.app.RemoveGroup(r.PathValue("name"), r.URL.Query().Get("untag") == "true")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"removed": r.PathValue("name"), "untagged": n})
}

func (s *Server) groupAddMembers(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Workstreams []string `json:"workstreams"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if len(b.Workstreams) == 0 {
		s.fail(w, badRequest{"workstreams is required"})
		return
	}
	name := r.PathValue("name")
	for _, q := range b.Workstreams {
		ws, err := s.app.Resolve(q)
		if err != nil {
			s.fail(w, err)
			return
		}
		if err := s.app.Tag(ws, name); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.events.changed()
	g, err := s.app.Group(name)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, g)
}

func (s *Server) groupRemoveMember(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !ws.InGroup(name) {
		s.fail(w, fmt.Errorf("%w: %s is not in group %q", app.ErrNotFound, ws.ID, name))
		return
	}
	if err := s.app.Untag(ws, name); err != nil {
		s.fail(w, err)
		return
	}
	s.events.changed()
	writeJSON(w, 200, map[string]any{"ok": true, "groups": ws.Groups})
}

// ---------------------------------------------------------------- sources / sync

// sources is GET /sources: every configured source with its ledger state
// (an empty list, never null).
func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	sts := s.app.SourceStatuses()
	if sts == nil {
		sts = []app.SourceStatus{}
	}
	writeJSON(w, 200, sts)
}

// sourceGet is GET /sources/{name}; 404 for a name not in the config.
func (s *Server) sourceGet(w http.ResponseWriter, r *http.Request) {
	for _, st := range s.app.SourceStatuses() {
		if st.Name == r.PathValue("name") {
			writeJSON(w, 200, st)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, apiError{Error: "no source " + r.PathValue("name"), Code: "not_found"})
}

// sourceSync runs one source now (synchronously; a few seconds).
func (s *Server) sourceSync(w http.ResponseWriter, r *http.Request) {
	src, err := s.app.SourceByName(r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	dry := r.URL.Query().Get("dry_run") == "true"
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	rep, err := s.app.Sync(ctx, src, app.SyncOptions{DryRun: dry})
	reps := []*app.Report{}
	if rep != nil {
		reps = append(reps, rep)
	}
	if !dry {
		s.events.publish("changed", "sync")
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"reports": reps, "error": err.Error(), "code": "sync_failed"})
		return
	}
	writeJSON(w, 200, map[string]any{"reports": reps})
}

// syncAll is POST /sync: every source in turn, under one deadline. A pull
// failure makes the whole response 502 sync_failed but still carries every
// report, so the caller sees what did run.
func (s *Server) syncAll(w http.ResponseWriter, r *http.Request) {
	dry := r.URL.Query().Get("dry_run") == "true"
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	reps, err := s.app.SyncAll(ctx, app.SyncOptions{DryRun: dry})
	if reps == nil {
		reps = []*app.Report{}
	}
	if !dry {
		s.events.publish("changed", "sync")
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"reports": reps, "error": err.Error(), "code": "sync_failed"})
		return
	}
	writeJSON(w, 200, map[string]any{"reports": reps})
}

// sourcePrune removes the source's untouched leftovers (?dry_run=true lists).
func (s *Server) sourcePrune(w http.ResponseWriter, r *http.Request) {
	if _, err := s.app.SourceByName(r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	dry := r.URL.Query().Get("dry_run") == "true"
	var removed, skipped []string
	err := s.app.WithEngine(func(e *layout.Engine) (err error) {
		removed, skipped, err = s.app.Prune(e, r.PathValue("name"), dry)
		return
	})
	var se *app.SwayError
	if errors.As(err, &se) {
		removed, skipped, err = s.app.Prune(nil, r.PathValue("name"), dry)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if removed == nil {
		removed = []string{}
	}
	if skipped == nil {
		skipped = []string{}
	}
	if !dry && len(removed) > 0 {
		s.events.changed()
	}
	writeJSON(w, 200, map[string]any{"removed": removed, "skipped": skipped, "dry_run": dry})
}

// sourceForgive is POST /sources/{name}/forgive: drop tombstones so the
// next sync may recreate those keys.
func (s *Server) sourceForgive(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Keys []string `json:"keys"`
	}
	if err := decode(r, &b); err != nil {
		s.fail(w, err)
		return
	}
	if len(b.Keys) == 0 {
		s.fail(w, badRequest{"keys is required"})
		return
	}
	for _, k := range b.Keys {
		if err := s.app.Forgive(r.PathValue("name"), k); err != nil {
			s.fail(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"forgiven": b.Keys})
}

// ---------------------------------------------------------------- helpers

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
