// Package sway is a minimal client for the sway IPC protocol (the i3 IPC
// protocol): run commands, read the tree, subscribe to events. It talks to
// $SWAYSOCK directly so a command round-trip is one syscall pair, not a
// swaymsg fork.
//
// Wire format (little endian): "i3-ipc" | uint32 payload length | uint32
// message type | payload. Replies carry the same header; events have the
// high bit of the type set.
package sway

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

const magic = "i3-ipc"

// Message types.
const (
	MsgRunCommand    uint32 = 0
	MsgGetWorkspaces uint32 = 1
	MsgSubscribe     uint32 = 2
	MsgGetTree       uint32 = 4
	MsgGetVersion    uint32 = 7
)

// Conn is one IPC connection. It is not safe for concurrent use.
type Conn struct {
	c net.Conn
	r *bufio.Reader
}

// SocketPath returns $SWAYSOCK, falling back to `sway --get-socketpath`.
func SocketPath() (string, error) {
	if p := os.Getenv("SWAYSOCK"); p != "" {
		return p, nil
	}
	out, err := exec.Command("sway", "--get-socketpath").Output()
	if err != nil {
		return "", errors.New("SWAYSOCK is not set and `sway --get-socketpath` failed; are you inside a sway session?")
	}
	return strings.TrimSpace(string(out)), nil
}

// Dial connects to the running sway.
func Dial() (*Conn, error) {
	p, err := SocketPath()
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("unix", p, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("sway ipc: %w", err)
	}
	return &Conn{c: c, r: bufio.NewReader(c)}, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

func (c *Conn) send(typ uint32, payload []byte) error {
	hdr := make([]byte, len(magic)+8)
	copy(hdr, magic)
	binary.LittleEndian.PutUint32(hdr[6:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[10:], typ)
	if _, err := c.c.Write(append(hdr, payload...)); err != nil {
		return fmt.Errorf("sway ipc write: %w", err)
	}
	return nil
}

// recv reads one message; it returns the type (with the event bit intact)
// and the payload.
func (c *Conn) recv() (uint32, []byte, error) {
	hdr := make([]byte, len(magic)+8)
	if _, err := io.ReadFull(c.r, hdr); err != nil {
		return 0, nil, fmt.Errorf("sway ipc read: %w", err)
	}
	if string(hdr[:6]) != magic {
		return 0, nil, errors.New("sway ipc: bad magic")
	}
	n := binary.LittleEndian.Uint32(hdr[6:])
	typ := binary.LittleEndian.Uint32(hdr[10:])
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return 0, nil, fmt.Errorf("sway ipc read: %w", err)
	}
	return typ, payload, nil
}

// roundTrip sends one request and reads one reply of the same type.
func (c *Conn) roundTrip(typ uint32, payload []byte) ([]byte, error) {
	if err := c.send(typ, payload); err != nil {
		return nil, err
	}
	for {
		t, p, err := c.recv()
		if err != nil {
			return nil, err
		}
		if t&0x80000000 != 0 {
			continue // stray event on a connection that also subscribed
		}
		return p, nil
	}
}

// Result is one entry of a RUN_COMMAND reply.
type Result struct {
	Success    bool   `json:"success"`
	ParseError bool   `json:"parse_error"`
	Error      string `json:"error"`
}

// Command runs a sway command string (";"-separated commands are executed
// as one batch, i.e. one layout transaction). It returns an error if any
// command in the batch failed.
func (c *Conn) Command(cmd string) ([]Result, error) {
	p, err := c.roundTrip(MsgRunCommand, []byte(cmd))
	if err != nil {
		return nil, err
	}
	var res []Result
	if err := json.Unmarshal(p, &res); err != nil {
		return nil, fmt.Errorf("sway ipc: bad command reply: %w", err)
	}
	var failed []string
	for i, r := range res {
		if !r.Success {
			failed = append(failed, fmt.Sprintf("#%d: %s", i+1, r.Error))
		}
	}
	if len(failed) > 0 {
		return res, fmt.Errorf("sway command failed (%s) in: %s", strings.Join(failed, "; "), cmd)
	}
	return res, nil
}

// GetTree returns the full layout tree.
func (c *Conn) GetTree() (*Node, error) {
	p, err := c.roundTrip(MsgGetTree, nil)
	if err != nil {
		return nil, err
	}
	var n Node
	if err := json.Unmarshal(p, &n); err != nil {
		return nil, fmt.Errorf("sway ipc: bad tree: %w", err)
	}
	return &n, nil
}

// Workspace is one entry of GET_WORKSPACES.
type Workspace struct {
	Num     int    `json:"num"`
	Name    string `json:"name"`
	Focused bool   `json:"focused"`
	Visible bool   `json:"visible"`
	Urgent  bool   `json:"urgent"`
	Output  string `json:"output"`
}

// GetWorkspaces lists workspaces.
func (c *Conn) GetWorkspaces() ([]Workspace, error) {
	p, err := c.roundTrip(MsgGetWorkspaces, nil)
	if err != nil {
		return nil, err
	}
	var ws []Workspace
	if err := json.Unmarshal(p, &ws); err != nil {
		return nil, fmt.Errorf("sway ipc: bad workspaces: %w", err)
	}
	return ws, nil
}

// Event is one subscription event.
type Event struct {
	Type    string          // "workspace", "window", ...
	Payload json.RawMessage // the raw event object
}

var eventNames = map[uint32]string{
	0: "workspace", 1: "output", 2: "mode", 3: "window", 4: "barconfig_update",
	5: "binding", 6: "shutdown", 7: "tick", 20: "bar_state_update", 21: "input",
}

// Subscribe registers for the given event types on this connection. After
// it returns, call Next repeatedly; the connection must not be used for
// other requests.
func (c *Conn) Subscribe(events ...string) error {
	b, _ := json.Marshal(events)
	p, err := c.roundTrip(MsgSubscribe, b)
	if err != nil {
		return err
	}
	var r struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(p, &r); err != nil || !r.Success {
		return errors.New("sway ipc: subscribe failed")
	}
	return nil
}

// Next blocks for the next event.
func (c *Conn) Next() (Event, error) {
	for {
		t, p, err := c.recv()
		if err != nil {
			return Event{}, err
		}
		if t&0x80000000 == 0 {
			continue
		}
		name := eventNames[t&0x7fffffff]
		if name == "" {
			name = fmt.Sprintf("event-%d", t&0x7fffffff)
		}
		return Event{Type: name, Payload: p}, nil
	}
}

// SetDeadline bounds the next read/write (use with Next for timeouts).
func (c *Conn) SetDeadline(t time.Time) error { return c.c.SetDeadline(t) }

// WindowEvent is the payload of a "window" event.
type WindowEvent struct {
	Change    string `json:"change"` // new, close, focus, title, move, mark, ...
	Container Node   `json:"container"`
}

// WorkspaceEvent is the payload of a "workspace" event.
type WorkspaceEvent struct {
	Change  string `json:"change"` // init, empty, focus, move, rename, urgent, reload
	Current *Node  `json:"current"`
	Old     *Node  `json:"old"`
}
