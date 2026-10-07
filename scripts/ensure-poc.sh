#!/usr/bin/env bash
# ensure-poc.sh — verify the sway facts behind juggler's layout repair (ensure).
#
# Runs on an empty throwaway workspace WITHOUT showing it: every batch is one
# IPC message that switches there, acts, and refocuses your window, so sway
# renders one frame and the visible workspace never changes (`exec`'d windows
# map into the recorded launch workspace; a view mapping into a non-visible
# workspace never takes focus). Prints PASS/FAIL per fact, then closes the
# windows it opened. Refuses to run on a workspace that has anything in it.
#
# Facts under test (see README "How it works" and layout.ensure):
#   E1  closing a stack's last view reaps the stack (its mark goes with it);
#       the root splith is NOT flattened — it survives with one child
#   E2  `move right` on a stack's only view puts it INTO the neighbouring
#       stack (the stack is reaped); `move left` from there promotes it to the
#       root as a BARE view, with the width fractions reset: that is the 50/50
#   E3  `floating toggle` twice lands the view inside the neighbouring stack
#   E4  the hop: `move scratchpad; [target] focus; move container to
#       workspace W; floating disable` makes a view a child of TARGET from
#       anywhere — a split container, because sway tiles a floating container
#       into the most recently focused tiling container of its workspace
#   E5  a view hopped into the ROOT lands as its last child; `splitv; layout
#       stacking` wraps it (it has a sibling), `swap container with mark
#       <right>` puts it first, `resize set width 33 ppt` gives the ⅓
#   E6  a repair applied to the wrong window: the same sequence on a view that
#       is ALREADY first inverts the order — why ensure checks the index
#       before swapping, and waits for a NEW window after exec
set -u

WS=${WS:-jugpoc}
TERM_CMD=${TERM_CMD:-alacritty}
pass=0; fail=0
ok()  { echo "PASS  $*"; pass=$((pass+1)); }
bad() { echo "FAIL  $*"; fail=$((fail+1)); }
tree() { swaymsg -t get_tree; }
focused_id() { tree | jq -r '.. | select(.focused? == true) | .id' | head -1; }
# one invisible batch: switch to the scratch workspace, act, refocus the user's window
batch() { local prev; prev=$(focused_id); swaymsg -q -- "workspace $WS; $*; [con_id=$prev] focus"; }
plain() { swaymsg -q -- "$@"; }
ws_json() { tree | jq -c --arg n "$WS" '.. | select(.type? == "workspace" and .name == $n)'; }
# shape of the scratch workspace: "splith[stacked[view] stacked[view view]]"
shape() { ws_json | jq -r 'def s: if (.app_id // .pid) then "view" else (.layout + "[" + ([.nodes[] | s] | join(" ")) + "]") end; [.nodes[] | s] | join(" ")'; }
by_app() { tree | jq -r --arg a "$1" '[.. | select(.app_id? == $a) | .id] | first // empty'; }
wait_new() { # wait_new APP_ID BEFORE_IDS(csv) -> id of a view with APP_ID not in BEFORE
	local a="$1" before=",$2," i id
	for ((i = 0; i < 100; i++)); do
		for id in $(tree | jq -r --arg a "$a" '.. | select(.app_id? == $a) | .id'); do
			[[ "$before" == *",$id,"* ]] || { echo "$id"; return 0; }
		done
		sleep 0.05
	done
	return 1
}
parent_of() { tree | jq -r --argjson t "$1" '.. | objects | select(has("nodes")) | select(any(.nodes[]?; .id == $t)) | .id' | head -1; }
index_in()  { tree | jq -r --argjson p "$1" --argjson t "$2" '.. | objects | select(.id? == $p) | .nodes | map(.id) | index($t)'; }
width_of()  { tree | jq -r --argjson t "$1" '.. | objects | select(.id? == $t) | .rect.width'; }
mark_of()   { tree | jq -r --arg m "$1" '.. | objects | select(.marks? and (.marks | index($m))) | .id' | head -1; }
spawn()     { echo "$TERM_CMD --class $1 -e sh -c 'sleep 600'"; }
cleanup()   { for a in jug-poc-e-oc jug-poc-e-term jug-poc-e-oc2; do local id; id=$(by_app "$a"); [[ -n "$id" ]] && plain "[con_id=$id] kill"; done; }
trap cleanup EXIT

if [[ -n "$(ws_json)" ]]; then
	echo "workspace $WS exists; pick an empty one: WS=name $0" >&2
	exit 2
fi

# --- build the canonical shape the way layout.build does
batch "exec $(spawn jug-poc-e-oc)"; OC=$(wait_new jug-poc-e-oc "") || { echo "no opencode stand-in appeared" >&2; exit 1; }
batch "[con_id=$OC] focus; focus parent; exec $(spawn jug-poc-e-term)"; TERM1=$(wait_new jug-poc-e-term "") || exit 1
plain "[con_id=$TERM1] splitv; [con_id=$TERM1] layout stacking"
L=$(parent_of "$OC"); R=$(parent_of "$TERM1")
plain "[con_id=$L] mark --add jug:poc:left; [con_id=$R] mark --add jug:poc:right"
batch "[con_id=$OC] focus; focus parent; focus parent; splith"
ROOT=$(parent_of "$L")
plain "[con_id=$ROOT] mark --add jug:poc; [con_id=$L] resize set width 33 ppt"
[[ "$(shape)" == "splith[stacked[view] stacked[view]]" ]] && ok "canonical shape built invisibly: $(shape)" || bad "canonical shape: $(shape)"

# --- E1
plain "[con_id=$OC] kill"; sleep 0.4
[[ -z "$(mark_of jug:poc:left)" && -n "$(mark_of jug:poc)" && "$(shape)" == "splith[stacked[view]]" ]] \
	&& ok "E1 stack reaped (mark gone), root survives with one child" || bad "E1 $(shape) left=$(mark_of jug:poc:left) root=$(mark_of jug:poc)"

# repair the way ensure does, with a NEW window
before=$(tree | jq -r '[.. | select(.app_id? == "jug-poc-e-oc") | .id] | join(",")')
batch "[con_id=$R] focus; exec $(spawn jug-poc-e-oc)"; OC=$(wait_new jug-poc-e-oc "$before") || exit 1
plain "[con_id=$OC] splitv; [con_id=$OC] layout stacking"; L=$(parent_of "$OC")
plain "[con_id=$L] mark --add jug:poc:left; [con_id=$L] swap container with mark jug:poc:right; [con_id=$L] resize set width 33 ppt"
[[ "$(index_in "$ROOT" "$L")" == 0 && "$(shape)" == "splith[stacked[view] stacked[view]]" ]] && ok "repair: new window wrapped, swapped first, resized (left $(width_of "$L")px)" || bad "repair $(shape)"

# --- E2
batch "[con_id=$OC] focus; move right"
[[ "$(parent_of "$OC")" == "$R" && -z "$(mark_of jug:poc:left)" ]] && ok "E2a move right: the view went INTO the right stack, its stack was reaped" || bad "E2a $(shape)"
batch "[con_id=$OC] focus; move left"
w=$(width_of "$OC"); total=$(width_of "$ROOT")
if [[ "$(parent_of "$OC")" == "$ROOT" && $((w * 10 / total)) == 5 ]]; then ok "E2b move left: bare view in the root at 50/50 ($w of $total)"; else bad "E2b parent=$(parent_of "$OC") root=$ROOT width=$w/$total"; fi

# E5 on the bare view (index 0: no swap)
plain "[con_id=$OC] splitv; [con_id=$OC] layout stacking"; L=$(parent_of "$OC")
plain "[con_id=$L] mark --add jug:poc:left; [con_id=$L] resize set width 33 ppt"
[[ "$(shape)" == "splith[stacked[view] stacked[view]]" && $(( $(width_of "$L") * 100 / total )) -le 34 ]] && ok "E5a bare view wrapped in place and resized to ⅓" || bad "E5a $(shape) $(width_of "$L")"

# --- E3
batch "[con_id=$OC] focus; floating toggle"; batch "[con_id=$OC] focus; floating toggle"
[[ "$(parent_of "$OC")" == "$R" ]] && ok "E3 floating toggle twice: inside the right stack" || bad "E3 parent=$(parent_of "$OC")"

# --- E4 + E5b: hop into the root, wrap, swap (index 1), resize
batch "[con_id=$OC] move scratchpad; [con_id=$ROOT] focus; [con_id=$OC] move container to workspace $WS; [con_id=$OC] floating disable"
[[ "$(parent_of "$OC")" == "$ROOT" && "$(index_in "$ROOT" "$OC")" == 1 ]] && ok "E4 hop with the root focused: bare, last child of the root" || bad "E4 parent=$(parent_of "$OC") index=$(index_in "$ROOT" "$OC")"
plain "[con_id=$OC] splitv; [con_id=$OC] layout stacking"; L=$(parent_of "$OC")
plain "[con_id=$L] mark --add jug:poc:left; [con_id=$L] swap container with mark jug:poc:right; [con_id=$L] resize set width 33 ppt"
[[ "$(index_in "$ROOT" "$L")" == 0 && $(( $(width_of "$L") * 100 / total )) -le 34 ]] && ok "E5b wrapped, swapped first, ⅓" || bad "E5b index=$(index_in "$ROOT" "$L") width=$(width_of "$L")"

# E4 into an existing stack: the view wanders into the right stack again, hop it into LEFT
batch "[con_id=$OC] focus; exec $(spawn jug-poc-e-oc2)"; X=$(wait_new jug-poc-e-oc2 "") || exit 1   # a second window keeps the left stack alive
batch "[con_id=$OC] focus; move right"
batch "[con_id=$OC] move scratchpad; [con_id=$L] focus; [con_id=$OC] move container to workspace $WS; [con_id=$OC] floating disable"
[[ "$(parent_of "$OC")" == "$L" ]] && ok "E4b hop with the left stack focused: its child again" || bad "E4b parent=$(parent_of "$OC")"
plain "[con_id=$X] kill"; sleep 0.3

# --- E6: the same repair on a window that is already first inverts the order
plain "[con_id=$L] swap container with mark jug:poc:right"
[[ "$(index_in "$ROOT" "$L")" == 1 ]] && ok "E6 swapping an already-first stack puts it last — the bug's shape ([right, left])" || bad "E6 index=$(index_in "$ROOT" "$L")"
plain "[con_id=$L] swap container with mark jug:poc:right"

echo
echo "pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
