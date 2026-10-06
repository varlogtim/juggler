package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/varlogtim/juggler/internal/app"
	"github.com/varlogtim/juggler/internal/config"
)

func configPath() string { return config.Path() }

// hub fans "something changed" events out to SSE clients. Sources: sway
// workspace/window events (debounced) and the server's own mutations. The
// payload is deliberately tiny — clients refetch what they show.
type hub struct {
	mu      sync.Mutex
	clients map[chan event]struct{}
	seq     uint64
}

type event struct {
	Seq    uint64 `json:"seq"`
	Kind   string `json:"kind"` // changed | tick
	Reason string `json:"reason,omitempty"`
	Time   string `json:"time"`
}

func newHub() *hub { return &hub{clients: map[chan event]struct{}{}} }

func (h *hub) publish(kind, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev := event{Seq: h.seq, Kind: kind, Reason: reason, Time: time.Now().Format(time.RFC3339)}
	for ch := range h.clients {
		select {
		case ch <- ev:
		default: // slow client: it will catch up on the next event
		}
	}
}

// changed is called after every mutation the server performed itself.
func (h *hub) changed() { h.publish("changed", "api") }

func (h *hub) subscribe() (chan event, func()) {
	ch := make(chan event, 8)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}
}

// watch turns sway events into hub events, debounced, reconnecting while
// sway is away. It also emits a tick every 30 s so clients refresh git state.
func (h *hub) watch(ctx context.Context, a *app.App) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.publish("tick", "")
			}
		}
	}()
	for ctx.Err() == nil {
		conn, err := a.Dial()
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		if err := conn.Subscribe("workspace", "window"); err != nil {
			conn.Close()
			time.Sleep(3 * time.Second)
			continue
		}
		h.publish("changed", "sway-connected")
		var pending *time.Timer
		for ctx.Err() == nil {
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			ev, err := conn.Next()
			if err != nil {
				if isTimeout(err) {
					continue
				}
				break
			}
			if ev.Type != "workspace" && ev.Type != "window" {
				continue
			}
			// debounce: a show generates a burst of window events
			if pending != nil {
				pending.Stop()
			}
			pending = time.AfterFunc(150*time.Millisecond, func() { h.publish("changed", "sway") })
		}
		conn.Close()
		time.Sleep(time.Second)
	}
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	ok := asTimeout(err, &t)
	return ok && t.Timeout()
}

func asTimeout(err error, target *interface{ Timeout() bool }) bool {
	for err != nil {
		if t, ok := err.(interface{ Timeout() bool }); ok {
			*target = t
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// serve is the SSE endpoint: GET /api/v1/events.
func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	ch, cancel := h.subscribe()
	defer cancel()
	write := func(ev event) bool {
		b, _ := json.Marshal(ev)
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !write(event{Kind: "hello", Time: time.Now().Format(time.RFC3339)}) {
		return
	}
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !write(ev) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
