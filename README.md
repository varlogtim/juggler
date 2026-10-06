# juggler — workstreams for sway

You have more things in flight than fit on one screen: a ticket, the branch
for it, an AI coding session, a couple of terminals, the pull request, some
notes. juggler calls that bundle a **workstream** and gives it a home in sway:

```
┌───────────────────── workspace 4, label "WS" ─────────────────────┐
│ ┌─ left stack (⅓) ─┐ ┌────────── right stack (⅔) ─────────────┐ │
│ │                  │ │ ▸ terminal           (cwd = code dir)   │ │
│ │   opencode       │ │ ▸ nvim TODO.md                          │ │
│ │   (AI session)   │ │ ▸ Pull Request #897 — Chrome            │ │
│ │                  │ │ ▸ AISW-53270 — Jira — Chrome            │ │
│ └──────────────────┘ └─────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────┘
```

One hotkey brings up a picker of your workstreams; choosing one **replaces**
what the workspace shows with that workstream's windows. The previous
workstream is not closed — it is parked, alive, in the **lot** (a hidden
workspace where each parked workstream is one full-size tab), and comes back
exactly as you left it (every extra terminal, the browser tab you had open,
the editor buffer). Your scratchpad is never touched. Your normal sway keys
keep working inside: open a terminal, move focus up and down the stack, close
things.

Everything is a small Go binary, `jug`, driven from sway bindings. No plugin,
no patched compositor: it only speaks sway's IPC protocol. The optional
`jug serve` (a systemd user service) puts the same operations behind a
REST API and a web UI.

**Status: stage two.** Stage one: slot, lot, picker, single-key menu,
per-workstream opencode sessions, git worktrees with seeded files, waybar
module, dictation hook. Stage two: the same operations as a **REST API** and
a **web UI** (`jug serve`, a systemd user service), with the CLI, the API and
the UI sharing one service layer; **groups** (sprints, projects, any bucket)
to organize workstreams. Refs (ticket/PR) and groups are still entered by
hand; syncing them from Jira/GitHub and the opencode-side integration are
next — see [Limitations and roadmap](#limitations-and-roadmap).

- [Concepts](#concepts)
- [Install](#install)
- [Use cases](#use-cases)
- [Commands](#commands)
- [Web UI and REST API](#web-ui-and-rest-api)
- [Configuration](#configuration)
- [How it works](#how-it-works)
- [Test plan](#test-plan)
- [Limitations and roadmap](#limitations-and-roadmap)

## Concepts

**Workstream.** A directory under the store root (default `~/workstreams/`),
named `<category>_<id>_<short-desc>`:

```
~/workstreams/
  work_AISW-53270_disagg-toggle-requires-pause/
    workstream.toml      id, category, description, code dir, refs
    TODO.md              the three interfaces that must agree before it's done
    notes/               dictation and free-form notes
  personal_tim-0001_juggler-dev/
```

The `id` is the external key when there is one (a Jira ticket, a GitHub
issue), otherwise a counter prefixed with your user name (`tim-0001`;
`id_prefix` in the config). The **code dir** is where opencode
and every terminal start. It is either an existing checkout anywhere
(`--code-dir`), or — the intended shape — the workstream's **own git
worktree** inside its directory, created by `jug add --repo <name>`:

```
~/workstreams/work_AISW-53270_disagg-toggle-requires-pause/
  workstream.toml        code_dir = "src/ezaddon-mlis"   (relative = inside)
  TODO.md  notes/
  src/ezaddon-mlis/      git worktree of ~/src/ezaddon-mlis, branch AISW-53270
```

The main checkout stays where it is and owns `.git`; the worktree is a
cheap, fully buildable checkout of its own branch (one branch per worktree,
enforced by git). `jug rm` removes it properly. Repos are named in the
config (`[repos.ezaddon-mlis] path = …`), so `--repo ezaddon-mlis` is enough.

**Seeds.** Git does not carry the files a checkout needs but never commits —
`.envrc`, a local `*.mk`, helper scripts. Put them in
`~/.config/juggler/seed/<repo>/` and every new worktree of that repo gets a
copy (`jug repo seed` re-applies them; existing files are never overwritten
without `--force`). Text files are templated with `{{id}}`, `{{name}}`,
`{{repo}}`, `{{dir}}`, `{{code_dir}}`, `{{home}}`, so one seed can give each
workstream, say, its own virtualenv path; a seeded `.envrc` is
`direnv allow`ed on the spot. Seeded files do not count as "dirty" for
`jug rm`.

**Refs** link the workstream to the outside world:

| ref type | what it is | why it is separate |
|---|---|---|
| `jira` | the ticket | the interface to leadership — their view of the state |
| `pr`   | the pull request | the interface to the codebase — CI, review, merge |
| `issue`, `url` | anything else | docs, dashboards, a design page |

Each ref caches a `title`, `status` and `updated` timestamp so the picker and
the bar can show them without a network call. (Today these are written by
`jug ref add`; keeping them fresh from Jira/GitHub is the next milestone.)

**Groups.** A group is any bucket you want to see workstreams by — a sprint,
a project, a date range, "backlog". It has two halves, kept apart on purpose:

- **Membership is a tag on the workstream**: `groups = ["PCFS-S20-26.09.23-Nebula"]`
  in its `workstream.toml`. That is the only place members are recorded, so a
  group with no tagged workstreams has no members and is not shown, removing
  a workstream leaves nothing dangling, and renaming one moves its tags along.
  Tagging is what brings a group into existence.
- **Metadata lives once, in `<root>/groups.toml`**: `kind` (free text:
  `sprint`, `project`, `bucket`, …), `start`, `end`, `desc`. A sprint's dates
  are a fact about the sprint, not about each ticket in it; copying them onto
  N workstreams would mean N edits when the sprint shifts. A group may be
  tagged but unregistered (no dates; sorts last) or registered but empty (next
  sprint, created ahead; hidden from the main views, editable in the groups
  view).

Groups are ordered by **end date** (undated ones after, by name); the one
whose `[start, end]` contains today is *current*. A workstream can be in
several groups and is listed under each.

**The slot.** One workspace you choose, at any time, to be where workstreams
are displayed. Toggling a workspace into the slot renames it (its label
becomes `WS`); toggling again gives the name back. The slot is just a normal
workspace — `$mod+N` still reaches it, `$mod+Shift+N` still moves windows to
it.

**Displayed / parked / closed.** Exactly one workstream is *displayed* in the
slot. Others that have been opened are *parked*: their windows live, hidden,
as one tab each in the **lot** — a workspace (label `WS+`) holding a single
tabbed container. `$mod+=` visits the lot and comes back; parked workstreams
keep their full size there, so nothing reflows. A workstream that has never
been opened (or was closed) has no windows; showing it builds them: opencode
on the left, a terminal on the right.

**Sessions.** Each workstream owns one opencode session, pinned as
`opencode_session` in `workstream.toml` and launched with `opencode -s <id>`.
The first show creates it (an empty session titled `<id>: <desc>`, in the
code dir); `jug session pick` lets you adopt an existing session instead —
e.g. the one you already had going for that ticket. Why not
`opencode --continue`? opencode resumes the newest session of the *project*,
and it identifies a project by the git repository: every clone and worktree
of the same repo is one project, so `--continue` in `repo-A` can hand you a
session from `repo-B`; sessions started from a non-repo directory (your
home) all belong to one "global" project. Pinning sidesteps both.

## Install

Requirements: sway (tested on 1.9), alacritty (the only terminal exercised:
juggler passes `--class`, `--working-directory`, `-T`, `-e`, and for the menu
`-o window.dimensions.*`; another terminal needs `terminal` in the config and
equivalent flags in `layout.terminalCmd`), fuzzel, a browser (google-chrome by
default), `notify-send`, git ≥ 2.30 (`worktree repair`), Go 1.22+ to build.
Optional: [opencode](https://opencode.ai), nvim, waybar, direnv,
[dictator](https://github.com/varlogtim/dictator) for dictated notes.

```sh
make install          # ~/.local/bin/jug + ~/.config/sway/juggler.conf
jug doctor            # checks sway IPC, tools, workspace naming, config
make service-install  # optional: `jug serve` (web UI + REST API) as a systemd --user service
make service-enable   #           start now and at login → http://127.0.0.1:7474/
```

Then three edits to your sway config (`sway/juggler.conf` is the bindings
file `make install` drops in place):

1. **Name workspaces `N:label` and switch with `workspace number`.** The slot
   is renamed at runtime (`4:WS`), and only the number keeps `$mod+4` landing
   on it:

   ```
   set $ws4 "4:󰾗"
   bindsym $mod+4        workspace number $ws4
   bindsym $mod+Shift+4  move container to workspace number $ws4
   ```

   `scripts/rename-workspaces.sh` renames the *live* workspaces once so you do
   not have to log out. In waybar use `"sway/workspaces": {"format": "{name}"}`
   to show only the label.
2. `include ~/.config/sway/juggler.conf` — the bindings below.
3. `swaymsg reload`.

Default bindings (`$mod` is yours):

| key | action |
|---|---|
| `$mod+t` | **picker**: choose a workstream and show it; the last row creates a new one |
| `$mod+w` | **action menu**: a floating window lists the actions that apply; press **one key**: |
| | `t` term · `j` jira · `p` pr · `n` notes · `r` review · `o` opencode · `d` dictate · `w` pick · `l` lot · `x` park · `s` slot · `c` close · `g` go · `Esc` cancel |
| `$mod+=` (`$mod++`) | visit the **lot** — parked workstreams as tabs — or come back |
| `$mod+Shift+t` | toggle the focused workspace as the slot |

The menu only lists what applies: no workstream displayed → *pick*, *lot*,
*slot*; no `pr` ref → no *pr* row; standing on a parked workstream in the lot →
`g` *go* shows it. Run `jug menu` in a terminal and it draws inline instead.

waybar: add `custom/juggler` (see `contrib/waybar.jsonc`) — the displayed
workstream, left-aligned after the workspaces:
`WS  AISW-53270 · disagg toggle requires pause · Blocked · PR#897 open · todo 2`.
Click = picker, right-click = toggle.

## Use cases

Each of these is also a test in the [Test plan](#test-plan).

### UC1 — Make a workspace the slot

Go to the workspace you want to work in, press `$mod+Shift+t`. Its label
becomes `WS`. Press again to release it (label restored, displayed workstream
parked). Toggling a *different* workspace moves the slot there.

### UC2 — Start on a workstream

`$mod+t`, type a few letters of the ticket or description, Enter. First time:
opencode starts in the code dir on the left (⅓), a terminal on the right (⅔),
focus on opencode. The bar shows the workstream.

If nothing is toggled yet, the workspace you are on becomes the slot.

### UC3 — Switch workstreams, come back later

Open a few extra windows in the right stack (`$mod+Return`, `$mod+w` →
*term*, a browser). `$mod+t`, pick another workstream: the stacks are replaced; the old
one is parked in the lot. Pick the first one again: it is back — same
windows, same order, same focused window, opencode still mid-conversation.
Switching takes milliseconds; nothing is restarted. `$mod+=` shows you the
lot: one tab per parked workstream, each at full size; `$mod+w` `g` there
brings the focused one back; `$mod+=` again returns to where you were.

### UC4 — Add windows the normal way

Inside the slot your sway keys are unchanged. `$mod+Return` opens a terminal
next to the focused window (so in whichever stack you are in); `$mod+j/k`
walk the stack; `$mod+h/l` jump between the two stacks; `$mod+Shift+q`
closes. Windows you add belong to the displayed workstream and travel with it
when it is parked.

### UC5 — Open the ticket or the PR beside the code

`$mod+w` → *jira* opens the Jira ticket in a browser window in the right
stack; `$mod+w` → *pr* the pull request. Press again and the existing window is focused
instead of opening another. The windows are ordinary browser windows — open
more tabs in them if the ticket links somewhere.

From a shell inside the workstream (every terminal has `$JUG_WORKSTREAM`
set): `jug open pr`, `jug open https://…`.

### UC6 — Let a program open things in your workstream

opencode (or any script) runs inside the workstream with `JUG_WORKSTREAM`,
`JUG_WORKSTREAM_DIR` and `JUG_CODE_DIR` set. After it creates a PR it can:

```sh
jug ref add pr https://github.example.com/org/repo/pull/897
jug open pr
```

If that workstream is not the displayed one at that moment, the open is
**queued** (you get a notification) and runs the next time you show it — it
never hijacks the workstream you are looking at.

Anything that can speak HTTP can do the same without the `jug` binary on
its PATH (see [Web UI and REST API](#web-ui-and-rest-api)):

```sh
curl -s -X POST -H 'Content-Type: application/json' \
  http://127.0.0.1:7474/api/v1/workstreams/AISW-53270/open -d '{"what":"pr"}'
```

### UC7 — Review what the AI wrote

`$mod+w` → *review* opens the code dir's uncommitted diff (staged and unstaged, vs
HEAD) read-only in the editor, in the right stack, beside opencode.

### UC7b — Adopt the opencode session you already had

You were already talking to opencode about this ticket before the workstream
existed. `jug session pick` (from a terminal in the workstream, or
`--ws AISW-52787`) lists sessions whose title mentions the workstream id or
one of its refs — from the code dir's project *and* from your home
directory's — pick one and it is pinned; add `--relaunch` to restart the
workstream's opencode on it right away (invisible if you are on another
workspace). `jug session` shows the pin; `jug session new` starts over with a
fresh titled session; `jug session pin ID` pins an id you know.

### UC8 — Keep the workstream's own to-do list

`$mod+w` → *notes* opens `TODO.md`. It starts with three sections — **Leadership
(ticket)**, **Code (PR)**, **Follow-ups** — because a workstream is only done
when all three agree: the ticket says what leadership expects, the PR is
merged, and the things you found along the way have their own tickets. The
picker and the bar show the count of open `- [ ]` items.

### UC9 — Dictate notes into the workstream

`jug dictate` (bind it, e.g. `$mod+Ctrl+n` — a commented example is in
`sway/juggler.conf`) toggles dictator's notes mode with the session file
under `<workstream>/notes/`. With no workstream displayed it falls back to
dictator's default directory.

### UC10 — Create a workstream

From the picker (`$mod+t` → `+ new workstream…`), one line:

```
[work:|personal:] [AISW-123] description
```

A leading **ticket key** becomes the id and makes it a `work` workstream (and
a `jira` ref); a `work:`/`personal:` prefix forces the category; a plain
description is `personal` (the configured `default_category`). A second
fuzzel asks where the code lives: a **worktree of a configured repo** (the
branch it will use is shown), **no code**, or an **existing directory**.
Nothing is created until both answers are in; `Esc` cancels. A workstream
without code starts its terminals and opencode in its own directory (notes
only — not a git checkout); the bar and the picker say `no code`, and
`jug repo add --repo <name>` fixes it later. Or:

```sh
jug add --jira AISW-53270 --repo ezaddon-mlis "disagg toggle requires pause"
#   -> ~/workstreams/work_AISW-53270_…/src/ezaddon-mlis on NEW branch AISW-53270 from origin/develop
jug add --repo ezaddon-mlis --branch tim/fix-thing "fix thing"      # explicit branch name
jug add --category personal --code-dir ~/src/juggler "juggler dev"  # any existing directory
jug ref add pr https://github.example.com/org/repo/pull/897 --ws AISW-53270
jug repo add --ws AISW-52932 --repo ezaddon-mlis                     # give an existing workstream its worktree
```

`--jira KEY` makes the id the key and the category `work` (unless
`--category` says otherwise). `--repo` fetches
the base branch first (`--no-fetch` to skip), reuses the branch if it already
exists locally or on the remote (tracking it), else creates it from the
remote's HEAD (`--base` to pick another). The branch defaults to the ticket
key for ticket ids, else `<user>/<slug>`. If the branch is checked out in
another worktree, git — and so `jug` — refuses. The repo's seed files (see
Concepts) are copied in last.

### UC10b — Python virtualenvs that survive worktrees

Keep venvs **outside** checkouts (e.g. `~/venvs/<name>`): a worktree
stays small, `jug rm` cannot take the venv with it, and console-script
shebangs stay short (Linux truncates them past 127 bytes). A seeded `.envrc`
like this gives each workstream its own venv on first `cd`:

```sh
VENV="{{home}}/venvs/{{repo}}-{{id}}"
[ -x "$VENV/bin/python" ] || python3 -m venv "$VENV" --prompt "{{repo}}-{{id}}"
source "$VENV/bin/activate"
```

To move an existing venv out of a checkout use `scripts/relocate-venv.sh
OLD NEW`: a venv is not relocatable by `mv` alone (`bin/activate*`, every
script's shebang and `pyvenv.cfg` embed its absolute path); the script moves
it, rewrites those, drops stale bytecode and verifies the result. Editable
installs (`pip install -e <src>`) point at *source* checkouts, not at the
venv, and tie a venv to one checkout — that is why the example above is
per-workstream rather than per-repo.

### UC10c — Rename or recategorize

Made it `personal` by accident, or the ticket arrived after you started?

```sh
jug set tim-0002 --jira AISW-53350 --desc "e2etest smart routing regression"
#  personal_tim-0002_… -> work_AISW-53350_e2etest-smart-routing-regression
jug set AISW-53350 --category personal
```

The directory follows the new name, the git worktree inside it is repaired
(`git worktree repair`), a seeded `.envrc` is re-allowed, and the pinned
opencode session is retitled. Live windows are closed first and the
workstream reopened if it was displayed — opencode resumes the same session;
shells in its terminals are lost.

### UC11 — Close, or remove, a workstream

`jug close AISW-53270` kills its windows (displayed or parked). The directory,
`TODO.md`, notes and the session pin stay; showing it again rebuilds the
layout and `opencode -s <pinned id>` resumes exactly that conversation.

`jug rm AISW-53270` prints what would go; `--yes` closes the windows, removes
the worktree with `git worktree remove` (refusing while it has uncommitted or
untracked changes unless `--force`; the branch is always kept) and deletes
the workstream directory — notes included, so archive first if you want them.

### UC12 — See where everything stands

`jug ls` is the table view; `jug current` prints the displayed workstream;
`jug pick --print` shows the picker rows without opening the picker:

```
● AISW-53270  disagg toggle requires pause  Blocked  PR#897 open  todo:2  work
◐ AISW-52787  deprecate all projects        Blocked  PR#895 open  todo:5  work
○ AISW-52932  reduce db round trips …       New                   todo:5  work
○ tim-0001    juggler dev                                         todo:5  personal
+ new workstream…
```

`●` displayed, `◐` parked (alive), `○` not open. Displayed first, then parked
by last shown, then the rest by creation.

### UC14 — See the week by sprint (or project, or anything)

```sh
jug group set PCFS-S20-26.09.23-Nebula --kind sprint --start 2026-09-23 --end 2026-10-06 --desc "Team Nebula sprint 20"
jug group set backlog --kind bucket --desc "assigned, not in a sprint"
jug group add PCFS-S20-26.09.23-Nebula AISW-53270 AISW-53350     # tag (creates the group if needed)
jug group add backlog AISW-52932
jug group ls             # groups with members, current one starred, days left
jug ls --group backlog   # just those
jug add --jira AISW-9 --group PCFS-S21-26.10.07-Nebula --repo ezaddon-mlis "next thing"
```

The web UI's default view is **grouped**: one collapsible section per
group in end-date order (current sprint first, with its days left in the
header), then `backlog`, then *no group* for the rest; *flat* is one click
away. *groups…* edits the metadata (dates with a date picker) and pre-creates
groups; a workstream's detail pane has a checkbox per group and an "add to
group" box for a new one. This is the shape the Jira sync (next stage) fills
in by itself: one group per sprint with the sprint's dates, each ticket
tagged with its sprint, the rest tagged `backlog`.

### UC13 — Manage everything from a browser

`make service-install && make service-enable`, then open
<http://127.0.0.1:7474/>. The page is the picker, the menu and `jug ls` in
one place, kept live by the server: the header shows the slot and what is
displayed, every row has *show*, and the detail pane does what the CLI does —
edit (category, id, description, ticket), refs, the TODO.md editor, worktree
and seeds, the opencode session, the window actions, and remove with the same
dry run. Useful when a workstream is parked and you want to fix its refs or
notes without bringing it to the slot, and as the thing to point a second
screen or a phone at (over an SSH tunnel — it binds to loopback only).

## Commands

```
jug toggle                       slot on/off for the focused workspace
jug pick [--print]               picker (fuzzel) → show; --print lists the rows
jug menu [--print]               single-key action menu (floating window from a hotkey, inline in a terminal)
jug show [--no-switch] WS        show WS; --no-switch keeps your current workspace/focus
jug show --under-focus           show the parked workstream whose window is focused (in the lot)
jug park [--others]              hide the displayed workstream (--others: move parked ones found elsewhere into the lot)
jug lot                          visit the lot / come back
jug current [--json]             the displayed workstream
jug open jira|pr|URL [--ws WS]   browser window in the right stack (focus if open; queue if parked)
jug term [--title T] [-- CMD…]   terminal in the right stack
jug notes | review | focus       TODO.md / diff / focus opencode
jug dictate                      dictator notes into <ws>/notes
jug env [--ws WS]                export JUG_* for a shell: eval "$(jug env)"
jug session [--ws WS] [--relaunch]            the pinned opencode session (id, updated, dir, title)
jug session pick [--all] [--relaunch]         adopt an existing session (titles matching id/refs; --all: everything)
jug session pin ID | new | unpin [--relaunch] pin an id / create a fresh titled one / forget the pin
jug ls [--group G] [--json]      all workstreams (or those tagged G)
jug add [--category C] [--id ID] [--jira KEY] [--pr URL] [--group G]… [--show] DESC…
        [--code-dir D | --repo NAME|PATH [--branch B] [--base BASE] [--no-fetch]]
jug repo add --repo NAME|PATH [--branch B] [--base BASE] [--ws WS]   worktree for an existing workstream
jug repo seed [--ws WS] [--force]                                   (re)copy the repo's seed files into the worktree
jug set WS [--category C] [--id ID] [--desc D] [--jira KEY]   rename / recategorize (directory follows)
jug rm WS [--yes] [--force]      remove: windows, worktree (branch kept), directory
jug group ls [--all] [--json]    groups with members (--all: registered-but-empty too); * = current
jug group show G [--json]        one group and its members
jug group set G [--kind K] [--start D] [--end D] [--desc T]    create / update metadata
jug group add G WS… | group remove G WS…                        tag / untag
jug group rm G [--untag]         forget the metadata (--untag: remove the tag from members too)
jug ref add TYPE VALUE [--title T] [--status S] [--ws WS]
jug ref rm TYPE [KEY|URL] [--ws WS]
jug ref ls [--ws WS]
jug close WS
jug watch [--format plain|json|waybar]
jug serve [--listen ADDR]        web UI + REST API (default 127.0.0.1:7474; see below)
jug doctor
```

`WS` resolves by canonical name (`work_AISW-53270_…`), id (`AISW-53270`), or
a unique prefix of either. `--ws` defaults to `$JUG_WORKSTREAM`, then the
displayed workstream. Set `JUG_DEBUG=1` to see every sway command issued.

## Web UI and REST API

`jug serve` runs a small HTTP server (default `127.0.0.1:7474`, config
`listen`) with two things on it: a JSON API that exposes every operation the
CLI has, and a single-page web UI built on that API. Run it by hand or as the
systemd user service `make service-install` installs (`init/juggler.service`:
restarts on failure, finds the live sway socket itself when started before
the compositor, answers `503 sway_unavailable` for window operations until
sway is there).

**The UI** (`http://127.0.0.1:7474/`): the header shows the slot, what is
displayed, how many are parked, the current group with its days left, and
has *grouped/flat*, *groups…*, *slot here / park / lot / + new*; the table
lists every workstream (state glyph, id, category, description, refs with
their cached status, open TODO count, branch with a *dirty* badge, session)
— grouped into collapsible sections in group order by default — with a
filter box (`/` focuses it) and a *show*/*focus* button per row. Clicking a row opens the detail pane: actions (show, focus, park, term,
notes, review, open jira/pr, dictate, close windows), an **edit** form
(category, id, description, ticket — the directory follows), **code**
(branch, head, upstream, worktree-of; *add worktree* / *re-seed*), **refs**
(add/remove/open), **groups** (a checkbox per known group, a box for a new
one), a **TODO.md** editor, the **opencode session** (what is pinned, choose
an existing session, new, relaunch, unpin) and **remove** with the same
dry-run the CLI prints. It updates live: the page subscribes to
`/api/v1/events`, which relays sway workspace/window events (debounced) and
the server's own mutations.

**Security:** there is no authentication — the API runs commands as you. It
binds to loopback only and refuses anything else unless `JUG_SERVE_ANY=1`.
Do not put it behind a reverse proxy.

**The API** (`/api/v1`, JSON; errors are `{"error": …, "code": …}` with
`400 bad_request`, `404 not_found`, `409 dirty|nothing_parked`,
`503 sway_unavailable`). `{ws}` is a canonical name, an id or a unique prefix.

| method, path | does | body / notes |
|---|---|---|
| `GET /health` | liveness | `{ok, version, time}` |
| `GET /status` | slot, displayed, lot, focused workspace | same picture the bar uses |
| `GET /doctor` | the `jug doctor` checks | `[{ok, what, detail}]` |
| `GET /config` | effective config, categories, repos | |
| `GET /repos` | configured repos | `[{name, path, remote, default_branch, seed_dir, ok}]` |
| `GET /events` | Server-Sent Events | `hello`, `changed`, `tick` |
| `POST /slot/toggle` | `jug toggle` | `{on, slot}` |
| `POST /slot/park` · `/slot/park-others` | `jug park` · `jug park --others` | |
| `POST /lot` | `jug lot` | 409 when nothing is parked |
| `GET /groups?all=true` | `jug group ls [--all]` | `[GroupInfo]` in group order; without `all` only groups with members |
| `GET /groups/{name}` | `jug group show` | `GroupInfo` (registered or merely in use) |
| `PUT /groups/{name}` | `jug group set` | `{kind, start, end, desc}` (fields present are set; `""` clears) |
| `DELETE /groups/{name}?untag=true` | `jug group rm [--untag]` | `{removed, untagged}` |
| `POST /groups/{name}/members` | `jug group add` | `{workstreams: [ws…]}` → `GroupInfo` |
| `DELETE /groups/{name}/members/{ws}` | `jug group remove` | 404 when not a member |
| `GET /workstreams?group=NAME` | `jug ls --group` | `[WorkstreamInfo]` |
| `POST /workstreams` | `jug add` | `{desc, jira, category, id, pr, groups, code_dir \| repo, branch, base, no_fetch, show}` → 201 `{workstream, worktree, warnings}` |
| `GET /workstreams/{ws}` | one `WorkstreamInfo` | |
| `PATCH /workstreams/{ws}` | `jug set` / retag | `{category, id, desc, jira, groups}` (any subset; `groups` replaces the tags) → `{result, workstream}` |
| `DELETE /workstreams/{ws}?force=true` | `jug rm --yes [--force]` | 409 `dirty` without force |
| `GET /workstreams/{ws}/plan` | the `jug rm` dry run | `{has_worktree, worktree, branch, dirty}` |
| `GET /workstreams/{ws}/env` | `jug env` | `{JUG_WORKSTREAM, …}` |
| `GET` · `PUT /workstreams/{ws}/todo` | TODO.md | `{text}` → `{open}` |
| `GET` · `POST /workstreams/{ws}/refs` | `jug ref ls` · `jug ref add` | `{type, value, title, status}` |
| `DELETE /workstreams/{ws}/refs/{type}?key=…` | `jug ref rm` | `url=` works too |
| `POST /workstreams/{ws}/repo` | `jug repo add` | `{repo, branch, base, no_fetch}` → 201 `{worktree, workstream}` |
| `POST /workstreams/{ws}/seed` | `jug repo seed` | `{force}` → `{seeded}` |
| `POST /workstreams/{ws}/show` | `jug show` | `{no_switch}` |
| `POST /workstreams/{ws}/close` | `jug close` | |
| `POST /workstreams/{ws}/open` | `jug open` | `{what: "jira"\|"pr"\|"issue"\|URL}` → `{queued}` |
| `POST /workstreams/{ws}/term` | `jug term` | `{title, command}` |
| `POST /workstreams/{ws}/notes` · `/review` · `/focus` · `/dictate` | the matching verb | |
| `GET /workstreams/{ws}/session` | `jug session` | `{pinned, session}` (starts a transient opencode server) |
| `GET /workstreams/{ws}/session/candidates?all=true` | `jug session pick` list | `{pinned, sessions}` |
| `PUT /workstreams/{ws}/session` | `jug session pin\|new` | `{id}` or `{new: true}`, `{relaunch}` |
| `DELETE /workstreams/{ws}/session` | `jug session unpin` | |
| `POST /workstreams/{ws}/session/relaunch` | `jug session --relaunch` | |

`WorkstreamInfo` (also what `jug ls --json` prints):

```json
{ "name": "work_AISW-53270_disagg-toggle-requires-pause", "id": "AISW-53270", "category": "work",
  "desc": "disagg toggle requires pause", "created": "…", "dir": "/home/me/workstreams/work_AISW-53270_…",
  "code_dir": "src/ezaddon-mlis", "code_path": "/home/me/workstreams/…/src/ezaddon-mlis", "code_inside": true, "has_code": true,
  "refs": [{ "type": "jira", "key": "AISW-53270", "url": "https://…/browse/AISW-53270", "status": "Blocked" }],
  "groups": ["PCFS-S20-26.09.23-Nebula"],
  "opencode_session": "ses_…", "state": "parked", "todos_open": 5, "shown": "2026-10-05T12:00:00-04:00",
  "git": { "branch": "tim/aisw-53270-…", "head": "48faeb67", "dirty": false, "linked": true, "main": "/home/me/src/ezaddon-mlis", "upstream": "origin/…", "ahead": 0, "behind": 0 } }
```

`GroupInfo`:

```json
{ "name": "PCFS-S20-26.09.23-Nebula", "kind": "sprint", "desc": "Team Nebula sprint 20",
  "start": "2026-09-23", "end": "2026-10-06", "registered": true, "current": true, "days_left": 2,
  "members": ["work_AISW-53270_…", "work_AISW-53350_…"], "count": 2 }
```

Copy/pasta:

```sh
J=http://127.0.0.1:7474/api/v1
curl -s $J/workstreams | jq -r '.[] | [.state, .id, .desc] | @tsv'
curl -s -X POST -H 'Content-Type: application/json' $J/workstreams \
  -d '{"desc":"fix the thing","jira":"AISW-123","repo":"ezaddon-mlis","show":true}' | jq .
curl -s -X POST -H 'Content-Type: application/json' $J/workstreams/AISW-123/open -d '{"what":"jira"}' | jq .
curl -s -X PATCH -H 'Content-Type: application/json' $J/workstreams/AISW-123 -d '{"desc":"renamed"}' | jq .result
curl -s $J/workstreams/AISW-123/plan | jq . && curl -s -X DELETE "$J/workstreams/AISW-123" | jq .
```

Not on the API by design: `pick` and `menu` (they *are* a UI — the web page
replaces them) and `watch` (use `/events`).

For scripted captures, `/?nolive` loads the page without the event stream
(headless browsers otherwise wait on the open connection);
`scripts/ui-shot.mjs URL OUT.png [EXPR] [CLICK]` (Node ≥ 22, google-chrome)
renders a page over the DevTools protocol, fails on console errors and
returns the value of `EXPR` — `make ui-shot` uses it for the list and detail
views.

## Configuration

`~/.config/juggler/config.toml` (or `$JUGGLER_CONFIG`); every key is optional:

```toml
root             = "~/workstreams"
state_dir        = "~/.local/state/juggler"
slot_label       = "WS"
lot_label        = "WS+"                      # the hidden workspace that holds parked workstreams
terminal         = "alacritty"                 # needs --class, --working-directory, -e
opencode         = "opencode"                  # left stack command; `-s <id>` is appended when pinned
opencode_sessions = true                       # one opencode session per workstream (see Concepts)
editor           = "nvim"
browser          = "google-chrome --new-window"
browser_app_id   = "google-chrome"             # sway app_id of the browser's windows
dictator         = "dictator"                  # "" disables jug dictate
left_width_ppt   = 33
default_category = "personal"
id_prefix        = "tim"                       # ids for workstreams without a ticket: tim-0001 (default: $USER)
jira_base_url    = "https://yourcompany.atlassian.net"   # required for --jira / jira refs (no default)
code_subdir      = "src"                       # <workstream>/src/<repo> for worktrees
listen           = "127.0.0.1:7474"            # `jug serve` address (loopback only)

# Top-level keys must come before any [table] (TOML).
[repos.ezaddon-mlis]                           # what `--repo ezaddon-mlis` means
path           = "~/src/ezaddon-mlis"          # the main checkout that owns .git
default_branch = "develop"                     # base for new branches (default: the remote's HEAD)
# remote       = "origin"
# seed_dir     = "~/.config/juggler/seed/ezaddon-mlis"   # the default, when it exists
```

## How it works

juggler never moves individual windows around; it builds one container per
workstream and moves *that*. These are the sway facts it relies on, each
verified by `scripts/m0-sway-poc.sh` (layout) and `scripts/lot-poc.sh`
(parking) — `make poc` / `make poc-lot`; both open and close a few windows on
scratch workspaces and refuse to touch a real slot or lot:

| fact | consequence |
|---|---|
| a new view is placed as a *sibling of the focused node*; `focus parent` from a top-level container focuses the workspace itself | spawning the terminal with the workspace focused puts it beside the opencode stack, not inside it |
| `splitv; layout stacking` on a view with siblings wraps it in a stack | the right stack |
| `splith` with the *workspace* focused wraps all its children in one container | the workstream **root**, marked `jug:<name>`; left/right stacks marked `jug:<name>:left/right`. Marks go on split containers only — on a view they would render in its title bar |
| `resize set width 33 ppt` on the left stack | ⅓ / ⅔ |
| moving a *tiling* container into an **empty** workspace dissolves it (the workspace takes its children); into a **non-empty** one inserts it beside that workspace's last focused view; moving into its own ancestor is a silent no-op | a root never travels tiling by `move container to workspace`. It goes to a **mark** (intact, as a child) or hops through the scratchpad as a floating unit and is tiled again on arrival |
| `move container to mark jug:lot` adds the root as a tab of the lotbox, intact; the first park has no lotbox yet, so the root hops via the scratchpad into the empty lot, where `workspace_layout` wraps it — that wrapper is marked `jug:lot` and set `tabbed` | **park** = one transaction. Tabs keep full size, so hidden TUIs do not reflow. Scratchpad keys are unaffected |
| strays can be moved into a hidden root's right stack by mark; then `move scratchpad; move container to workspace <slot>; floating disable; split none; resize …` and a final `focus` | **restore** = one IPC message = one frame, even when the slot workspace is not visible; focus ends where it was |
| sway records the focused workspace at `exec` time (launch context, matched by pid) and a view that maps into a non-visible workspace never steals focus | `jug term` focuses the right stack, execs, and refocuses your previous window — all in one message — so a program can add a terminal to a workstream you are not looking at |
| Chrome hands the URL to its running instance, so launch context is lost; the window lands wherever focus is | `jug open` subscribes to `window::new`, catches the new browser window and `move container to mark`s it into the right stack; its con_id is remembered so the next `open` of the same ref focuses it |
| `rename workspace to "4:WS"` keeps the number; `workspace number "4:󰾗"` lands on `4:WS` | the slot label changes while `$mod+4` keeps working — hence the `N:label` naming requirement |
| sway splits a command batch on `;` while tracking quotes, but an escaped quote directly followed by a quote (the shell idiom `'\''` ending a word) makes it hand the *rest of the batch* to `exec`'s shell | every argument juggler puts in an `exec` is quoted without backslashes: `'` becomes `'"'"'`, `\` becomes `'\\'` (`layout.ShellQuote`) |
| a linked worktree's `.git` is a *file* holding an absolute `gitdir:` path into the main checkout; one branch can be checked out in one worktree; `git worktree remove` is the only clean way out | the main checkout must not move; `jug rm` goes through git; `jug ls` shows branch + `*` for dirty |
| opencode's `/session` API is reachable from a transient `opencode serve`; a session created with `?directory=<code dir>` belongs to that project, and `-s <id>` opens a session from any project | `jug` creates/pins sessions through the public API, never the database |

The menu is a tiny TUI: from a hotkey `jug menu` has no terminal, so it
re-launches itself in a floating alacritty (`for_window [app_id=jug-menu]`),
draws the rows, reads one raw key, and starts the chosen verb *detached* with
`JUG_AFTER_CLOSE=<con_id>`; the child waits for the menu window to vanish
before touching the tree, so focus is already back where it belongs.

Runtime state (`~/.local/state/juggler/state.json`) holds what sway cannot
tell us: which workspace is the slot and its original name, which workstream
is displayed, the browser windows we opened per ref, and queued opens.
"Displayed" is re-derived from the tree (which root is tiling in the slot) on
every run, so a show that fails halfway cannot leave the two disagreeing, and
a stale state file is harmless: a workstream whose marks are gone is simply
rebuilt. Batches never reference a mark that might not exist: sway fails the
whole batch with "No matching node." for an unmatched criteria. The opencode
session pin is part of the workstream itself (`workstream.toml`), because it
must survive everything else.

**One service layer, three faces.** Every operation lives once, in
`internal/app`, as a function that takes inputs and returns values. The CLI
(`cmd/jug`) parses flags and prints; the HTTP layer (`internal/web`) decodes
JSON and encodes results; the web UI calls the HTTP layer. Operations that
touch sway take a `*layout.Engine` — one IPC connection plus the runtime
state — which the CLI opens once per invocation and the server opens once
per request, serialized by a mutex so two requests never do tree surgery at
the same time. Store-only operations (create, refs, TODO, rename, remove)
work without a compositor, which is what the HTTP tests run against.

Code map: `cmd/jug` (CLI) · `internal/app` (service layer) · `internal/web`
(REST API, SSE, embedded UI in `ui/`) · `internal/sway` (IPC client, tree
helpers) · `internal/layout` (build/show/park/spawn/open) · `internal/store`
(workstream.toml, state.json) · `internal/picker` (fuzzel rows) ·
`internal/bar` (waybar stream) · `internal/oc` (opencode session API) ·
`internal/gitwt` (git worktrees) · `internal/seed` (per-checkout files) ·
`internal/config`. Scripts: `scripts/relocate-venv.sh`,
`scripts/rename-workspaces.sh`, `scripts/ui-shot.mjs` (headless render of
the UI over the DevTools protocol, for smoke tests), the PoCs.

## Test plan

Manual, on a live sway session. Run top to bottom; each step names the use
case it exercises and what you should see. `jug doctor` must be all `ok`
first. Keep a terminal on a *non*-slot workspace for the `jug` commands
that are not hotkeys.

| # | UC | do | expect |
|---|---|---|---|
| 1 | — | `jug doctor` | every line `ok`; workspaces listed as `N:label` |
| 2 | UC10 | `jug add --category personal --code-dir <some git checkout> "juggler dev"`; `jug ls` | a `personal_tim-NNNN_juggler-dev/` dir with `workstream.toml`, `TODO.md`, `notes/`; `ls` shows it with `TODO 5` |
| 3 | UC1 | go to an empty workspace (e.g. `$mod+6`), `$mod+Shift+t` | bar label becomes `WS`; notification "workspace 6 is now the workstream slot"; the bar shows `WS — (Super+t to pick)` |
| 4 | UC2 | `$mod+t`, type `jug`, Enter | left ⅓: opencode running in the code dir **on a new session titled `tim-NNNN: juggler dev`** (`jug session` shows it; `workstream.toml` has `opencode_session`); right ⅔: a terminal in the same dir; focus on opencode; bar: `WS tim-NNNN · juggler dev · todo 5` |
| 5 | UC7b | `jug session pick --ws <ws> --all`, choose another session, then `$mod+w` → *opencode* | the pin changed (`jug session`); after `jug session --relaunch` (or closing opencode and `$mod+w` → *term*) the left stack runs `opencode -s <that id>` (`pgrep -af "opencode -s"`) and shows that conversation |
| 6 | UC4 | `$mod+l` (right stack), `$mod+Return` twice, `$mod+k`/`$mod+j` | two more terminals stacked on the right; focus walks the stack; `$mod+h` jumps back to opencode |
| 7 | UC5 | `$mod+w`, press `j` on a workstream with a jira ref (or `jug open https://example.com`) | a floating menu lists only applicable rows with the workstream id as title; the single key acts (no Enter); a browser window joins the right stack and is focused |
| 8 | UC5 | repeat step 7 | no new window; the existing one is focused (`jug ls --json` / state.json shows the con_id under `windows`) |
| 9 | UC3 | create a second workstream (`+ new workstream…` in the picker, type a name) | first workstream disappears from the slot; the new one is built; `jug ls` shows `parked` / `displayed`; a `WS+` workspace appears in the bar; `$mod+-` does **not** show the parked one |
| 10 | UC3 | `$mod+=` | the lot: the parked workstream as a full-width tab (its opencode ⅓ / terminal ⅔ intact); `$mod+w` `g` shows it; `$mod+=` from the lot returns to the slot |
| 11 | UC3 | `$mod+t`, pick the first one again | it is back with all windows from steps 6–7, same order, opencode focused, ⅓ / ⅔ widths |
| 12 | UC7 | edit a file in the code dir, `$mod+w` → *review* | read-only diff opens in the editor in the right stack |
| 13 | UC8 | `$mod+w` → *notes*, tick a box, save | picker/bar `todo` count drops by one |
| 14 | UC6 | from a terminal on another workspace: `jug open --ws <parked ws> https://example.com/q` | notification "… is not displayed; … will open when it is"; nothing moves |
| 15 | UC6 | show that workstream | the queued URL opens in its right stack |
| 16 | UC6 | from a terminal *inside* the displayed workstream: `echo $JUG_WORKSTREAM; jug term -- htop` | prints the workstream name; a new terminal running the command appears in the right stack |
| 17 | UC6 | from a terminal on another workspace, with the slot not visible: `jug term --ws <displayed>` then `$mod+6` | your focus did not move while away; the new terminal is in the right stack |
| 18 | UC9 | `$mod+Ctrl+n`, say a sentence, `$mod+Ctrl+n` | `<workstream>/notes/<timestamp>.md` contains the sentence; dictator icon in the bar cycled notes → idle |
| 19 | UC1 | `$mod+w` → *park* | slot is empty; the bar shows `WS — (Super+t to pick)`; `jug ls` shows the workstream `parked` |
| 20 | UC1 | `$mod+Shift+t` on the slot | label restored to the original; `jug ls` unchanged; `$mod+6` still reaches it |
| 21 | UC2 | `$mod+t` from a non-slot workspace with no slot toggled | the current workspace becomes the slot (label `WS`) and the pick is shown |
| 22 | UC11 | `jug close <ws>` for each test workstream | all their windows gone (`swaymsg -t get_tree` has no `jug:` marks, no `jug-oc:`/`jug-term:` app_ids); directories remain |
| 23 | UC12 | `jug pick --print`, `jug ls`, `jug current` | rows/table match reality; `current` exits 1 with nothing displayed |
| 24 | — | close opencode with `$mod+Shift+q`, then `$mod+w` → *term* | the left stack is rebuilt with a fresh opencode on the pinned session (self-repair); same after closing every right-stack window |
| 25 | — | close opencode in a workstream, park it (`$mod+w` → *park*), then show it again | it comes back with opencode rebuilt — a missing stack must not abort the show; `jug current` agrees with what is on screen |
| 26 | — | hand-edit `state.json` to `"displayed": ""` while a workstream is on the slot, then `jug current` | prints the displayed workstream (state is re-derived from the tree on every run) |
| 27 | — | `$mod+w` with nothing displayed, and with a workstream that has no `pr` ref | menu shows only *pick*/*lot*/*slot*; no *pr* row; `Esc` closes it and focus returns to the previous window |
| 28 | — | with a workstream displayed and others parked: `jug park --others` | no-op (everything already in the lot); `swaymsg -t get_tree` shows no `jug:` marks under `__i3_scratch` |
| 29 | — | `swaymsg reload` while a workstream is displayed | windows survive; bar module restarts and shows the same text |
| 30 | UC7b | two workstreams on two clones of the same repo, show one then the other | each opencode shows its own session (no bleed-through — the thing `--continue` gets wrong) |
| 31 | UC10 | `jug add --repo <name> --branch t-wt "wt test"` | `~/workstreams/…/src/<repo>/` exists, `.git` is a file, `git worktree list` shows it on `t-wt` at the remote HEAD; `jug ls` shows `./src/<repo>` and the branch; the repo's own build works there (e.g. `make version`) |
| 32 | UC10 | repeat with the same `--branch` | refused: "already checked out at …" |
| 33 | UC11 | `jug rm <ws>` → dry run; edit a file; `jug rm <ws> --yes` → refused (dirty); `--yes --force` | directory and worktree gone, branch still exists, `git worktree list` clean |
| 34 | UC10 | with `~/.config/juggler/seed/<repo>/.envrc` present: `jug add --repo <repo> …`, then `cd` into the worktree | `.envrc` is there with `{{id}}` etc. substituted, already allowed (`direnv status`), and takes effect; `jug rm <ws> --yes` succeeds without `--force` (the seeded file is not "dirty") |
| 35 | UC10 | `$mod+t` → `+ new workstream…`, type `AISW-1 picker test`, Enter, choose the repo row | `work_AISW-1_picker-test/` with a `jira` ref and `src/<repo>` worktree on branch `AISW-1`; it is shown. Repeat with `personal: just notes` → `no code` → a `personal_tim-NNNN_just-notes/`, code dir = the workstream dir. `Esc` at the second prompt creates nothing |
| 36 | UC10c | `jug set <ws> --jira AISW-2 --desc "renamed"` on a displayed workstream with a worktree | directory renamed to `work_AISW-2_renamed`, `git worktree list` shows the new path (nothing prunable), `direnv status` allowed, `jug session` shows the new title, windows reopened on the same session, your focused workspace unchanged |
| 37 | UC10b | `scripts/relocate-venv.sh <checkout>/.venv ~/venvs/x` | prints the move; `~/venvs/x/bin/python -c 'import sys;print(sys.prefix)'` is the new path; `~/venvs/x/bin/pip --version` runs; `source ~/venvs/x/bin/activate` sets `VIRTUAL_ENV` to the new path and the prompt to `(x)` |
| 38 | web | `make service-install && make service-enable`, open http://127.0.0.1:7474/ | header shows the slot and the displayed workstream; the table matches `jug ls`; the live dot is green |
| 39 | web | *+ new* → description `web test`, ticket `AISW-1`, worktree of a repo, *show it now* → create | row appears as displayed, the slot shows it, detail pane opens; `jug ls` agrees |
| 40 | web | in the detail pane: change the description, save; add a `url` ref; edit TODO.md and save; *choose existing…* and pick a session | directory renamed (`jug ls`), ref listed with its link, TODO count updated, session pinned (`jug session --ws AISW-1`) |
| 41 | web | *term*, *notes*, *open jira*, *park*, *show* from the pane | each does what the key does; after *park* the row is `◐` and the header says nothing displayed |
| 42 | web | *remove workstream…* → read the plan → confirm | row gone, directory gone, worktree removed (`git worktree list`), branch kept |
| 43 | api | `curl -s -X POST …/workstreams/nope/show` and `curl -s …/workstreams/x` with the service stopped | `404 not_found` / connection refused; with sway gone (e.g. from a tty) every window endpoint answers `503 sway_unavailable` and CRUD still works |
| 44 | api | `JUG_SERVE_ANY= jug serve --listen 0.0.0.0:7474` | refuses to start |
| 45 | UC14 | `jug group set s-now --kind sprint --start <today> --end <today+13>`; `jug group set s-next --start <today+14> --end <today+27>`; `jug group add s-now <ws>` | `jug group ls` shows `s-now *` with 14d left and 1 member, not `s-next`; `--all` shows both, `s-now` first; `~/workstreams/groups.toml` has both, the workstream's `workstream.toml` has `groups = ["s-now"]` |
| 46 | UC14 | web UI, grouped view | a section per group in end-date order (current one with the green bar and "Nd left"), then *no group*; clicking a header collapses it and the state survives a reload; the header chip shows `sprint: s-now · 14d left` |
| 47 | UC14 | in a workstream's detail pane tick `s-next`, then untick `s-now`; type a new name in "add to group" | each change saves (`jug ls --group …`), the list regroups live; the new group appears as a section; unticking the last member of an unregistered group makes it disappear |
| 48 | UC14 | *groups…*: change `s-now`'s end date with the picker, save; create `proj-x` with no dates; delete `s-next` (empty) and then `s-now` (choose "untag") | `jug group ls --all` reflects each step; after the last one the workstream has no `s-now` tag |
| 49 | UC14 | `jug group set x --start 2026-02-01 --end 2026-01-01`; `jug group add "a/b" <ws>` | both rejected with a reason |

Automated: `make test` (store naming/resolution, picker rows and the
new-workstream spec, sway-safe shell quoting, group validation/ordering and
the registry, the service layer's store-only paths, and the HTTP API end to
end against a temp store without sway). The
sway mechanics are covered by `make poc` and `make poc-lot`; `make ui-shot`
renders the UI headlessly (list + detail) and fails on console errors.

## Limitations and roadmap

- **The lot needs `workspace_layout stacking` (or `tabbed`).** The first park
  relies on sway wrapping a sole container in a new workspace; with the
  default `workspace_layout` there is no wrapper to turn into the lotbox and
  `jug park` reports it. (`jug doctor` checks the setting.)
- **A fresh worktree is a fresh dev environment.** Ignored artifacts
  (virtualenvs, downloaded deps, generated code, local `*.mk`) are per
  checkout, so the first build in a new worktree pays the full setup cost and
  disk (for ezaddon-mlis: ~2–3 GB once built vs ~0.5 GB bare).
- **Same commit, same version.** ezaddon-mlis derives the image tag from
  `git describe` and drops `-dirty`, so two worktrees at the same commit —
  uncommitted changes or not — build the *same* tag and the last push wins.
  Commit (even WIP) in each worktree before `make release-local`, or override
  with `make -e VERSION=…`. Each cluster also holds one deployed MLIS, so
  simultaneous deploys need distinct clusters (kontext).
- **The web UI has no authentication.** It is a local control panel bound
  to loopback; anything that can reach the port can run `jug` as you.
- **Session operations start a short-lived `opencode serve`** (~1 s) — on
  the first show of a workstream and for `jug session …`. Set
  `opencode_sessions = false` to launch the configured command as-is.
- **The lot is one extra workspace in the bar** (`WS+`), present only while
  something is parked. Tab titles there are sway's container representation
  plus the `jug:` mark.
- **Browser windows flash once** when opened for a workstream while you are
  on another workspace: Chrome ignores launch context, so the window appears
  where you are for a frame before it is moved. Opened from a hotkey on the
  slot there is no flash.
- **A sway restart loses the windows** (not the files): juggler rebuilds a
  workstream the next time you show it and `opencode -s <pinned id>` resumes
  the conversation.
- **Refs and groups are static** until the importer lands (`jug sync`, stage
  three): a timer that queries Jira for tickets assigned to you and GitHub
  for the branch's PR, writes the cached `status`/`title`/`updated`, creates
  workstreams for new tickets, registers each sprint as a group with its
  dates and tags tickets into their sprint or `backlog`. The data model
  already has the fields.
- **opencode integration** (stage three): a skill so the assistant keeps `TODO.md`
  current, records follow-up tickets there, and calls `jug ref add` /
  `jug open pr` when it opens a PR.
- Then: nvim picker (`:Jug`), notifications when CI goes red or a review
  arrives, a calendar importer, archiving finished workstreams.
