#!/usr/bin/env bash
# lot-poc.sh — verifies the sway moves juggler's "lot" (parked workstreams)
# relies on; see README "How it works". Opens and closes a few alacritty
# windows on scratch workspaces and steals focus for a few seconds.
#
#   T1  first park: a tiling root moved straight into an EMPTY workspace is
#       dissolved, so it hops through the scratchpad (floating) and is tiled on
#       arrival; the empty workspace wraps it — that wrapper becomes the lotbox
#   T2  later parks: `move container to mark $LOTMARK` adds a root as a tab, intact
#   T3  restore into the slot while another workspace is visible: strays adopted
#       into the hidden root first, then scratchpad hop; focus/visibility unchanged
#   T4  swap: park the displayed root by mark, restore another — tabs stay intact
#   T5  a root parked in the scratchpad (older juggler) can be moved into the lotbox
#   T6  visiting the lot shows one tab per parked workstream
#   T7  the lot workspace disappears with its last tab
#
# Shapes are printed as  layout{marks}[children…]  with "v" for a view.
set -u
msg(){ swaymsg -q -- "$@"; }
msgv(){ swaymsg -- "$@" | jq -c '[.[] | select(.success|not) | .error]'; }
tree(){ swaymsg -t get_tree; }
ws_node(){ tree | jq -c --arg n "$1" '.. | select(.type? == "workspace" and .name == $n)'; }
shape(){ ws_node "$1" | jq -r 'def s: if (.nodes|length)==0 then "v" else "\(.layout)\(if (.marks|length)>0 then "{"+(.marks|join(","))+"}" else "" end)[\([.nodes[] | s] | join(" "))]" end; [.nodes[] | s] | join(" ")'; }
wait_app(){ for i in $(seq 1 50); do c=$(tree | jq -r --arg a "$1" '[.. | select(.app_id? == $a) | .id] | first // empty'); [ -n "$c" ] && { echo $c; return; }; sleep 0.1; done; return 1; }
pass=0; fail=0; ok(){ echo "PASS  $*"; pass=$((pass+1)); }; bad(){ echo "FAIL  $*"; fail=$((fail+1)); }
orig_ws=$(swaymsg -t get_workspaces | jq -r '.[]|select(.focused)|.name'); orig_focus=$(tree | jq -r '.. | select(.focused? == true) | .id' | head -1)
# Scratch names only — NEVER juggler's real slot/lot. The lotbox mark is also
# a scratch one for the same reason.
SLOT=jugpoc-slot; LOT=jugpoc-lot; LOTMARK=jugpoc:lot
for w in $SLOT $LOT; do
	if swaymsg -t get_workspaces | jq -e --arg w "$w" '.[] | select(.name == $w)' >/dev/null; then
		echo "error: workspace $w already exists; refusing to run" >&2; exit 1
	fi
done
build(){ local n=$1
  msg "exec alacritty --class poc-oc-$n -o window.dynamic_title=false -T 'oc $n' -e sh -c 'sleep 600'"; oc=$(wait_app poc-oc-$n)
  msg "[con_id=$oc] focus; focus parent"
  msg "exec alacritty --class poc-term-$n -o window.dynamic_title=false -T 'term $n' -e sh -c 'sleep 600'"; term=$(wait_app poc-term-$n)
  msg "[con_id=$term] focus; splitv; layout stacking; [con_id=$oc] focus; focus parent; focus parent; splith"
  root=$(ws_node $SLOT | jq -r '.nodes[0].id'); left=$(ws_node $SLOT | jq -r '.nodes[0].nodes[0].id'); right=$(ws_node $SLOT | jq -r '.nodes[0].nodes[1].id')
  msg "[con_id=$root] mark --add jug:$n; [con_id=$left] mark --add jug:$n:left; [con_id=$right] mark --add jug:$n:right; [con_id=$left] resize set width 33 ppt"
}
msg "workspace $SLOT"; build A
# T1 first park: scratchpad hop -> lot (floating) -> unfloat (empty lot => wrapped) -> wrapper = lotbox
e=$(msgv "[con_mark=^jug:A\$] move scratchpad; [con_mark=^jug:A\$] move container to workspace \"$LOT\"; [con_mark=^jug:A\$] floating disable"); [ "$e" = "[]" ] || echo "  errors: $e"
s=$(shape "$LOT"); echo "  lot: $s"
wrapper=$(ws_node "$LOT" | jq -r '.nodes[0] | select(.marks | index("jug:A") | not) | .id')
if [ -n "$wrapper" ] && [[ $s == *'{jug:A}'* ]]; then ok "T1 root A intact and wrapped in the lot (wrapper $wrapper)"; else bad "T1 lot=$s"; fi
e=$(msgv "[con_id=$wrapper] mark --add $LOTMARK; [con_mark=^jug:A\$] layout tabbed"); [ "$e" = "[]" ] || echo "  errors: $e"
s=$(shape "$LOT"); [[ $s == "tabbed{$LOTMARK}[splith{jug:A}["* ]] && ok "T1b lotbox is tabbed: $s" || bad "T1b $s"
vis=$(swaymsg -t get_workspaces | jq -r '.[]|select(.visible)|.name'); [ "$vis" = "$SLOT" ] && ok "T1c lot stayed hidden (visible=$vis)" || bad "T1c visible=$vis"
# T2 second park straight into the lotbox (tiling -> container destination)
build B
e=$(msgv "[con_mark=^jug:B\$] move container to mark $LOTMARK"); [ "$e" = "[]" ] || echo "  errors: $e"
s=$(shape "$LOT"); [[ $s == "tabbed{$LOTMARK}[splith{jug:A}["*"splith{jug:B}["* ]] && ok "T2 B is a second tab, intact: $s" || bad "T2 $s"
aw=$(tree | jq -r '.. | select(.marks? and (.marks|index("jug:A:left"))) | .rect.width'); [ -n "$aw" ] && [ "$aw" -gt 0 ] && [ "$aw" -lt 1500 ] && ok "T2b parked A keeps its ~1/3 width (left=${aw}px)" || bad "T2b left=$aw"
echo "  slot now: $(shape $SLOT)"
# T3 stray in slot, restore A while elsewhere: adopt the stray into hidden A
#    first, then the scratchpad hop, then put focus back — one batch, exactly
#    what layout.restore sends
msg "exec alacritty --class poc-stray -o window.dynamic_title=false -e sh -c 'sleep 600'"; stray=$(wait_app poc-stray)
msg "workspace jugpoc-other; exec alacritty --class poc-other -o window.dynamic_title=false -e sh -c 'sleep 600'"; other=$(wait_app poc-other); sleep 0.2
e=$(msgv "[con_id=$stray] move container to mark jug:A:right"); [ "$e" = "[]" ] || echo "  errors: $e"
inA=$(tree | jq -r --argjson s $stray '.. | select(.marks? and (.marks|index("jug:A:right"))) | [.nodes[].id] | index($s) != null'); [ "$inA" = true ] && ok "T3 stray adopted into hidden A's right stack" || bad "T3 inA=$inA"
restore(){ # NAME — the engine's restore batch, focus put back on $other
	msgv "[con_mark=^jug:$1\$] move scratchpad; [con_mark=^jug:$1\$] move container to workspace $SLOT; [con_mark=^jug:$1\$] floating disable; [con_mark=^jug:$1\$] split none; [con_mark=^jug:$1:left\$] resize set width 33 ppt; [con_id=$other] focus"
}
e=$(restore A); [ "$e" = "[]" ] || echo "  errors: $e"
s=$(shape $SLOT); lw=$(tree | jq -r '.. | select(.marks? and (.marks|index("jug:A:left"))) | .rect.width')
[[ $s == 'splith{jug:A}[stacked{jug:A:left}[v] stacked{jug:A:right}[v v]]' && $lw -gt 0 && $lw -lt 1500 ]] && ok "T3b A restored: $s left=$lw" || bad "T3b $s left=$lw"
vis=$(swaymsg -t get_workspaces | jq -r '.[]|select(.visible)|.name'); foc=$(tree | jq -r '.. | select(.focused? == true) | .id' | head -1)
[ "$vis" = jugpoc-other ] && [ "$foc" = "$other" ] && ok "T3c restore did not steal focus/visibility" || bad "T3c visible=$vis focus=$foc"
# T4 swap: park A (into the lotbox by mark), restore B — the lot now holds exactly A
e=$(msgv "[con_mark=^jug:A\$] move container to mark $LOTMARK; [con_id=$other] focus"); [ "$e" = "[]" ] || echo "  errors(park A): $e"
e=$(restore B); [ "$e" = "[]" ] || echo "  errors: $e"
s=$(shape "$LOT"); [[ $s == "tabbed{$LOTMARK}[splith{jug:A}["*"]]" && $(shape $SLOT) == 'splith{jug:B}['* ]] && ok "T4 swapped: slot=B, lot holds exactly A" || bad "T4 lot=$s slot=$(shape $SLOT)"
# T5 legacy: a root sitting in the scratchpad (parked by an older juggler) goes
#    into the lotbox by mark after being tiled somewhere first
e=$(msgv "[con_mark=^jug:B\$] move scratchpad; [con_mark=^jug:B\$] move container to workspace $SLOT; [con_mark=^jug:B\$] floating disable; [con_mark=^jug:B\$] move container to mark $LOTMARK; [con_id=$other] focus"); [ "$e" = "[]" ] || echo "  errors: $e"
s=$(shape "$LOT"); [[ $s == *'{jug:A}'* && $s == *'{jug:B}'* && -z $(shape $SLOT) ]] && ok "T5 scratchpad root moved into the lotbox: 2 tabs, slot empty" || bad "T5 lot=$s slot=$(shape $SLOT)"
# T6 visit the lot
msg "workspace \"$LOT\""; sleep 0.3
tabs=$(ws_node "$LOT" | jq -r '.nodes[0].nodes | length'); [[ $(swaymsg -t get_workspaces | jq -r '.[]|select(.visible)|.name') == "$LOT" && $tabs == 2 ]] && ok "T6 visiting the lot shows $tabs tabs" || bad "T6 tabs=$tabs"
# T7 the lot disappears with its last tab (what `jug lot` uses to say "nothing is parked")
msg "workspace jugpoc-other"; e=$(restore A); [ "$e" = "[]" ] || echo "  errors: $e"
for a in poc-oc-B poc-term-B; do msg "[app_id=^$a\$] kill"; done; sleep 0.5
if swaymsg -t get_workspaces | jq -e --arg w "$LOT" '.[] | select(.name == $w)' >/dev/null; then bad "T7 lot workspace still exists: $(shape "$LOT")"; else ok "T7 lot workspace gone once its last tab left"; fi
echo "  scratchpad marks: $(tree | jq -c '.. | select(.name? == "__i3_scratch") | [.floating_nodes[] | .marks[]?]')"
for a in poc-oc-A poc-term-A poc-oc-B poc-term-B poc-other poc-stray; do msg "[app_id=^$a\$] kill"; done; sleep 0.4
msg "workspace \"$orig_ws\""; msg "[con_id=$orig_focus] focus"
echo "workspaces: $(swaymsg -t get_workspaces | jq -c '[.[]|.name]')"; echo "RESULT pass=$pass fail=$fail"
