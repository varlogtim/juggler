#!/usr/bin/env bash
# rename-workspaces.sh — one-time migration of LIVE sway workspaces to the
# "N:name" scheme juggler needs (see README "Install").
#
# Reads `set $wsN "name"` lines from the sway config to learn which number a
# name belongs to, then renames every live workspace that has no number.
# Idempotent: already-numbered workspaces are left alone.
set -euo pipefail

CONFIG=${SWAY_CONFIG:-$HOME/.config/sway/config}

# name -> number, from lines like:  set $ws4 "4:󰾗"   or   set $ws4 "󰾗"
declare -A num_of
while IFS= read -r line; do
	[[ $line =~ ^[[:space:]]*set[[:space:]]+\$ws([0-9]+)[[:space:]]+\"?([^\"]+)\"?[[:space:]]*$ ]] || continue
	n=${BASH_REMATCH[1]}; name=${BASH_REMATCH[2]}
	name=${name#"$n:"}            # strip an existing "N:" prefix
	num_of["$name"]=$n
done < "$CONFIG"

if ((${#num_of[@]} == 0)); then
	echo "no 'set \$wsN' lines found in $CONFIG" >&2
	exit 1
fi

renamed=0
while IFS=$'\t' read -r num name; do
	if (( num >= 0 )); then
		continue
	fi
	n=${num_of["$name"]:-}
	if [[ -z $n ]]; then
		echo "skip: workspace \"$name\" has no \$wsN in the config" >&2
		continue
	fi
	echo "rename \"$name\" -> \"$n:$name\""
	swaymsg -q -- "rename workspace \"$name\" to \"$n:$name\""
	renamed=$((renamed+1))
done < <(swaymsg -t get_workspaces | jq -r '.[] | select(.name != "__i3_scratch") | "\(.num)\t\(.name)"')

echo "renamed $renamed workspace(s)"
