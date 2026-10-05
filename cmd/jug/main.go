// jug is the juggler CLI: workstreams for sway.
//
//	jug toggle                      make the focused workspace the workstream slot (or release it)
//	jug pick                        choose a workstream (fuzzel) and show it
//	jug show <ws>                   show a workstream in the slot
//	jug park                        hide the displayed workstream (windows stay alive)
//	jug open jira|pr|<url>          browser window in the right stack (focus if already open)
//	jug term [-- cmd…]              terminal in the right stack
//	jug notes | review | focus      TODO.md / diff in the editor / focus opencode
//	jug add … | ls | current | ref  manage workstreams
//	jug watch --format waybar       stream the displayed workstream to the bar
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/varlogtim/juggler/internal/bar"
	"github.com/varlogtim/juggler/internal/config"
	"github.com/varlogtim/juggler/internal/gitwt"
	"github.com/varlogtim/juggler/internal/layout"
	"github.com/varlogtim/juggler/internal/oc"
	"github.com/varlogtim/juggler/internal/picker"
	"github.com/varlogtim/juggler/internal/seed"
	"github.com/varlogtim/juggler/internal/store"
	"github.com/varlogtim/juggler/internal/sway"
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
  ls [--json]            list workstreams
  add [--category C] [--id ID] [--jira KEY] [--pr URL] [--show] DESC…
      [--code-dir DIR | --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch]]
                         create <C>_<ID>_<slug>/ under the root (default C: personal, ID: tim-NNNN);
                         --repo gives it its own git worktree at <ws>/src/<repo> (branch default:
                         the id for ticket ids, else <user>/<slug>; base: the remote's HEAD)
  repo add --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch] [--ws WS]
                         add such a worktree to an existing workstream and point code_dir at it
  repo seed [--ws WS] [--force]
                         (re)copy the repo's seed files (~/.config/juggler/seed/<repo>/: .envrc etc.,
                         templated with {{id}} {{name}} {{repo}} {{dir}} {{code_dir}} {{home}}) into the
                         worktree; done automatically on add. A seeded .envrc is direnv-allowed.
  set WS [--category C] [--id ID] [--desc D] [--jira KEY]
                         rename / recategorize (the directory follows); windows are closed and, if it
                         was displayed, reopened — opencode resumes its pinned session. --jira attaches
                         the ticket and, unless given, sets the id to it and the category to work
  rm WS --yes [--force]  close its windows, remove its worktree (refuses if dirty unless --force;
                         the branch is kept) and delete the workstream directory
  ref add TYPE VALUE [--title T] [--status S] [--ws WS]
                         attach a ref: jira KEY | pr URL | issue URL | url URL
  close WS               kill the workstream's windows (files are kept)

misc
  watch [--format plain|json|waybar]
  doctor                 check sway, tools, workspace naming
  version | help

Workstream names resolve by canonical name (work_AISW-1_foo), id (AISW-1) or unique prefix.
Config: ~/.config/juggler/config.toml   State: ~/.local/state/juggler/   Store: ~/workstreams/
`

func main() { os.Exit(run(os.Args[1:])) }

type app struct {
	cfg   config.Config
	store store.Store
	eng   *layout.Engine
	debug bool
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
	a := &app{cfg: cfg, store: store.Store{Root: cfg.Root}, debug: os.Getenv("JUG_DEBUG") != ""}

	switch cmd {
	case "watch":
		return a.watch(rest)
	case "ls":
		return a.ls(rest)
	case "add":
		return a.add(rest)
	case "env":
		return a.env(rest)
	case "ref":
		return a.ref(rest)
	case "repo":
		return a.repo(rest)
	case "doctor":
		return a.doctor()
	}

	// everything else talks to sway
	conn, err := sway.Dial()
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	st, err := store.LoadState(cfg.StateDir)
	if err != nil {
		return fail(err)
	}
	a.eng = &layout.Engine{Sway: conn, Cfg: cfg, Store: a.store, State: st}
	if a.debug {
		a.eng.Debug = func(f string, v ...any) { fmt.Fprintf(os.Stderr, "jug: "+f+"\n", v...) }
	}
	a.waitAfterClose()
	if st.Slot != nil {
		if err := a.eng.Reconcile(); err != nil {
			return fail(err)
		}
	}

	switch cmd {
	case "toggle":
		return a.toggle()
	case "pick":
		return a.pick(rest)
	case "menu":
		return a.menu(rest)
	case "show":
		return a.show(rest)
	case "park":
		if len(rest) == 1 && rest[0] == "--others" {
			return fail(a.eng.ParkStrays())
		}
		return fail(a.eng.Park())
	case "lot":
		return a.lot()
	case "current":
		return a.current(rest)
	case "open":
		return a.open(rest)
	case "term":
		return a.term(rest)
	case "notes":
		return a.notes(rest)
	case "review":
		return a.review(rest)
	case "focus":
		return a.focus(rest)
	case "dictate":
		return a.dictate(rest)
	case "session":
		return a.session(rest)
	case "close":
		return a.close(rest)
	case "rm":
		return a.rm(rest)
	case "set":
		return a.set(rest)
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

// ---------------------------------------------------------------- slot verbs

func (a *app) toggle() int {
	on, err := a.eng.Toggle()
	if err != nil {
		return fail(err)
	}
	if on {
		info("juggler", fmt.Sprintf("workspace %d is now the workstream slot", a.eng.State.Slot.Num))
	} else {
		info("juggler", "slot released")
	}
	return 0
}

func (a *app) entries() ([]picker.Entry, error) {
	all, err := a.store.List()
	if err != nil {
		return nil, err
	}
	tree, err := a.eng.Sway.GetTree()
	if err != nil {
		return nil, err
	}
	var es []picker.Entry
	for _, w := range all {
		live, _ := a.eng.IsLive(tree, w)
		e := picker.Entry{W: w, Live: live, Displayed: a.eng.State.Displayed == w.Name()}
		if s := a.eng.State.Streams[w.Name()]; s != nil {
			e.Shown, _ = time.Parse(time.RFC3339, s.Shown)
		}
		es = append(es, e)
	}
	return es, nil
}

func (a *app) pick(args []string) int {
	fs := flag.NewFlagSet("pick", flag.ContinueOnError)
	print := fs.Bool("print", false, "print the picker rows instead of showing the picker")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	es, err := a.entries()
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
		return a.pickNew()
	}
	return fail(a.eng.Show(ordered[idx].W, layout.ShowOptions{}))
}

// knownCategories is what a "category:" prefix may name in the picker.
func (a *app) knownCategories() map[string]bool {
	known := map[string]bool{"work": true, "personal": true, a.cfg.DefaultCategory: true}
	if all, err := a.store.List(); err == nil {
		for _, w := range all {
			known[w.Category] = true
		}
	}
	return known
}

// pickNew is the picker's "+ new workstream…" flow: one line of text
// ("[category:] [TICKET-1] description"), then where the code lives
// (a worktree of a configured repo, none, or an existing directory).
// Nothing is created until both answers are in; Esc anywhere cancels.
func (a *app) pickNew() int {
	text, err := picker.Prompt("new  [work:|personal:] [AISW-123] description> ")
	if err != nil {
		return fail(err)
	}
	spec := picker.ParseSpec(text, a.knownCategories())
	if spec.Desc == "" {
		if spec.Key == "" {
			return fail(errors.New("a description is required"))
		}
		if spec.Desc, err = picker.Prompt(spec.Key + " description> "); err != nil {
			return fail(err)
		}
	}
	category := a.categoryFor(spec.Category, spec.Key)
	id := strings.ToUpper(spec.Key)
	if id == "" {
		if id, err = a.store.NextManualID(a.cfg.IDPrefix); err != nil {
			return fail(err)
		}
	}
	probe := &store.Workstream{ID: id, Desc: spec.Desc, Category: category}

	// where does the code live?
	var repos []string
	for name := range a.cfg.Repos {
		repos = append(repos, name)
	}
	sort.Strings(repos)
	var lines []string
	for _, r := range repos {
		lines = append(lines, fmt.Sprintf("worktree of %-16s branch %s", r, defaultBranch(probe)))
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

	w, err := a.create(category, id, spec.Desc, codeDir, spec.Key, "")
	if err != nil {
		return fail(err)
	}
	if choice < len(repos) {
		if err := a.addWorktree(w, &wtOpts{repo: repos[choice]}); err != nil {
			layout.Notify("critical", "juggler", fmt.Sprintf("%s created without code: %v\nretry: jug repo add --ws %s --repo %s", w.ID, err, w.ID, repos[choice]))
		}
	}
	return fail(a.eng.Show(w, layout.ShowOptions{}))
}

// set renames / recategorizes a workstream. The directory name is derived
// from category, id and description, so changing any of them moves the
// directory; live windows are closed first (opencode resumes its pinned
// session) and the workstream is shown again if it was displayed.
func (a *app) set(args []string) int {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	category := fs.String("category", "", "")
	id := fs.String("id", "", "")
	desc := fs.String("desc", "", "")
	jira := fs.String("jira", "", "attach this ticket; also sets --id (and --category work) unless given")
	pos, err := parseMixed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: jug set WS [--category C] [--id ID] [--desc D] [--jira KEY]")
		return 2
	}
	w, err := a.store.Resolve(pos[0])
	if err != nil {
		return fail(err)
	}
	oldName, oldDir, oldTitle := w.Name(), w.Dir, w.SessionTitle()
	if *jira != "" {
		key := strings.ToUpper(*jira)
		if *id == "" && !strings.EqualFold(w.ID, key) {
			*id = key
		}
		if *category == "" && w.Category == a.cfg.DefaultCategory {
			*category = "work"
		}
		u, err := a.jiraURL(key)
		if err != nil {
			return fail(err)
		}
		if r := w.Ref("jira"); r != nil {
			r.Key, r.URL = key, u
		} else {
			w.Refs = append(w.Refs, store.Ref{Type: "jira", Key: key, URL: u})
		}
	}
	if *category != "" {
		w.Category = *category
	}
	if *id != "" {
		w.ID = *id
	}
	if *desc != "" {
		w.Desc = *desc
	}
	newDir := filepath.Join(a.cfg.Root, store.DirName(w.Category, w.ID, w.Desc))
	if newDir == oldDir {
		return fail(a.store.Save(w)) // only refs changed
	}
	if _, err := os.Stat(newDir); err == nil {
		return fail(fmt.Errorf("%s already exists", newDir))
	}

	tree, err := a.eng.Sway.GetTree()
	if err != nil {
		return fail(err)
	}
	wasDisplayed := a.eng.State.Displayed == oldName
	if tree.ByMark(layout.RootMark(oldName)) != nil {
		w.Dir = oldDir
		if err := a.eng.Close(&store.Workstream{Dir: oldDir}); err != nil {
			return fail(err)
		}
		// let the windows go before anything is launched in the new place
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if t, err := a.eng.Sway.GetTree(); err == nil && t.ByMark(layout.RootMark(oldName)) == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return fail(err)
	}
	w.Dir = newDir
	if err := a.store.Save(w); err != nil {
		return fail(err)
	}
	fmt.Printf("%s -> %s\n", oldName, w.Name())
	code := w.ResolvedCodeDir()
	if w.CodeInside() && gitwt.IsLinkedWorktree(code) {
		if err := gitwt.Repair(code); err != nil {
			fmt.Fprintln(os.Stderr, "jug: git worktree repair:", err)
		}
		if err := seed.DirenvAllow(code); err != nil {
			fmt.Fprintln(os.Stderr, "jug:", err)
		}
	}
	if w.SessionTitle() != oldTitle {
		if err := a.eng.RetitleSession(w); err != nil {
			fmt.Fprintln(os.Stderr, "jug: opencode session title not updated:", err)
		}
	}
	if wasDisplayed {
		return fail(a.eng.Show(w, layout.ShowOptions{NoSwitch: true}))
	}
	return 0
}

// menuAction is one row of `jug menu`.
type menuAction struct {
	key  byte
	name string
	desc string
	run  func() int
}

// menuActions returns the actions that apply right now. w is the displayed
// workstream (nil if none); under is the parked workstream under focus.
func (a *app) menuActions(w *store.Workstream, under *store.Workstream, lotExists bool) []menuAction {
	var rows []menuAction
	add := func(k byte, name, desc string, run func() int) {
		rows = append(rows, menuAction{k, name, desc, run})
	}
	if under != nil && (w == nil || under.Name() != w.Name()) {
		u := under
		add('g', "go", "show "+u.ID+" (the parked workstream under focus)", func() int { return fail(a.eng.Show(u, layout.ShowOptions{})) })
	}
	if w != nil {
		add('t', "term", "new terminal in the right stack", func() int { return a.term(nil) })
		if w.Ref("jira") != nil {
			add('j', "jira", "open the ticket in a browser window in the right stack", func() int { return a.open([]string{"jira"}) })
		}
		if w.Ref("pr") != nil {
			add('p', "pr", "open the pull request in a browser window in the right stack", func() int { return a.open([]string{"pr"}) })
		}
		add('n', "notes", "edit TODO.md in the right stack", func() int { return a.notes(nil) })
		add('r', "review", "read the uncommitted diff in the editor, right stack", func() int { return a.review(nil) })
		add('o', "opencode", "focus the opencode window", func() int { return a.focus(nil) })
		add('d', "dictate", "toggle dictation notes into the workstream", func() int { return a.dictate(nil) })
	}
	add('w', "pick", "switch to another workstream", func() int { return a.pick(nil) })
	if lotExists {
		add('l', "lot", "visit the lot (parked workstreams) / come back", a.lot)
	}
	if w != nil {
		add('x', "park", "hide the displayed workstream (windows stay alive)", func() int { return fail(a.eng.Park()) })
	}
	add('s', "slot", "toggle this workspace as the workstream slot", a.toggle)
	if w != nil {
		ww := w
		add('c', "close", "kill this workstream's windows (files are kept)", func() int { return fail(a.eng.Close(ww)) })
	}
	return rows
}

// menu is the single-key action menu. From a hotkey (no tty) it opens itself
// in a floating terminal window; in a terminal it draws the rows, waits for
// ONE key and runs that action. Inside the menu window the action is started
// detached and runs once the window is gone, so focus is already back where
// it belongs.
func (a *app) menu(args []string) int {
	fs := flag.NewFlagSet("menu", flag.ContinueOnError)
	print := fs.Bool("print", false, "print the rows instead of showing the menu")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var w *store.Workstream
	if a.eng.State.Displayed != "" {
		w, _ = a.store.Load(filepath.Join(a.cfg.Root, a.eng.State.Displayed))
	}
	tree, err := a.eng.Sway.GetTree()
	if err != nil {
		return fail(err)
	}
	var under *store.Workstream
	if n := layout.RootUnderFocus(tree); n != "" {
		under, _ = a.store.Load(filepath.Join(a.cfg.Root, n))
	}
	rows := a.menuActions(w, under, tree.WorkspaceNamed(a.eng.LotName()) != nil)

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
			a.cfg.Terminal, layout.MenuAppID, cols, rowsN, shq(os.Args[0]))
		_, err := a.eng.Sway.Command("exec " + cmd)
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
	argv := menuArgv(chosen.name)
	child := exec.Command(os.Args[0], argv...)
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

// waitAfterClose honours JUG_AFTER_CLOSE: wait until that window is gone.
func (a *app) waitAfterClose() {
	v := os.Getenv("JUG_AFTER_CLOSE")
	if v == "" {
		return
	}
	os.Unsetenv("JUG_AFTER_CLOSE")
	var id int64
	fmt.Sscanf(v, "%d", &id)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tree, err := a.eng.Sway.GetTree()
		if err != nil || tree.ByID(id) == nil {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func (a *app) show(args []string) int {
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
		tree, terr := a.eng.Sway.GetTree()
		if terr != nil {
			return fail(terr)
		}
		n := layout.RootUnderFocus(tree)
		if n == "" {
			return fail(errors.New("no workstream window is focused"))
		}
		w, err = a.store.Load(filepath.Join(a.cfg.Root, n))
	} else {
		w, err = a.store.Resolve(fs.Arg(0))
	}
	if err != nil {
		return fail(err)
	}
	return fail(a.eng.Show(w, layout.ShowOptions{NoSwitch: *noSwitch}))
}

func (a *app) lot() int {
	err := a.eng.Lot()
	if errors.Is(err, layout.ErrNothingParked) {
		info("juggler", "nothing is parked")
		return 0
	}
	return fail(err)
}

func (a *app) current(args []string) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	if a.eng.State.Displayed == "" {
		if asJSON {
			fmt.Println("null")
		}
		return 1
	}
	w, err := a.store.Load(filepath.Join(a.cfg.Root, a.eng.State.Displayed))
	if err != nil {
		return fail(err)
	}
	if asJSON {
		json.NewEncoder(os.Stdout).Encode(w)
	} else {
		fmt.Println(w.Name())
	}
	return 0
}

// target resolves --ws / $JUG_WORKSTREAM / displayed.
func (a *app) target(explicit string) (*store.Workstream, error) {
	q := explicit
	if q == "" {
		q = os.Getenv("JUG_WORKSTREAM")
	}
	if q == "" {
		q = a.eng.State.Displayed
	}
	if q == "" {
		return nil, errors.New("no workstream displayed and none given (--ws)")
	}
	return a.store.Resolve(q)
}

func wsFlag(fs *flag.FlagSet) *string {
	p := fs.String("ws", "", "workstream (default: $JUG_WORKSTREAM, else displayed)")
	fs.StringVar(p, "workstream", "", "")
	return p
}

func (a *app) open(args []string) int {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: jug open [--ws WS] jira|pr|issue|URL")
		return 2
	}
	w, err := a.target(*ws)
	if err != nil {
		return fail(err)
	}
	what := fs.Arg(0)
	key, url := what, ""
	if strings.Contains(what, "://") {
		key, url = "url:"+what, what
	} else if r := w.Ref(what); r != nil {
		url = r.URL
	} else {
		return fail(fmt.Errorf("%s has no %q ref (attach one: jug ref add %s …)", w.ID, what, what))
	}
	queued, err := a.eng.Open(w, key, url)
	if err != nil {
		return fail(err)
	}
	if queued {
		info("juggler", fmt.Sprintf("%s is not displayed; %s will open when it is", w.ID, what))
	}
	return 0
}

func (a *app) term(args []string) int {
	fs := flag.NewFlagSet("term", flag.ContinueOnError)
	ws := wsFlag(fs)
	title := fs.String("title", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, err := a.target(*ws)
	if err != nil {
		return fail(err)
	}
	command := ""
	if fs.NArg() > 0 {
		command = strings.Join(fs.Args(), " ")
	}
	return fail(a.eng.Spawn(w, *title, command))
}

func (a *app) notes(args []string) int {
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, err := a.target(*ws)
	if err != nil {
		return fail(err)
	}
	return fail(a.eng.Spawn(w, w.ID+" notes", a.cfg.Editor+" "+shq(layout.TodoFile(w))))
}

func (a *app) review(args []string) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, err := a.target(*ws)
	if err != nil {
		return fail(err)
	}
	// staged + unstaged changes vs HEAD, read-only in the editor
	command := fmt.Sprintf(`cd %s && { git diff HEAD --stat; echo; git diff HEAD; } | %s -R -c "set ft=diff nomodified" -`,
		shq(w.ResolvedCodeDir()), a.cfg.Editor)
	return fail(a.eng.Spawn(w, w.ID+" review", command))
}

func (a *app) focus(args []string) int {
	fs := flag.NewFlagSet("focus", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w, err := a.target(*ws)
	if err != nil {
		return fail(err)
	}
	return fail(a.eng.FocusOpencode(w))
}

func (a *app) dictate(args []string) int {
	if a.cfg.Dictator == "" {
		return fail(errors.New("dictator is disabled in config"))
	}
	w, err := a.target("")
	var argv []string
	if err != nil {
		argv = []string{"-notify", "toggle", "notes"} // no workstream: default notes dir
	} else {
		argv = []string{"-notify", "-dir", w.NotesDir(), "toggle", "notes"}
	}
	c := exec.Command(a.cfg.Dictator, argv...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return fail(c.Run())
}

// ---------------------------------------------------------------- opencode sessions

func (a *app) session(args []string) int {
	sub := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("session", flag.ContinueOnError)
	ws := wsFlag(fs)
	all := fs.Bool("all", false, "")
	relaunch := fs.Bool("relaunch", false, "")
	var pos []string
	for len(args) > 0 {
		if strings.HasPrefix(args[0], "-") {
			if err := fs.Parse(args); err != nil {
				return 2
			}
			pos = append(pos, fs.Args()...)
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	w, err := a.target(*ws)
	if err != nil {
		return fail(err)
	}
	finish := func() int {
		if *relaunch {
			return fail(a.eng.RelaunchOpencode(w))
		}
		if a.eng.State.Displayed == w.Name() {
			fmt.Println("(opencode is running on the previous session; `jug session … --relaunch` or close it to restart on this one)")
		}
		return 0
	}
	switch sub {
	case "show":
		if w.OpencodeSession == "" {
			fmt.Printf("%s: no session pinned (one is created on the next show)\n", w.ID)
			return 1
		}
		srv, err := a.ocServer(w)
		if err != nil {
			fmt.Println(w.OpencodeSession)
			return fail(err)
		}
		defer srv.Stop()
		sess, err := srv.Get(w.ResolvedCodeDir(), w.OpencodeSession)
		if err != nil {
			fmt.Printf("%s\t(not found in opencode: %v)\n", w.OpencodeSession, err)
			return 1
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", sess.ID, sess.Updated().Format("2006-01-02 15:04"), layout.ShortDir(sess.Directory), sess.Title)
		if *relaunch {
			srv.Stop()
			return fail(a.eng.RelaunchOpencode(w))
		}
		return 0
	case "unpin":
		w.OpencodeSession = ""
		return fail(a.store.Save(w))
	case "pin":
		if len(pos) != 1 {
			fmt.Fprintln(os.Stderr, "usage: jug session pin ID [--relaunch]")
			return 2
		}
		w.OpencodeSession = pos[0]
		if err := a.store.Save(w); err != nil {
			return fail(err)
		}
		return finish()
	case "new":
		id, err := a.eng.NewSession(w)
		if err != nil {
			return fail(err)
		}
		fmt.Println(id)
		return finish()
	case "pick":
		srv, err := a.ocServer(w)
		if err != nil {
			return fail(err)
		}
		defer srv.Stop()
		home, _ := os.UserHomeDir()
		dirs := []string{w.ResolvedCodeDir()}
		if home != "" && home != w.ResolvedCodeDir() {
			dirs = append(dirs, home) // sessions started from ~ live in the "global" project
		}
		seen := map[string]bool{}
		var cands []oc.Session
		for _, d := range dirs {
			list, err := srv.List(d, "", 200)
			if err != nil {
				return fail(err)
			}
			for _, sess := range list {
				if seen[sess.ID] {
					continue
				}
				seen[sess.ID] = true
				if *all || a.sessionMatches(w, sess) {
					cands = append(cands, sess)
				}
			}
		}
		if len(cands) == 0 {
			return fail(fmt.Errorf("no sessions mention %s (try --all)", w.ID))
		}
		sortSessions(cands)
		var lines []string
		for _, c := range cands {
			mark := " "
			if c.ID == w.OpencodeSession {
				mark = "●"
			}
			lines = append(lines, fmt.Sprintf("%s %s  %-22s  %s", mark, c.Updated().Format("01-02 15:04"), trunc(layout.ShortDir(c.Directory), 22), c.Title))
		}
		idx, err := picker.Fuzzel(w.ID+" session> ", lines)
		if err != nil {
			return fail(err)
		}
		if idx < 0 || idx >= len(cands) {
			return fail(fmt.Errorf("bad selection %d", idx))
		}
		w.OpencodeSession = cands[idx].ID
		if err := a.store.Save(w); err != nil {
			return fail(err)
		}
		fmt.Printf("%s	%s\n", cands[idx].ID, cands[idx].Title)
		return finish()
	}
	fmt.Fprintf(os.Stderr, "jug session: unknown subcommand %q\n", sub)
	return 2
}

// sessionMatches reports whether a session's title mentions the
// workstream: its id, any ref key, or (for manual ids) its description.
func (a *app) sessionMatches(w *store.Workstream, s oc.Session) bool {
	t := strings.ToLower(s.Title)
	if strings.Contains(t, strings.ToLower(w.ID)) {
		return true
	}
	for _, r := range w.Refs {
		if r.Key != "" && strings.Contains(t, strings.ToLower(r.Key)) {
			return true
		}
	}
	if strings.HasPrefix(w.ID, "tim-") && w.Desc != "" && strings.Contains(t, strings.ToLower(w.Desc)) {
		return true
	}
	return false
}

func sortSessions(ss []oc.Session) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j].Time.Updated > ss[j-1].Time.Updated; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

// ocServer starts a transient opencode server rooted at w's code dir.
func (a *app) ocServer(w *store.Workstream) (*oc.Server, error) {
	bin, err := oc.Binary(a.cfg.Opencode)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	_ = cancel // the server is stopped explicitly by the caller
	return oc.Start(ctx, bin, w.ResolvedCodeDir())
}

func (a *app) close(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: jug close WS | jug close --displayed")
		return 2
	}
	q := args[0]
	if q == "--displayed" {
		q = a.eng.State.Displayed
		if q == "" {
			return fail(errors.New("nothing displayed"))
		}
	}
	w, err := a.store.Resolve(q)
	if err != nil {
		return fail(err)
	}
	return fail(a.eng.Close(w))
}

// ---------------------------------------------------------------- store verbs

func (a *app) ls(args []string) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	all, err := a.store.List()
	if err != nil {
		return fail(err)
	}
	if asJSON {
		json.NewEncoder(os.Stdout).Encode(all)
		return 0
	}
	st, _ := store.LoadState(a.cfg.StateDir)
	var tree *sway.Node
	if c, err := sway.Dial(); err == nil {
		tree, _ = c.GetTree()
		c.Close()
	}
	probe := &layout.Engine{Cfg: a.cfg, State: st}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATE\tID\tCATEGORY\tDESC\tJIRA\tPR\tTODO\tSESSION\tCODE DIR\tBRANCH")
	for _, w := range all {
		state := "-"
		if tree != nil && st != nil {
			if live, parked := probe.IsLive(tree, w); live {
				state = "parked"
				if !parked {
					state = "shown"
				}
			}
		}
		if st != nil && st.Displayed == w.Name() {
			state = "displayed"
		}
		jira, pr := "", ""
		if r := w.Ref("jira"); r != nil {
			jira = r.Key
			if r.Status != "" {
				jira += " (" + r.Status + ")"
			}
		}
		if r := w.Ref("pr"); r != nil {
			pr = r.Key
			if r.Status != "" {
				pr += " (" + r.Status + ")"
			}
		}
		sess := "-"
		if w.OpencodeSession != "" {
			sess = w.OpencodeSession[len(w.OpencodeSession)-8:]
		}
		code := layout.ShortDir(w.ResolvedCodeDir())
		switch {
		case w.CodeDir == "":
			code = "(none — the workstream dir)"
		case w.CodeInside():
			code = "./" + w.CodeDir
		}
		branch := "-"
		if in, err := gitwt.Inspect(w.ResolvedCodeDir()); err == nil {
			branch = in.Branch
			if branch == "" {
				branch = "@" + in.Head
			}
			if in.Dirty {
				branch += "*"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", state, w.ID, w.Category, w.Desc, jira, pr, w.OpenTodos(), sess, code, branch)
	}
	tw.Flush()
	return 0
}

func (a *app) add(args []string) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	category := fs.String("category", "", "work|personal|… (default: work with --jira, else "+a.cfg.DefaultCategory+")")
	id := fs.String("id", "", "")
	codeDir := fs.String("code-dir", "", "")
	jira := fs.String("jira", "", "")
	pr := fs.String("pr", "", "")
	show := fs.Bool("show", false, "")
	wt := wtFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: jug add [--category C] [--id ID] [--jira KEY] [--pr URL] [--show] [--code-dir DIR | --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch]] DESC…")
		return 2
	}
	if *codeDir != "" && wt.repo != "" {
		return fail(errors.New("--code-dir and --repo are mutually exclusive"))
	}
	w, err := a.create(a.categoryFor(*category, *jira), *id, strings.Join(fs.Args(), " "), *codeDir, *jira, *pr)
	if err != nil {
		return fail(err)
	}
	fmt.Println(w.Dir)
	if wt.repo != "" {
		if err := a.addWorktree(w, wt); err != nil {
			return fail(fmt.Errorf("%w (the workstream exists; retry with `jug repo add --ws %s …`)", err, w.ID))
		}
	}
	if *show {
		return run([]string{"show", w.Name()})
	}
	return 0
}

// resolveWS resolves --ws / $JUG_WORKSTREAM / the displayed workstream
// without needing a sway connection.
func (a *app) resolveWS(explicit string) (*store.Workstream, error) {
	q := explicit
	if q == "" {
		q = os.Getenv("JUG_WORKSTREAM")
	}
	if q == "" {
		if st, err := store.LoadState(a.cfg.StateDir); err == nil {
			q = st.Displayed
		}
	}
	if q == "" {
		return nil, errors.New("no workstream given (--ws) and none displayed")
	}
	return a.store.Resolve(q)
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

// ---------------------------------------------------------------- worktrees

type wtOpts struct {
	repo, branch, base string
	noFetch            bool
}

func wtFlags(fs *flag.FlagSet) *wtOpts {
	o := &wtOpts{}
	fs.StringVar(&o.repo, "repo", "", "repo name from config, or a path to a main checkout")
	fs.StringVar(&o.branch, "branch", "", "branch for the worktree (default: the id, or <user>/<slug>)")
	fs.StringVar(&o.base, "base", "", "branch to start a new branch from (default: the remote's HEAD)")
	fs.BoolVar(&o.noFetch, "no-fetch", false, "do not fetch before creating")
	return o
}

var ticketID = regexp.MustCompile(`^[A-Z][A-Z0-9]+-[0-9]+$`)

// defaultBranch names the workstream's branch: the ticket key as-is, else
// <user>/<slug>.
func defaultBranch(w *store.Workstream) string {
	if ticketID.MatchString(w.ID) {
		return w.ID
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "me"
	}
	return user + "/" + store.Slug(w.Desc)
}

// resolveRepo turns --repo into (name, main checkout, remote, default base).
func (a *app) resolveRepo(q string) (name, main, remote, base string, err error) {
	if r, ok := a.cfg.Repos[q]; ok {
		return q, r.Path, r.Remote, r.DefaultBranch, nil
	}
	p := config.Expand(q)
	if abs, e := filepath.Abs(p); e == nil {
		p = abs
	}
	if !gitwt.IsRepo(p) {
		if len(a.cfg.Repos) > 0 {
			var names []string
			for k := range a.cfg.Repos {
				names = append(names, k)
			}
			return "", "", "", "", fmt.Errorf("%q is neither a configured repo (%s) nor a git checkout", q, strings.Join(names, ", "))
		}
		return "", "", "", "", fmt.Errorf("%q is not a git checkout (configure [repos] in %s to use short names)", q, config.Path())
	}
	top, e := gitwt.Toplevel(p)
	if e != nil {
		return "", "", "", "", e
	}
	mainRepo, e := gitwt.MainRepo(top)
	if e != nil {
		return "", "", "", "", e
	}
	return filepath.Base(mainRepo), mainRepo, "origin", "", nil
}

// addWorktree creates <ws>/<code_subdir>/<repo> as a worktree and points
// code_dir at it.
func (a *app) addWorktree(w *store.Workstream, o *wtOpts) error {
	name, main, remote, base, err := a.resolveRepo(o.repo)
	if err != nil {
		return err
	}
	if o.base != "" {
		base = o.base
	}
	branch := o.branch
	if branch == "" {
		branch = defaultBranch(w)
	}
	rel := filepath.Join(a.cfg.CodeSubdir, name)
	path := filepath.Join(w.Dir, rel)
	what, err := gitwt.Add(main, path, branch, gitwt.AddOptions{Remote: remote, Base: base, Fetch: !o.noFetch})
	if err != nil {
		return err
	}
	w.CodeDir = rel
	if err := a.store.Save(w); err != nil {
		return err
	}
	fmt.Printf("%s: %s -> %s\n", w.ID, what, layout.ShortDir(path))
	return a.seedWorktree(w, name, false)
}

// seedWorktree copies the repo's seed files into w's code dir (templated)
// and trusts a seeded .envrc with direnv.
func (a *app) seedWorktree(w *store.Workstream, repoName string, force bool) error {
	sd := a.cfg.SeedDirFor(repoName)
	if sd == "" {
		return nil
	}
	home, _ := os.UserHomeDir()
	written, err := seed.Apply(sd, w.ResolvedCodeDir(), seed.Vars{
		ID: w.ID, Name: w.Name(), Repo: repoName, Dir: w.Dir, CodeDir: w.ResolvedCodeDir(), Home: home,
	}, force)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if len(written) > 0 {
		fmt.Printf("%s: seeded %s from %s\n", w.ID, strings.Join(written, ", "), layout.ShortDir(sd))
	}
	for _, rel := range written {
		if rel == ".envrc" {
			if err := seed.DirenvAllow(w.ResolvedCodeDir()); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedPaths lists the relative paths the repo's seed dir would put into w's
// worktree — untracked files that do not count as "dirty".
func (a *app) seedPaths(w *store.Workstream) []string {
	name := a.repoNameOf(w)
	sd := a.cfg.SeedDirFor(name)
	if sd == "" {
		return nil
	}
	var paths []string
	_ = filepath.WalkDir(sd, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(sd, p)
			paths = append(paths, rel)
		}
		return nil
	})
	return paths
}

// repoNameOf returns the configured repo name whose main checkout owns w's
// code dir, else the main checkout's base name.
func (a *app) repoNameOf(w *store.Workstream) string {
	mainRepo, err := gitwt.MainRepo(w.ResolvedCodeDir())
	if err != nil {
		return ""
	}
	for name, r := range a.cfg.Repos {
		if r.Path == mainRepo {
			return name
		}
	}
	return filepath.Base(mainRepo)
}

func (a *app) repo(args []string) int {
	if len(args) > 0 && args[0] == "seed" {
		fs := flag.NewFlagSet("repo seed", flag.ContinueOnError)
		ws := wsFlag(fs)
		force := fs.Bool("force", false, "overwrite files that already exist")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		w, err := a.resolveWS(*ws)
		if err != nil {
			return fail(err)
		}
		name := a.repoNameOf(w)
		if name == "" || a.cfg.SeedDirFor(name) == "" {
			return fail(fmt.Errorf("no seed dir for %s (expected %s)", w.ID, filepath.Join(filepath.Dir(config.Path()), "seed", name)))
		}
		return fail(a.seedWorktree(w, name, *force))
	}
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(os.Stderr, "usage: jug repo add --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch] [--ws WS]\n       jug repo seed [--ws WS] [--force]")
		return 2
	}
	fs := flag.NewFlagSet("repo add", flag.ContinueOnError)
	ws := wsFlag(fs)
	wt := wtFlags(fs)
	if err := fs.Parse(args[1:]); err != nil || wt.repo == "" {
		fmt.Fprintln(os.Stderr, "usage: jug repo add --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch] [--ws WS]")
		return 2
	}
	q := *ws
	if q == "" {
		q = os.Getenv("JUG_WORKSTREAM")
	}
	if q == "" {
		if st, err := store.LoadState(a.cfg.StateDir); err == nil {
			q = st.Displayed
		}
	}
	if q == "" {
		return fail(errors.New("no workstream given (--ws) and none displayed"))
	}
	w, err := a.store.Resolve(q)
	if err != nil {
		return fail(err)
	}
	if w.CodeInside() {
		return fail(fmt.Errorf("%s already has its own code dir (%s)", w.ID, w.CodeDir))
	}
	old := w.ResolvedCodeDir()
	if err := a.addWorktree(w, wt); err != nil {
		return fail(err)
	}
	if st, err := store.LoadState(a.cfg.StateDir); err == nil && st.Streams[w.Name()] != nil {
		fmt.Printf("note: windows already open for %s still use %s; `jug close %s` then show it to start them in the worktree\n", w.ID, layout.ShortDir(old), w.ID)
	}
	return 0
}

// rm closes the workstream's windows, removes its worktree and deletes its
// directory. Destructive: requires --yes.
func (a *app) rm(args []string) int {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	force := fs.Bool("force", false, "remove the worktree even with uncommitted changes")
	pos, err := parseMixed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: jug rm WS --yes [--force]")
		return 2
	}
	w, err := a.store.Resolve(pos[0])
	if err != nil {
		return fail(err)
	}
	code := w.ResolvedCodeDir()
	if !*yes {
		fmt.Printf("would remove %s:\n  windows: closed\n", w.Name())
		if w.CodeInside() && gitwt.IsLinkedWorktree(code) {
			in, _ := gitwt.Inspect(code)
			dirty := ""
			if changes, _ := gitwt.Changes(code); len(changes) > 0 {
				exp := map[string]bool{}
				for _, p := range a.seedPaths(w) {
					exp[p] = true
				}
				for _, c := range changes {
					if !exp[c] {
						dirty = ", DIRTY (" + c + ") — needs --force"
						break
					}
				}
			}
			fmt.Printf("  worktree: %s (branch %s%s) removed; the branch is kept\n", layout.ShortDir(code), in.Branch, dirty)
		}
		fmt.Printf("  directory: %s deleted (TODO.md, notes/, …)\nre-run with --yes\n", layout.ShortDir(w.Dir))
		return 1
	}
	if err := a.eng.Close(w); err != nil {
		return fail(err)
	}
	if w.CodeInside() && gitwt.IsLinkedWorktree(code) {
		if err := gitwt.Remove(code, *force, a.seedPaths(w)); err != nil {
			if errors.Is(err, gitwt.ErrDirty) {
				return fail(fmt.Errorf("%s: %w (commit or stash, or use --force)", layout.ShortDir(code), err))
			}
			return fail(err)
		}
	}
	if err := os.RemoveAll(w.Dir); err != nil {
		return fail(err)
	}
	fmt.Printf("removed %s\n", w.Name())
	return 0
}

// jiraURL builds the browse URL for a ticket key; jira_base_url must be set.
func (a *app) jiraURL(key string) (string, error) {
	if a.cfg.JiraBaseURL == "" {
		return "", fmt.Errorf("jira_base_url is not set in %s (e.g. \"https://yourcompany.atlassian.net\")", config.Path())
	}
	return strings.TrimRight(a.cfg.JiraBaseURL, "/") + "/browse/" + strings.ToUpper(key), nil
}

// categoryFor applies the one rule: a workstream with a ticket is "work"
// unless the category was given explicitly.
func (a *app) categoryFor(explicit string, jira string) string {
	switch {
	case explicit != "":
		return explicit
	case jira != "":
		return "work"
	}
	return a.cfg.DefaultCategory
}

func (a *app) create(category, id, desc, codeDir, jira, pr string) (*store.Workstream, error) {
	if id == "" && jira != "" {
		id = strings.ToUpper(jira)
	}
	if desc == "" {
		return nil, errors.New("a description is required")
	}
	if id == "" {
		var err error
		if id, err = a.store.NextManualID(a.cfg.IDPrefix); err != nil {
			return nil, err
		}
	}
	if codeDir != "" {
		abs, err := filepath.Abs(config.Expand(codeDir))
		if err != nil {
			return nil, err
		}
		codeDir = layout.ShortDir(abs)
	}
	w := &store.Workstream{ID: id, Category: category, Desc: desc, Created: time.Now(), CodeDir: codeDir}
	if jira != "" {
		u, err := a.jiraURL(jira)
		if err != nil {
			return nil, err
		}
		w.Refs = append(w.Refs, store.Ref{Type: "jira", Key: strings.ToUpper(jira), URL: u})
	}
	if pr != "" {
		w.Refs = append(w.Refs, store.Ref{Type: "pr", Key: prKey(pr), URL: pr})
	}
	dir := filepath.Join(a.cfg.Root, store.DirName(category, id, desc))
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("%s already exists", dir)
	}
	w.Dir = dir
	if err := a.store.Save(w); err != nil {
		return nil, err
	}
	return w, nil
}

var prURL = regexp.MustCompile(`https?://[^/]+/([^/]+/[^/]+)/pull/(\d+)`)

// prKey turns https://host/owner/repo/pull/870 into owner/repo#870.
func prKey(url string) string {
	if m := prURL.FindStringSubmatch(url); m != nil {
		return m[1] + "#" + m[2]
	}
	return url
}

func (a *app) ref(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: jug ref add TYPE VALUE [--title T] [--status S] [--ws WS] | jug ref ls [--ws WS]")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("ref", flag.ContinueOnError)
	ws := wsFlag(fs)
	title := fs.String("title", "", "")
	status := fs.String("status", "", "")
	// allow flags after positionals
	var pos []string
	for len(rest) > 0 {
		if strings.HasPrefix(rest[0], "-") {
			if err := fs.Parse(rest); err != nil {
				return 2
			}
			pos = append(pos, fs.Args()...)
			break
		}
		pos = append(pos, rest[0])
		rest = rest[1:]
	}
	q := *ws
	if q == "" {
		q = os.Getenv("JUG_WORKSTREAM")
	}
	if q == "" {
		if st, err := store.LoadState(a.cfg.StateDir); err == nil {
			q = st.Displayed
		}
	}
	if q == "" {
		return fail(errors.New("no workstream given (--ws) and none displayed"))
	}
	w, err := a.store.Resolve(q)
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
		typ, val := pos[0], pos[1]
		r := store.Ref{Type: typ, URL: val, Title: *title, Status: *status}
		if *status != "" || *title != "" {
			r.Updated = time.Now()
		}
		switch typ {
		case "jira":
			r.Key = strings.ToUpper(val)
			if r.URL, err = a.jiraURL(val); err != nil {
				return fail(err)
			}
		case "pr":
			r.Key = prKey(val)
		}
		// replace an existing ref of the same type/key, else append
		replaced := false
		for i := range w.Refs {
			if w.Refs[i].Type == typ && (w.Refs[i].Key == r.Key || w.Refs[i].URL == r.URL) {
				w.Refs[i] = r
				replaced = true
			}
		}
		if !replaced {
			w.Refs = append(w.Refs, r)
		}
		return fail(a.store.Save(w))
	}
	fmt.Fprintf(os.Stderr, "jug ref: unknown subcommand %q\n", sub)
	return 2
}

func (a *app) env(args []string) int {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	ws := wsFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := *ws
	if q == "" {
		q = os.Getenv("JUG_WORKSTREAM")
	}
	if q == "" {
		if st, err := store.LoadState(a.cfg.StateDir); err == nil {
			q = st.Displayed
		}
	}
	if q == "" {
		return fail(errors.New("no workstream"))
	}
	w, err := a.store.Resolve(q)
	if err != nil {
		return fail(err)
	}
	for k, v := range layout.Env(w) {
		fmt.Printf("export %s=%s\n", k, shq(v))
	}
	return 0
}

// ---------------------------------------------------------------- watch / doctor

func (a *app) watch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	format := fs.String("format", "plain", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	render, ok := bar.Renderers[*format]
	if !ok {
		return fail(fmt.Errorf("unknown --format %q (plain|json|waybar)", *format))
	}
	return bar.Watch(a.cfg, a.store, render)
}

func (a *app) doctor() int {
	okc := 0
	check := func(ok bool, what string, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
		} else {
			okc++
		}
		fmt.Printf("%s  %-28s %s\n", mark, what, detail)
	}
	conn, err := sway.Dial()
	check(err == nil, "sway ipc", fmt.Sprint(err))
	if err == nil {
		defer conn.Close()
		wss, _ := conn.GetWorkspaces()
		var unnumbered []string
		for _, w := range wss {
			if w.Num < 0 && w.Name != a.cfg.LotLabel {
				unnumbered = append(unnumbered, w.Name)
			}
		}
		check(len(unnumbered) == 0, "workspaces named N:name", strings.Join(unnumbered, ", "))
		cfgText, _ := swayConfig(conn)
		layoutOK := strings.Contains(cfgText, "workspace_layout stacking") || strings.Contains(cfgText, "workspace_layout tabbed")
		check(layoutOK, "workspace_layout stacking|tabbed", "(the lot needs sway to wrap a sole container in a new workspace; see README, Limitations)")
		st, _ := store.LoadState(a.cfg.StateDir)
		if st != nil && st.Slot != nil {
			tree, _ := conn.GetTree()
			ws := tree.WorkspaceNum(st.Slot.Num)
			detail := fmt.Sprintf("workspace %d (%s)", st.Slot.Num, st.Slot.Name)
			if ws != nil {
				detail += " shape " + ws.Shape()
			} else {
				detail += " (currently empty/reaped)"
			}
			check(true, "slot", detail)
			check(true, "displayed", st.Displayed)
		} else {
			check(true, "slot", "none toggled")
		}
	}
	for _, tool := range []string{firstWord(a.cfg.Terminal), "fuzzel", firstWord(a.cfg.Browser), a.cfg.Editor, "opencode", "notify-send"} {
		p, err := exec.LookPath(tool)
		check(err == nil, tool, p)
	}
	if a.cfg.Dictator != "" {
		p, err := exec.LookPath(a.cfg.Dictator)
		check(err == nil, a.cfg.Dictator, p)
	}
	_, err = os.Stat(a.cfg.Root)
	check(err == nil, "store root", a.cfg.Root)
	for name, r := range a.cfg.Repos {
		ok := gitwt.IsRepo(r.Path)
		detail := r.Path
		if ok {
			detail += "  base " + gitwt.DefaultBranch(r.Path, r.Remote)
			if r.DefaultBranch != "" {
				detail = r.Path + "  base " + r.DefaultBranch
			}
		}
		check(ok, "repo "+name, detail)
	}
	fmt.Printf("config: %s\n", config.Path())
	return 0
}

func swayConfig(conn *sway.Conn) (string, error) {
	out, err := exec.Command("swaymsg", "-t", "get_config").Output()
	if err != nil {
		return "", err
	}
	var r struct {
		Config string `json:"config"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", err
	}
	return r.Config, nil
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return s
	}
	return f[0]
}

// shq quotes for sh; same rules as layout.ShellQuote (sway-safe).
func shq(s string) string { return layout.ShellQuote(s) }
