#!/usr/bin/env bash
# m0-sway-poc.sh — milestone 0: verify the sway tree mechanics juggler relies on.
#
# Runs on a throwaway workspace, prints a PASS/FAIL line per fact, then cleans
# up (kills the windows it opened, returns to the workspace you were on).
# It WILL steal focus for a few seconds.
#
# Facts under test (see README "How it works"):
#   F1  a view exec'd into an empty workspace is wrapped in a stacking container
#       (workspace_layout stacking)
#   F2  with the *workspace* focused (focus parent from a top-level container),
#       the next exec'd view lands beside the stack, not inside it
#   F3  `splitv; layout stacking` on that sibling turns it into a stack
#   F4  `splith` with the workspace focused wraps all children in ONE container
#       (that wrapper is the workstream root we mark and park)
#   F5  `resize set width 33ppt` on the left stack gives ~1/3 of the workspace
#   F6  the root container (with children) can be parked in the scratchpad as a
#       unit: `[con_mark=root] move scratchpad`
#   F7  it comes back intact: `scratchpad show; floating disable`, and the
#       33/67 split can be re-applied
#   F8  launcher context: focusing a container on a non-visible workspace, exec,
#       and refocusing the previous window in ONE IPC message spawns the view
#       inside that container without the visible workspace changing or the
#       new view taking focus
#   F9  where a browser window (google-chrome, single-instance) spawned the same
#       way ends up: in the right stack (launcher ctx honoured) or elsewhere
#       (needs the detect-and-move fallback)
set -u

WS=jugtest
WS2=jugtest2
TERM_CMD=${TERM_CMD:-alacritty}
BROWSER_CMD=${BROWSER_CMD:-google-chrome}
pass=0; fail=0
ok()   { echo "PASS  $*"; pass=$((pass+1)); }
bad()  { echo "FAIL  $*"; fail=$((fail+1)); }
msg()  { swaymsg -q -- "$@"; }
tree() { swaymsg -t get_tree; }

# node_for_ws NAME -> JSON of the workspace node
ws_node() { tree | jq -c --arg n "$1" '.. | select(.type? == "workspace" and .name == $n)'; }
# wait_app_id APP_ID [secs] -> con_id of the first view with that app_id
wait_app_id() {
	local id="$1" n=${2:-50} i
	for ((i=0; i<n; i++)); do
		local cid
		cid=$(tree | jq -r --arg a "$id" '[.. | select(.app_id? == $a) | .id] | first // empty')
		[[ -n "$cid" ]] && { echo "$cid"; return 0; }
		sleep 0.1
	done
	return 1
}
focused_id()   { tree | jq -r '.. | select(.focused? == true) | .id' | head -1; }
visible_ws()   { swaymsg -t get_workspaces | jq -r '.[] | select(.visible) | .name'; }
focused_ws()   { swaymsg -t get_workspaces | jq -r '.[] | select(.focused) | .name'; }

orig_ws=$(focused_ws)
orig_focus=$(focused_id)
echo "orig workspace=$orig_ws focus=$orig_focus"
echo

# ---------------------------------------------------------------- F1
msg "workspace $WS"
msg "exec $TERM_CMD --class jug-poc-oc -o window.dynamic_title=false -T 'POC opencode' -e sh -c 'sleep 600'"
oc=$(wait_app_id jug-poc-oc) || { bad "F1 opencode view never appeared"; exit 1; }
shape=$(ws_node $WS | jq -r '[.nodes[] | "\(.layout):\(.nodes|length)"] | join(",")')
[[ "$shape" == "stacked:1" ]] && ok "F1 ws=[stacked:1] (workspace_layout stacking wraps the view)" || bad "F1 shape=$shape"

# ---------------------------------------------------------------- F2
msg "[con_id=$oc] focus; focus parent"
msg "exec $TERM_CMD --class jug-poc-term -o window.dynamic_title=false -T 'POC term' -e sh -c 'sleep 600'"
term=$(wait_app_id jug-poc-term) || { bad "F2 term view never appeared"; exit 1; }
shape=$(ws_node $WS | jq -r '[.nodes[] | "\(.layout):\(.nodes|length)"] | join(",")')
[[ "$shape" == "stacked:1,none:0" ]] && ok "F2 ws=[stacked:1, view] (sibling of the stack, not inside)" || bad "F2 shape=$shape"

# ---------------------------------------------------------------- F3
msg "[con_id=$term] focus; splitv; layout stacking"
shape=$(ws_node $WS | jq -r '[.nodes[] | "\(.layout):\(.nodes|length)"] | join(",")')
[[ "$shape" == "stacked:1,stacked:1" ]] && ok "F3 ws=[stacked:1, stacked:1] (splitv; layout stacking)" || bad "F3 shape=$shape"

# ---------------------------------------------------------------- F4
msg "[con_id=$oc] focus; focus parent; focus parent; splith"
shape=$(ws_node $WS | jq -r '[.nodes[] | "\(.layout):\(.nodes|length)"] | join(",")')
inner=$(ws_node $WS | jq -r '[.nodes[0].nodes[] | "\(.layout):\(.nodes|length)"] | join(",")')
if [[ "$shape" == "splith:2" && "$inner" == "stacked:1,stacked:1" ]]; then
	ok "F4 ws=[splith[stacked:1, stacked:1]] (wrapper container created)"
else
	bad "F4 shape=$shape inner=$inner"
fi
root=$(ws_node $WS | jq -r '.nodes[0].id')
left=$(ws_node $WS | jq -r '.nodes[0].nodes[0].id')
right=$(ws_node $WS | jq -r '.nodes[0].nodes[1].id')
msg "[con_id=$root] mark --add jug:poc; [con_id=$left] mark --add jug:poc:left; [con_id=$right] mark --add jug:poc:right"
marks=$(tree | jq -r '[.. | select(.marks? != null) | .marks[] | select(startswith("jug:poc"))] | sort | join(",")')
[[ "$marks" == "jug:poc,jug:poc:left,jug:poc:right" ]] && ok "F4b marks on split containers: $marks" || bad "F4b marks=$marks"

# ---------------------------------------------------------------- F5
msg "[con_mark=^jug:poc:left\$] resize set width 33ppt"
wsw=$(ws_node $WS | jq -r '.rect.width')
lw=$(tree | jq -r --argjson id "$left" '.. | select(.id? == $id) | .rect.width')
pct=$(( lw * 100 / wsw ))
(( pct >= 31 && pct <= 35 )) && ok "F5 left width ${lw}px = ${pct}% of ${wsw}px" || bad "F5 left ${lw}px = ${pct}%"

# ---------------------------------------------------------------- F6
msg "[con_mark=^jug:poc\$] move scratchpad"
sp=$(tree | jq -c '.. | select(.name? == "__i3_scratch") | .floating_nodes[] | select(.marks | index("jug:poc")) | {id, layout, children: [.nodes[] | "\(.layout):\(.nodes|length)"], views: [.. | select(.app_id?) | .app_id]}')
if [[ -n "$sp" ]] && [[ $(jq -r '.children | join(",")' <<<"$sp") == "stacked:1,stacked:1" ]]; then
	ok "F6 root parked in scratchpad as one unit: $sp"
else
	bad "F6 scratchpad content: ${sp:-<none>}  ws=$(ws_node $WS | jq -c '[.nodes[]|.layout]')"
fi
left_in_ws=$(ws_node $WS | jq -r '.nodes | length')
[[ "$left_in_ws" == "0" ]] && ok "F6b workspace emptied (would be reaped/reused)" || bad "F6b workspace still has $left_in_ws tiling children"

# ---------------------------------------------------------------- F7
msg "workspace $WS; [con_mark=^jug:poc\$] scratchpad show; [con_mark=^jug:poc\$] floating disable; [con_mark=^jug:poc:left\$] resize set width 33ppt"
shape=$(ws_node $WS | jq -r '[.nodes[] | "\(.layout):\(.nodes|length)"] | join(",")')
inner=$(ws_node $WS | jq -r '[.nodes[0].nodes[] | "\(.layout):\(.nodes|length)"] | join(",")')
lw=$(tree | jq -r --argjson id "$left" '.. | select(.id? == $id) | .rect.width')
wsw=$(ws_node $WS | jq -r '.rect.width')
pct=$(( lw * 100 / wsw ))
if [[ "$shape" == "splith:2" && "$inner" == "stacked:1,stacked:1" ]] && (( pct >= 31 && pct <= 35 )); then
	ok "F7 restored: ws=[splith[stacked:1, stacked:1]] left=${pct}%"
else
	bad "F7 shape=$shape inner=$inner left=${pct}%"
fi
floating=$(ws_node $WS | jq -r '.floating_nodes | length')
[[ "$floating" == "0" ]] && ok "F7b nothing left floating" || bad "F7b $floating floating nodes remain"

# ---------------------------------------------------------------- F8
msg "workspace $WS2; exec $TERM_CMD --class jug-poc-other -o window.dynamic_title=false -T 'POC other' -e sh -c 'sleep 600'"
other=$(wait_app_id jug-poc-other) || { bad "F8 'other' view never appeared"; exit 1; }
sleep 0.3
msg "[con_mark=^jug:poc:right\$] focus; focus child; exec $TERM_CMD --class jug-poc-spawned -o window.dynamic_title=false -T 'POC spawned' -e sh -c 'sleep 600'; [con_id=$other] focus"
spawned=$(wait_app_id jug-poc-spawned) || { bad "F8 spawned view never appeared"; exit 1; }
sleep 0.3
vis=$(visible_ws); foc=$(focused_id)
parent_is_right=$(tree | jq -r --argjson id "$right" --argjson v "$spawned" '.. | select(.id? == $id) | [.nodes[].id] | index($v) != null')
if [[ "$vis" == "$WS2" && "$foc" == "$other" && "$parent_is_right" == "true" ]]; then
	ok "F8 spawned into right stack of hidden workspace; visible=$vis focus unchanged"
else
	bad "F8 visible=$vis (want $WS2) focus=$foc (want $other) in_right_stack=$parent_is_right"
fi
right_n=$(tree | jq -r --argjson id "$right" '.. | select(.id? == $id) | .nodes | length')
echo "      right stack now has $right_n children"

# ---------------------------------------------------------------- F9
before=$(tree | jq -c '[.. | select(.app_id? == "google-chrome") | .id] | sort')
msg "[con_mark=^jug:poc:right\$] focus; focus child; exec $BROWSER_CMD --new-window 'https://example.com/?jug-poc'; [con_id=$other] focus"
newchrome=""
for i in $(seq 1 80); do
	newchrome=$(tree | jq -r --argjson b "$before" '[.. | select(.app_id? == "google-chrome") | .id] | sort | . - $b | first // empty')
	[[ -n "$newchrome" ]] && break
	sleep 0.1
done
if [[ -z "$newchrome" ]]; then
	bad "F9 no new chrome window within 8s"
else
	cws=$(tree | jq -r --argjson id "$newchrome" '.. | select(.type? == "workspace") | select([.. | select(.id? == $id)] | length > 0) | .name')
	in_right=$(tree | jq -r --argjson id "$right" --argjson v "$newchrome" '.. | select(.id? == $id) | [.nodes[].id] | index($v) != null')
	title=$(tree | jq -r --argjson id "$newchrome" '.. | select(.id? == $id) | .name')
	vis=$(visible_ws); foc=$(focused_id)
	if [[ "$in_right" == "true" ]]; then
		ok "F9 chrome window landed in the right stack (launcher ctx honoured) title=$title visible=$vis focus=$foc"
	else
		bad "F9 chrome window landed on ws=$cws in_right=$in_right title=$title visible=$vis focus=$foc -> detect-and-move fallback needed"
		# demonstrate the fallback: move it by mark
		msg "[con_id=$newchrome] move container to mark jug:poc:right"
		in_right=$(tree | jq -r --argjson id "$right" --argjson v "$newchrome" '.. | select(.id? == $id) | [.nodes[].id] | index($v) != null')
		[[ "$in_right" == "true" ]] && ok "F9b fallback: move container to mark jug:poc:right works" || bad "F9b fallback move failed"
	fi
	msg "[con_id=$newchrome] kill"
fi

# ---------------------------------------------------------------- cleanup
echo
echo "cleanup"
for a in jug-poc-oc jug-poc-term jug-poc-other jug-poc-spawned; do msg "[app_id=^$a\$] kill"; done
sleep 0.5
msg "workspace $orig_ws"
[[ -n "$orig_focus" ]] && msg "[con_id=$orig_focus] focus"
echo
echo "RESULT pass=$pass fail=$fail"
