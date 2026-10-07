package layout

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/store"
)

func TestOpencodeCmd(t *testing.T) {
	e := &Engine{Cfg: config.Config{Opencode: "opencode", OpencodeSessions: true}}
	w := &store.Workstream{ID: "AISW-1", OpencodeSession: "ses_x", OpencodePort: 45617}
	if got, want := e.opencodeCmd(w), "opencode -s ses_x --port 45617 --hostname 127.0.0.1"; got != want {
		t.Errorf("with port: %q, want %q", got, want)
	}
	w.OpencodePort = 0
	if got, want := e.opencodeCmd(w), "opencode -s ses_x"; got != want {
		t.Errorf("no port: %q, want %q", got, want)
	}
	w.OpencodeSession = ""
	w.OpencodePort = 45617
	if got := e.opencodeCmd(w); got != "opencode" {
		t.Errorf("no session: %q (a port alone must not change the command)", got)
	}
	e.Cfg.OpencodeSessions = false
	w.OpencodeSession = "ses_x"
	if got := e.opencodeCmd(w); got != "opencode" {
		t.Errorf("sessions off: %q, want the configured command as-is", got)
	}
}

func TestEnsurePort(t *testing.T) {
	s := store.Store{Root: t.TempDir()}
	w := &store.Workstream{ID: "tim-0001", Category: "personal", Desc: "x", Created: time.Now()}
	if err := s.Save(w); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Cfg: config.Config{Opencode: "opencode", OpencodeSessions: true}, Store: s}

	// none yet: one is allocated and saved
	if err := e.ensurePort(w); err != nil {
		t.Fatal(err)
	}
	first := w.OpencodePort
	if first <= 0 {
		t.Fatalf("no port allocated")
	}
	again, _ := s.Resolve("tim-0001")
	if again.OpencodePort != first {
		t.Fatalf("port not saved: %d vs %d", again.OpencodePort, first)
	}
	// free: kept
	if err := e.ensurePort(w); err != nil || w.OpencodePort != first {
		t.Fatalf("free port should be kept: %v %d", err, w.OpencodePort)
	}
	// taken by someone else: replaced
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	bound, _ := strconv.Atoi(u.Port())
	w.OpencodePort = bound
	if err := e.ensurePort(w); err != nil {
		t.Fatal(err)
	}
	if w.OpencodePort == bound || w.OpencodePort <= 0 {
		t.Fatalf("taken port %d should be replaced, got %d", bound, w.OpencodePort)
	}
}
