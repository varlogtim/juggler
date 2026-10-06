// jug is the juggler CLI: workstreams for sway. It is a thin layer over
// internal/app (which the REST API and web UI share): parse flags, call the
// operation, print the result.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/varlogtim/juggler/internal/app"
	"github.com/varlogtim/juggler/internal/bar"
	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/picker"
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/web"
)

var version = "dev"

const usage = `usage: jug <command> [flags] [args]

slot (the workspace that displays workstreams)
  toggle                 make the focused workspace the slot — its label becomes "WS" —
                         or release it if it already is
  pick                   fuzzel picker: choose a workstream and show it. The last row creates one:
                         type "[work:|personal:] [AISW-123] description" (a ticket key means work
                         and becomes the id), then choose a repo worktree / no code / a directory
  menu [--print]         single-key menu of the actions below ($mod+w): a floating window when
                         run from a hotkey, inline when run in a terminal
  show [--no-switch] WS  show WS in the slot; the one displayed before is parked
  park [--others]        hide the displayed workstream (windows stay alive, as a tab in the lot);
                         --others instead moves parked workstreams found elsewhere (scratchpad) into the lot
  lot                    visit the lot — the hidden workspace holding parked workstreams — or come back
  current [--json]       print the displayed workstream

inside the displayed workstream (default --ws: $JUG_WORKSTREAM, else the displayed one)
  open jira|pr|URL       open in a browser window in the right stack; focus it if already open
  term [-- CMD…]         new terminal in the right stack (cwd = code dir)
  notes                  edit the workstream's TODO.md in the right stack
  review                 read the code dir's uncommitted diff in the editor, right stack
  focus                  focus the opencode window
  dictate                toggle dictation notes into the workstream's notes/ dir
  env                    print the JUG_* environment for shells (eval "$(jug env)")
  session [--ws WS]      the opencode session this workstream resumes (id + title)
  session pick [--all] [--relaunch]
                         choose an existing opencode session for it (default: titles matching
                         the workstream id / refs; --all: every session of the home + code projects)
  session pin ID [--relaunch] | session new [--relaunch] | session unpin
                         pin a session id / create a fresh titled one / forget the pin
                         --relaunch restarts the workstream's opencode on the new session

workstreams
  ls [--json]            list workstreams (--json: the same objects the REST API returns)
  add [--category C] [--id ID] [--jira KEY] [--pr URL] [--show] DESC…
      [--code-dir DIR | --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch]]
                         create <C>_<ID>_<slug>/ under the root (default C: personal, ID: <user>-NNNN);
                         --repo gives it its own git worktree at <ws>/src/<repo> (branch default:
                         the id for ticket ids, else <user>/<slug>; base: the remote's HEAD)
  repo add --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch] [--ws WS]
                         add such a worktree to an existing workstream and point code_dir at it
  repo seed [--ws WS] [--force]
                         (re)copy the repo's seed files (~/.config/juggler/seed/<repo>/: .envrc etc.,
                         templated with {{id}} {{name}} {{repo}} {{dir}} {{code_dir}} {{home}}) into the
                         worktree; done automatically on add. A seeded .envrc is direnv-allowed.
  ref add TYPE VALUE [--title T] [--status S] [--ws WS]
                         attach a ref: jira KEY | pr URL | issue URL | url URL
  ref rm TYPE [KEY|URL] [--ws WS] | ref ls [--ws WS]
  set WS [--category C] [--id ID] [--desc D] [--jira KEY]
                         rename / recategorize (the directory follows); windows are closed and, if it
                         was displayed, reopened — opencode resumes its pinned session. --jira attaches
                         the ticket and, unless given, sets the id to it and the category to work
  close WS               kill the workstream's windows (files are kept)
  rm WS --yes [--force]  close its windows, remove its worktree (refuses if dirty unless --force;
                         the branch is kept) and delete the workstream directory

server
  serve [--listen ADDR]  REST API + web UI (default 127.0.0.1:7474); see README "Web UI and REST API"

misc
  watch [--format plain|json|waybar]
  doctor                 check sway, tools, workspace naming, config
  version | help

Workstream names resolve by canonical name (work_AISW-1_foo), id (AISW-1) or unique prefix.
Config: ~/.config/juggler/config.toml   State: ~/.local/state/juggler/   Store: ~/workstreams/
`

func main() { os.Exit(run(os.Args[1:])) }

// cli holds the service and, for sway-backed commands, one engine for the
// whole invocation.
type cli struct {
	app *app.App
	eng *layout.Engine
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	case "version", "-v", "--version":
		fmt.Println("jug", version)
		return 0
	}

	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	app.Version = version
	c := &cli{app: app.New(cfg)}
	if os.Getenv("JUG_DEBUG") != "" {
		c.app.Debug = func(f string, v ...any) { fmt.Fprintf(os.Stderr, "jug: "+f+"\n", v...) }
	}

	// commands that work without a compositor
	switch cmd {
	case "watch":
		return c.watch(rest)
	case "ls":
		return c.ls(rest)
	case "add":
		return c.add(rest)
	case "env":
		return c.env(rest)
	case "ref":
		return c.ref(rest)
	case "repo":
		return c.repo(rest)
	case "doctor":
		return c.doctor()
	case "serve":
		return c.serve(rest)
	}

	// everything else talks to sway
	c.waitAfterClose()
	eng, done, err := c.app.Engine()
	if err != nil {
		return fail(err)
	}
	defer done()
	c.eng = eng

	switch cmd {
	case "toggle":
		return c.toggle()
	case "pick":
		return c.pick(rest)
	case "menu":
		return c.menu(rest)
	case "show":
		return c.show(rest)
	case "park":
		if len(rest) == 1 && rest[0] == "--others" {
			return fail(c.app.ParkOthers(c.eng))
		}
		return fail(c.app.Park(c.eng))
	case "lot":
		return c.lot()
	case "current":
		return c.current(rest)
	case "open":
		return c.open(rest)
	case "term":
		return c.term(rest)
	case "notes":
		return c.notes(rest)
	case "review":
		return c.review(rest)
	case "focus":
		return c.focus(rest)
	case "dictate":
		return c.dictate()
	case "session":
		return c.session(rest)
	case "close":
		return c.close(rest)
	case "rm":
		return c.rm(rest)
	case "set":
		return c.set(rest)
	}
	fmt.Fprintf(os.Stderr, "jug: unknown command %q\n\n%s", cmd, usage)
	return 2
}

// fail reports err (nil → success). Without a terminal (hotkey) it also
// raises a notification.
func fail(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, picker.ErrCancelled) {
		return 1
	}
	fmt.Fprintln(os.Stderr, "jug:", err)
	if fi, e := os.Stderr.Stat(); e == nil && fi.Mode()&os.ModeCharDevice == 0 {
		layout.Notify("critical", "juggler", err.Error())
	}
	return 1
}

func info(title, body string) {
	fmt.Println(title + ": " + body)
	if fi, e := os.Stdout.Stat(); e == nil && fi.Mode()&os.ModeCharDevice == 0 {
		layout.Notify("low", title, body)
	}
}

// target resolves --ws / $JUG_WORKSTREAM / displayed (engine state when
// there is an engine, saved state otherwise).
func (c *cli) target(explicit string) (*store.Workstream, error) {
	displayed := ""
	if c.eng != nil {
		displayed = c.eng.State.Displayed
	} else {
		displayed = c.app.Displayed()
	}
	w, err := c.app.Target(explicit, displayed)
	if errors.Is(err, app.ErrNoTarget) {
		return nil, errors.New("no workstream displayed and none given (--ws)")
	}
	return w, err
}

func wsFlag(fs *flag.FlagSet) *string {
	p := fs.String("ws", "", "workstream (default: $JUG_WORKSTREAM, else displayed)")
	fs.StringVar(p, "workstream", "", "")
	return p
}

// parseMixed parses flags that may come before or after positionals
// (Go's flag package stops at the first positional).
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	return pos, nil
}

// ---------------------------------------------------------------- slot verbs

func (c *cli) toggle() int {
	on, slot, err := c.app.Toggle(c.eng)
	if err != nil {
		return fail(err)
	}
	if on {
		info("juggler", fmt.Sprintf("workspace %d is now the workstream slot", slot.Num))
	} else {
		info("juggler", "slot released")
	}
	return 0
}

func (c *cli) pick(args []string) int {
	fs := flag.NewFlagSet("pick", flag.ContinueOnError)
	print := fs.Bool("print", false, "print the picker rows instead of showing the picker")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	es, err := c.app.Entries(c.eng)
	if err != nil {
		return fail(err)
	}
	lines := picker.Lines(es)
	if *print {
		fmt.Println(strings.Join(lines, "\n"))
		return 0
	}
	idx, err := picker.Fuzzel("WS> ", lines)
	if err != nil {
		return fail(err)
	}
	ordered := picker.Ordered(es)
	if idx < 0 || idx > len(ordered) {
		return fail(fmt.Errorf("bad selection %d", idx))
	}
	if idx == len(ordered) { // "+ new workstream…"
		return c.pickNew()
	}
	return fail(c.app.Show(c.eng, ordered[idx].W, false))
}

// pickNew is the picker's "+ new workstream…" flow: one line of text
// ("[category:] [TICKET-1] description"), then where the code lives
// (a worktree of a configured repo, none, or an existing directory).
// Nothing is created until both answers are in; Esc anywhere cancels.
func (c *cli) pickNew() int {
	text, err := picker.Prompt("new  [work:|personal:] [AISW-123] description> ")
	if err != nil {
		return fail(err)
	}
	spec := picker.ParseSpec(text, c.app.KnownCategories())
	if spec.Desc == "" {
		if spec.Key == "" {
			return fail(errors.New("a description is required"))
		}
		if spec.Desc, err = picker.Prompt(spec.Key + " description> "); err != nil {
			return fail(err)
		}
	}
	category := c.app.CategoryFor(spec.Category, spec.Key)
	id := strings.ToUpper(spec.Key)
	if id == "" {
		if id, err = c.app.NextManualID(); err != nil {
			return fail(err)
		}
	}
	probe := &store.Workstream{ID: id, Desc: spec.Desc, Category: category}

	// where does the code live?
	repos := c.app.Repos()
	var lines []string
	for _, r := range repos {
		lines = append(lines, fmt.Sprintf("worktree of %-16s branch %s", r.Name, app.DefaultBranch(probe)))
	}
	lines = append(lines, "no code — notes only", "existing directory…")
	choice, err := picker.Fuzzel(store.DirName(category, id, spec.Desc)+"  code> ", lines)
	if err != nil {
		return fail(err)
	}
	codeDir := ""
	if choice == len(repos)+1 {
		if codeDir, err = picker.Prompt("directory> "); err != nil {
			return fail(err)
		}
	}

	w, err := c.app.Create(app.CreateOptions{Category: category, ID: id, Desc: spec.Desc, CodeDir: codeDir, Jira: spec.Key})
	if err != nil {
		return fail(err)
	}
	if choice < len(repos) {
		if _, err := c.app.AddWorktree(w, app.WorktreeOptions{Repo: repos[choice].Name}); err != nil {
			layout.Notify("critical", "juggler", fmt.Sprintf("%s created without code: %v\nretry: jug repo add --ws %s --repo %s", w.ID, err, w.ID, repos[choice].Name))
		}
	}
	return fail(c.app.Show(c.eng, w, false))
}

func (c *cli) set(args []string) int {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	o := app.SetOptions{}
	fs.StringVar(&o.Category, "category", "", "")
	fs.StringVar(&o.ID, "id", "", "")
	fs.StringVar(&o.Desc, "desc", "", "")
	fs.StringVar(&o.Jira, "jira", "", "attach this ticket; also sets --id (and --category work) unless given")
	pos, err := parseMixed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: jug set WS [--category C] [--id ID] [--desc D] [--jira KEY]")
		return 2
	}
	w, err := c.app.Resolve(pos[0])
	if err != nil {
		return fail(err)
	}
	res, err := c.app.Set(c.eng, w, o)
	for _, warn := range res.Warnings {
		fmt.Fprintln(os.Stderr, "jug:", warn)
	}
	if err != nil {
		return fail(err)
	}
	if res.Moved {
		fmt.Printf("%s -> %s\n", res.OldName, res.Name)
	}
	return 0
}

// ---------------------------------------------------------------- menu

// menuAction is one row of `jug menu`.
type menuAction struct {
	key  byte
	name string
	desc string
	run  func() int
}

// menuActions returns the actions that apply right now. w is the displayed
// workstream (nil if none); under is the parked workstream under focus.
func (c *cli) menuActions(w *store.Workstream, under *store.Workstream, lotExists bool) []menuAction {
	var rows []menuAction
	add := func(k byte, name, desc string, run func() int) {
		rows = append(rows, menuAction{k, name, desc, run})
	}
	if under != nil && (w == nil || under.Name() != w.Name()) {
		u := under
		add('g', "go", "show "+u.ID+" (the parked workstream under focus)", func() int { return fail(c.app.Show(c.eng, u, false)) })
	}
	if w != nil {
		add('t', "term", "new terminal in the right stack", func() int { return c.term(nil) })
		if w.Ref("jira") != nil {
			add('j', "jira", "open the ticket in a browser window in the right stack", func() int { return c.open([]string{"jira"}) })
		}
		if w.Ref("pr") != nil {
			add('p', "pr", "open the pull request in a browser window in the right stack", func() int { return c.open([]string{"pr"}) })
		}
		add('n', "notes", "edit TODO.md in the right stack", func() int { return c.notes(nil) })
		add('r', "review", "read the uncommitted diff in the editor, right stack", func() int { return c.review(nil) })
		add('o', "opencode", "focus the opencode window", func() int { return c.focus(nil) })
		add('d', "dictate", "toggle dictation notes into the workstream", c.dictate)
	}
	add('w', "pick", "switch to another workstream", func() int { return c.pick(nil) })
	if lotExists {
		add('l', "lot", "visit the lot (parked workstreams) / come back", c.lot)
	}
	if w != nil {
		add('x', "park", "hide the displayed workstream (windows stay alive)", func() int { return fail(c.app.Park(c.eng)) })
	}
	add('s', "slot", "toggle this workspace as the workstream slot", c.toggle)
	if w != nil {
		ww := w
		add('c', "close", "kill this workstream's windows (files are kept)", func() int { return fail(c.app.Close(c.eng, ww)) })
	}
	return rows
}

// menu is the single-key action menu. From a hotkey (no tty) it opens itself
// in a floating terminal window; in a terminal it draws the rows, waits for
// ONE key and runs that action. Inside the menu window the action is started
// detached and runs once the window is gone, so focus is already back where
// it belongs.
func (c *cli) menu(args []string) int {
	fs := flag.NewFlagSet("menu", flag.ContinueOnError)
	print := fs.Bool("print", false, "print the rows instead of showing the menu")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, _ := c.app.Current(c.eng)
	under, _ := c.app.UnderFocus(c.eng)
	tree, err := c.eng.Sway.GetTree()
	if err != nil {
		return fail(err)
	}
	rows := c.menuActions(w, under, tree.WorkspaceNamed(c.eng.LotName()) != nil)

	title := "no workstream displayed"
	if w != nil {
		title = w.ID + "  " + w.Desc
	}
	var lines []string
	for _, m := range rows {
		lines = append(lines, fmt.Sprintf("  %c  %-9s %s", m.key, m.name, m.desc))
	}
	if *print {
		fmt.Println(title)
		fmt.Println(strings.Join(lines, "\n"))
		return 0
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// hotkey: no terminal — open ourselves in a floating one
		cols, rowsN := 84, len(lines)+5
		cmd := fmt.Sprintf("%s --class %s -T 'juggler' -o window.dimensions.columns=%d -o window.dimensions.lines=%d -e %s menu",
			c.app.Cfg.Terminal, layout.MenuAppID, cols, rowsN, layout.ShellQuote(os.Args[0]))
		_, err := c.eng.Sway.Command("exec " + cmd)
		return fail(err)
	}
	defer tty.Close()

	inMenuWindow := false
	if f := tree.FocusedNode(); f != nil && f.AppID == layout.MenuAppID {
		inMenuWindow = true
	}
	fmt.Fprintf(tty, "\x1b[?25l\x1b[1m %s\x1b[0m\n\n", title)
	for _, m := range rows {
		fmt.Fprintf(tty, "  \x1b[1;33m%c\x1b[0m  \x1b[1m%-9s\x1b[0m %s\n", m.key, m.name, m.desc)
	}
	fmt.Fprintf(tty, "\n  \x1b[2mEsc  cancel\x1b[0m\n")

	key, err := readKey(tty)
	fmt.Fprint(tty, "\x1b[?25h")
	if err != nil {
		return fail(err)
	}
	var chosen *menuAction
	for i := range rows {
		if rows[i].key == key {
			chosen = &rows[i]
		}
	}
	if chosen == nil {
		return 1 // Esc / q / unknown: cancel
	}
	if !inMenuWindow {
		return chosen.run()
	}
	// Re-run ourselves detached with the chosen verb; the child waits for
	// this window to disappear (JUG_AFTER_CLOSE) before touching the tree.
	menuWin := tree.FocusedNode().ID
	child := exec.Command(os.Args[0], menuArgv(chosen.name)...)
	child.Env = append(os.Environ(), fmt.Sprintf("JUG_AFTER_CLOSE=%d", menuWin))
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devnull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	child.Stdin, child.Stdout, child.Stderr = devnull, devnull, devnull
	if err := child.Start(); err != nil {
		return fail(err)
	}
	return 0
}

// menuArgv maps a menu action name to the jug command line that performs it.
func menuArgv(name string) []string {
	switch name {
	case "go":
		return []string{"show", "--under-focus"}
	case "jira", "pr":
		return []string{"open", name}
	case "opencode":
		return []string{"focus"}
	case "slot":
		return []string{"toggle"}
	case "close":
		return []string{"close", "--displayed"}
	}
	return []string{name}
}

// readKey puts the tty in raw-ish mode, reads one key and restores it.
// Escape sequences (arrows etc.) are swallowed and reported as Esc.
func readKey(tty *os.File) (byte, error) {
	stty := func(args ...string) error {
		c := exec.Command("stty", args...)
		c.Stdin = tty
		return c.Run()
	}
	if err := stty("-icanon", "-echo", "min", "1", "time", "0"); err != nil {
		return 0, fmt.Errorf("stty: %w", err)
	}
	defer stty("icanon", "echo")
	buf := make([]byte, 1)
	if _, err := tty.Read(buf); err != nil {
		return 0, err
	}
	if buf[0] == 0x1b {
		// drain the rest of a possible escape sequence (non-blocking-ish)
		_ = stty("min", "0", "time", "1")
		rest := make([]byte, 16)
		_, _ = tty.Read(rest)
	}
	return buf[0], nil
}

// waitAfterClose honours JUG_AFTER_CLOSE: wait until that window is gone
// before anything touches the tree.
func (c *cli) waitAfterClose() {
	v := os.Getenv("JUG_AFTER_CLOSE")
	if v == "" {
		return
	}
	os.Unsetenv("JUG_AFTER_CLOSE")
	var id int64
	fmt.Sscanf(v, "%d", &id)
	conn, err := c.app.Dial()
	if err != nil {
		return
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tree, err := conn.GetTree()
		if err != nil || tree.ByID(id) == nil {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- show / open / spawn

func (c *cli) show(args []string) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	noSwitch := fs.Bool("no-switch", false, "")
	underFocus := fs.Bool("under-focus", false, "show the parked workstream whose window is focused (in the lot)")
	if err := fs.Parse(args); err != nil || (fs.NArg() != 1) == !*underFocus {
		fmt.Fprintln(os.Stderr, "usage: jug show [--no-switch] WS | jug show --under-focus")
		return 2
	}
	var w *store.Workstream
	var err error
	if *underFocus {
		if w, err = c.app.UnderFocus(c.eng); err == nil && w == nil {
			err = errors.New("no workstream window is focused")
		}
	} else {
		w, err = c.app.Resolve(fs.Arg(0))
	}
	if err != nil {
		return fail(err)
	}
	return fail(c.app.Show(c.eng, w, *noSwitch))
}

func (c *cli) lot() int {
	err := c.app.Lot(c.eng)
	if errors.Is(err, app.ErrNothingParked) {
		info("juggler", "nothing is parked")
		return 0
	}
	return fail(err)
}

func (c *cli) current(args []string) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	w, err := c.app.Current(c.eng)
	if err != nil {
		return fail(err)
	}
	if w == nil {
		if asJSON {
			fmt.Println("null")
		}
		return 1
	}
	if asJSON {
		json.NewEncoder(os.Stdout).Encode(c.app.Info(w))
	} else {
		fmt.Println(w.Name())
	}
	return 0
}

func (c *cli) open(args []string) int {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: jug open [--ws WS] jira|pr|issue|URL")
		return 2
	}
	w, err := c.target(*ws)
	if err != nil {
		return fail(err)
	}
	what := fs.Arg(0)
	queued, err := c.app.Open(c.eng, w, what)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			return fail(fmt.Errorf("%s has no %q ref (attach one: jug ref add %s …)", w.ID, what, what))
		}
		return fail(err)
	}
	if queued {
		info("juggler", fmt.Sprintf("%s is not displayed; %s will open when it is", w.ID, what))
	}
	return 0
}

func (c *cli) term(args []string) int {
	fs := flag.NewFlagSet("term", flag.ContinueOnError)
	ws := wsFlag(fs)
	title := fs.String("title", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, err := c.target(*ws)
	if err != nil {
		return fail(err)
	}
	return fail(c.app.Term(c.eng, w, *title, strings.Join(fs.Args(), " ")))
}

func (c *cli) notes(args []string) int {
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, err := c.target(*ws)
	if err != nil {
		return fail(err)
	}
	return fail(c.app.Notes(c.eng, w))
}

func (c *cli) review(args []string) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, err := c.target(*ws)
	if err != nil {
		return fail(err)
	}
	return fail(c.app.Review(c.eng, w))
}

func (c *cli) focus(args []string) int {
	fs := flag.NewFlagSet("focus", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, err := c.target(*ws)
	if err != nil {
		return fail(err)
	}
	return fail(c.app.Focus(c.eng, w))
}

func (c *cli) dictate() int {
	w, _ := c.target("") // no workstream: dictator's default notes dir
	return fail(c.app.Dictate(w))
}

func (c *cli) close(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: jug close WS | jug close --displayed")
		return 2
	}
	q := args[0]
	if q == "--displayed" {
		q = c.eng.State.Displayed
		if q == "" {
			return fail(errors.New("nothing displayed"))
		}
	}
	w, err := c.app.Resolve(q)
	if err != nil {
		return fail(err)
	}
	return fail(c.app.Close(c.eng, w))
}

// ---------------------------------------------------------------- opencode sessions

func (c *cli) session(args []string) int {
	sub := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("session", flag.ContinueOnError)
	ws := wsFlag(fs)
	all := fs.Bool("all", false, "")
	relaunch := fs.Bool("relaunch", false, "")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	w, err := c.target(*ws)
	if err != nil {
		return fail(err)
	}
	finish := func() int {
		if *relaunch {
			return fail(c.app.SessionRelaunch(c.eng, w))
		}
		if c.eng.State.Displayed == w.Name() {
			fmt.Println("(opencode is running on the previous session; `jug session … --relaunch` or close it to restart on this one)")
		}
		return 0
	}
	switch sub {
	case "show":
		sess, err := c.app.SessionGet(w)
		if errors.Is(err, app.ErrNoSession) {
			fmt.Printf("%s: no session pinned (one is created on the next show)\n", w.ID)
			return 1
		}
		if err != nil {
			fmt.Printf("%s\t(not found in opencode: %v)\n", w.OpencodeSession, err)
			return 1
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", sess.ID, sess.Updated().Format("2006-01-02 15:04"), layout.ShortDir(sess.Directory), sess.Title)
		if *relaunch {
			return fail(c.app.SessionRelaunch(c.eng, w))
		}
		return 0
	case "unpin":
		return fail(c.app.SessionUnpin(w))
	case "pin":
		if len(pos) != 1 {
			fmt.Fprintln(os.Stderr, "usage: jug session pin ID [--relaunch]")
			return 2
		}
		if err := c.app.SessionPin(nil, w, pos[0], false); err != nil {
			return fail(err)
		}
		return finish()
	case "new":
		id, err := c.app.SessionNew(nil, w, false)
		if err != nil {
			return fail(err)
		}
		fmt.Println(id)
		return finish()
	case "pick":
		cands, err := c.app.SessionCandidates(w, *all)
		if err != nil {
			return fail(err)
		}
		if len(cands) == 0 {
			return fail(fmt.Errorf("no sessions mention %s (try --all)", w.ID))
		}
		var lines []string
		for _, s := range cands {
			mark := " "
			if s.ID == w.OpencodeSession {
				mark = "●"
			}
			lines = append(lines, fmt.Sprintf("%s %s  %-22s  %s", mark, s.Updated().Format("01-02 15:04"), trunc(layout.ShortDir(s.Directory), 22), s.Title))
		}
		idx, err := picker.Fuzzel(w.ID+" session> ", lines)
		if err != nil {
			return fail(err)
		}
		if idx < 0 || idx >= len(cands) {
			return fail(fmt.Errorf("bad selection %d", idx))
		}
		if err := c.app.SessionPin(nil, w, cands[idx].ID, false); err != nil {
			return fail(err)
		}
		fmt.Printf("%s\t%s\n", cands[idx].ID, cands[idx].Title)
		return finish()
	}
	fmt.Fprintf(os.Stderr, "jug session: unknown subcommand %q\n", sub)
	return 2
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

// ---------------------------------------------------------------- store verbs

func (c *cli) ls(args []string) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	infos, err := c.app.Infos()
	if err != nil {
		return fail(err)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(infos)
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATE\tID\tCATEGORY\tDESC\tJIRA\tPR\tTODO\tSESSION\tCODE DIR\tBRANCH")
	for _, in := range infos {
		state := "-"
		if in.State != "none" {
			state = in.State
		}
		jira, pr := "", ""
		for _, r := range in.Refs {
			label := r.Key
			if r.Status != "" {
				label += " (" + r.Status + ")"
			}
			switch {
			case r.Type == "jira" && jira == "":
				jira = label
			case r.Type == "pr" && pr == "":
				pr = label
			}
		}
		sess := "-"
		if n := len(in.OpencodeSession); n > 8 {
			sess = in.OpencodeSession[n-8:]
		}
		code := layout.ShortDir(in.CodePath)
		switch {
		case !in.HasCode:
			code = "(none — the workstream dir)"
		case in.CodeInside:
			code = "./" + in.CodeDir
		}
		branch := "-"
		if g := in.Git; g != nil {
			branch = g.Branch
			if branch == "" {
				branch = "@" + g.Head
			}
			if g.Dirty {
				branch += "*"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", state, in.ID, in.Category, in.Desc, jira, pr, in.TodosOpen, sess, code, branch)
	}
	tw.Flush()
	return 0
}

type wtOpts struct {
	o app.WorktreeOptions
}

func wtFlags(fs *flag.FlagSet) *wtOpts {
	w := &wtOpts{}
	fs.StringVar(&w.o.Repo, "repo", "", "repo name from config, or a path to a main checkout")
	fs.StringVar(&w.o.Branch, "branch", "", "branch for the worktree (default: the id, or <user>/<slug>)")
	fs.StringVar(&w.o.Base, "base", "", "branch to start a new branch from (default: the remote's HEAD)")
	fs.BoolVar(&w.o.NoFetch, "no-fetch", false, "do not fetch before creating")
	return w
}

func (c *cli) add(args []string) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	o := app.CreateOptions{}
	fs.StringVar(&o.Category, "category", "", "work|personal|… (default: work with --jira, else "+c.app.Cfg.DefaultCategory+")")
	fs.StringVar(&o.ID, "id", "", "")
	fs.StringVar(&o.CodeDir, "code-dir", "", "")
	fs.StringVar(&o.Jira, "jira", "", "")
	fs.StringVar(&o.PR, "pr", "", "")
	show := fs.Bool("show", false, "")
	wt := wtFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: jug add [--category C] [--id ID] [--jira KEY] [--pr URL] [--show] [--code-dir DIR | --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch]] DESC…")
		return 2
	}
	if o.CodeDir != "" && wt.o.Repo != "" {
		return fail(errors.New("--code-dir and --repo are mutually exclusive"))
	}
	o.Desc = strings.Join(fs.Args(), " ")
	w, err := c.app.Create(o)
	if err != nil {
		return fail(err)
	}
	fmt.Println(w.Dir)
	if wt.o.Repo != "" {
		if err := c.addWorktree(w, wt.o); err != nil {
			return fail(fmt.Errorf("%w (the workstream exists; retry with `jug repo add --ws %s …`)", err, w.ID))
		}
	}
	if *show {
		return run([]string{"show", w.Name()})
	}
	return 0
}

func (c *cli) addWorktree(w *store.Workstream, o app.WorktreeOptions) error {
	res, err := c.app.AddWorktree(w, o)
	if res.What != "" {
		fmt.Printf("%s: %s -> %s\n", w.ID, res.What, layout.ShortDir(res.Path))
	}
	if len(res.Seeded) > 0 {
		fmt.Printf("%s: seeded %s from %s\n", w.ID, strings.Join(res.Seeded, ", "), layout.ShortDir(c.app.Cfg.SeedDirFor(res.Repo)))
	}
	return err
}

func (c *cli) repo(args []string) int {
	if len(args) > 0 && args[0] == "seed" {
		fs := flag.NewFlagSet("repo seed", flag.ContinueOnError)
		ws := wsFlag(fs)
		force := fs.Bool("force", false, "overwrite files that already exist")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		w, err := c.target(*ws)
		if err != nil {
			return fail(err)
		}
		written, err := c.app.Seed(w, *force)
		if len(written) > 0 {
			fmt.Printf("%s: seeded %s\n", w.ID, strings.Join(written, ", "))
		}
		return fail(err)
	}
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(os.Stderr, "usage: jug repo add --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch] [--ws WS]\n       jug repo seed [--ws WS] [--force]")
		return 2
	}
	fs := flag.NewFlagSet("repo add", flag.ContinueOnError)
	ws := wsFlag(fs)
	wt := wtFlags(fs)
	if err := fs.Parse(args[1:]); err != nil || wt.o.Repo == "" {
		fmt.Fprintln(os.Stderr, "usage: jug repo add --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch] [--ws WS]")
		return 2
	}
	w, err := c.target(*ws)
	if err != nil {
		return fail(err)
	}
	old := w.ResolvedCodeDir()
	if err := c.addWorktree(w, wt.o); err != nil {
		return fail(err)
	}
	if st, err := store.LoadState(c.app.Cfg.StateDir); err == nil && st.Streams[w.Name()] != nil {
		fmt.Printf("note: windows already open for %s still use %s; `jug close %s` then show it to start them in the worktree\n", w.ID, layout.ShortDir(old), w.ID)
	}
	return 0
}

// rm closes the workstream's windows, removes its worktree and deletes its
// directory. Destructive: requires --yes.
func (c *cli) rm(args []string) int {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	force := fs.Bool("force", false, "remove the worktree even with uncommitted changes")
	pos, err := parseMixed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: jug rm WS --yes [--force]")
		return 2
	}
	w, err := c.app.Resolve(pos[0])
	if err != nil {
		return fail(err)
	}
	if !*yes {
		p := c.app.Plan(w)
		fmt.Printf("would remove %s:\n  windows: closed\n", p.Name)
		if p.HasWorktree {
			dirty := ""
			if p.Dirty != "" {
				dirty = ", DIRTY (" + p.Dirty + ") — needs --force"
			}
			fmt.Printf("  worktree: %s (branch %s%s) removed; the branch is kept\n", layout.ShortDir(p.Worktree), p.Branch, dirty)
		}
		fmt.Printf("  directory: %s deleted (TODO.md, notes/, …)\nre-run with --yes\n", layout.ShortDir(p.Dir))
		return 1
	}
	if err := c.app.Remove(c.eng, w, *force); err != nil {
		if errors.Is(err, app.ErrDirty) {
			return fail(fmt.Errorf("%v — use --force", err))
		}
		return fail(err)
	}
	fmt.Printf("removed %s\n", w.Name())
	return 0
}

func (c *cli) ref(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: jug ref add TYPE VALUE [--title T] [--status S] [--ws WS] | jug ref rm TYPE [KEY|URL] [--ws WS] | jug ref ls [--ws WS]")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("ref", flag.ContinueOnError)
	ws := wsFlag(fs)
	title := fs.String("title", "", "")
	status := fs.String("status", "", "")
	pos, err := parseMixed(fs, rest)
	if err != nil {
		return 2
	}
	w, err := c.target(*ws)
	if err != nil {
		return fail(err)
	}
	switch sub {
	case "ls":
		for _, r := range w.Refs {
			fmt.Printf("%s\t%s\t%s\t%s\n", r.Type, r.Key, r.Status, r.URL)
		}
		return 0
	case "add":
		if len(pos) != 2 {
			fmt.Fprintln(os.Stderr, "usage: jug ref add TYPE VALUE [--title T] [--status S] [--ws WS]")
			return 2
		}
		_, err := c.app.AddRef(w, pos[0], pos[1], *title, *status)
		return fail(err)
	case "rm":
		if len(pos) < 1 || len(pos) > 2 {
			fmt.Fprintln(os.Stderr, "usage: jug ref rm TYPE [KEY|URL] [--ws WS]")
			return 2
		}
		ident := ""
		if len(pos) == 2 {
			ident = pos[1]
		}
		return fail(c.app.RemoveRef(w, pos[0], ident))
	}
	fmt.Fprintf(os.Stderr, "jug ref: unknown subcommand %q\n", sub)
	return 2
}

func (c *cli) env(args []string) int {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, err := c.target(*ws)
	if err != nil {
		return fail(err)
	}
	env := c.app.Env(w)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("export %s=%s\n", k, layout.ShellQuote(env[k]))
	}
	return 0
}

// ---------------------------------------------------------------- watch / doctor / serve

func (c *cli) watch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	format := fs.String("format", "plain", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	render, ok := bar.Renderers[*format]
	if !ok {
		return fail(fmt.Errorf("unknown --format %q (plain|json|waybar)", *format))
	}
	return bar.Watch(c.app.Cfg, c.app.Store, render)
}

func (c *cli) doctor() int {
	for _, ch := range c.app.Doctor() {
		mark := "ok  "
		if !ch.OK {
			mark = "FAIL"
		}
		fmt.Printf("%s  %-28s %s\n", mark, ch.What, ch.Detail)
	}
	fmt.Printf("config: %s\n", config.Path())
	return 0
}

func (c *cli) serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", c.app.Cfg.Listen, "address to listen on (loopback only is wise: the API runs commands as you)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	srv := web.New(c.app)
	fmt.Printf("juggler %s: web UI and REST API on http://%s/  (root %s)\n", version, *listen, layout.ShortDir(c.app.Cfg.Root))
	return fail(srv.ListenAndServe(*listen))
}
