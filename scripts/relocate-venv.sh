#!/usr/bin/env bash
# relocate-venv.sh OLD NEW — move a Python venv and fix the absolute paths
# baked into it, so it keeps working at the new location.
#
# A venv is not relocatable by plain `mv`: bin/activate* hardcode
# VIRTUAL_ENV, every console script in bin/ has the venv's python in its
# shebang, and pyvenv.cfg records the creating command. This script moves the
# directory and rewrites exactly those strings (and the "(name) " prompt), then
# verifies: python's sys.prefix, a console script (pip) via its shebang, and
# `source bin/activate` in a subshell.
#
# Not touched: editable installs (they point at SOURCE checkouts, not at the
# venv — re-run `pip install -e <src>` to re-point them), RECORD/direct_url
# metadata (informational), and symlinks (bin/python -> system python).
set -euo pipefail

usage() { echo "usage: relocate-venv.sh OLD NEW" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
old=$(realpath -s "$1"); new=$(realpath -sm "$2")
[[ -f $old/pyvenv.cfg ]] || { echo "error: $old is not a venv (no pyvenv.cfg)" >&2; exit 1; }
[[ ! -e $new ]] || { echo "error: $new already exists" >&2; exit 1; }
if ! [[ -x $old/bin/python ]]; then
	echo "error: $old/bin/python is not runnable (dangling interpreter symlink?) — recreate it instead:" >&2
	echo "       python3 -m venv $new" >&2; exit 1
fi

# The path embedded in the venv may differ from where it sits now (moved
# before without a fix-up); take it from bin/activate, falling back to OLD.
embedded=$(sed -n 's/^ *export VIRTUAL_ENV=\([^$"].*\)$/\1/p' "$old/bin/activate" | head -1)
embedded=${embedded:-$old}
oldbase=$(basename "$embedded"); newbase=$(basename "$new")
(( ${#new} + 12 <= 127 )) || echo "warning: $new/bin/python3 is longer than 127 chars; shebangs will break" >&2

mkdir -p "$(dirname "$new")"
mv "$old" "$new"
trap 'echo "error: fix-up failed; moving back" >&2; mv "$new" "$old"' ERR

esc() { printf '%s' "$1" | sed -e 's/[\/&|]/\\&/g'; }
E_OLD=$(esc "$embedded"); E_NEW=$(esc "$new")

# 1. activate scripts + pyvenv.cfg: the path, and the prompt label
for f in bin/activate bin/activate.csh bin/activate.fish bin/Activate.ps1 pyvenv.cfg; do
	[[ -f $new/$f ]] || continue
	sed -i -e "s|$E_OLD|$E_NEW|g" -e "s|($(esc "$oldbase")) |($(esc "$newbase")) |g" "$new/$f"
done

# 2. shebangs: regular files in bin/ whose first line is #!<embedded>/...
fixed=0
for f in "$new"/bin/*; do
	[[ -f $f && ! -L $f ]] || continue
	if head -c 2 "$f" 2>/dev/null | grep -q '^#!' && head -1 "$f" | grep -q "^#!$embedded/"; then
		sed -i "1s|^#!$E_OLD/|#!$E_NEW/|" "$f"; fixed=$((fixed+1))
	fi
done

# 3. verify
prefix=$("$new/bin/python" -c 'import sys; print(sys.prefix)')
[[ $prefix == "$new" ]] || { echo "error: sys.prefix is $prefix, want $new" >&2; false; }
if [[ -x $new/bin/pip ]]; then
	"$new/bin/pip" --version >/dev/null || { echo "error: bin/pip does not run (shebang?)" >&2; false; }
fi
venv_in_shell=$(bash -c "source '$new/bin/activate' && printf '%s %s' \"\$VIRTUAL_ENV\" \"\$(command -v python)\"")
[[ $venv_in_shell == "$new $new/bin/python" ]] || { echo "error: activate gave: $venv_in_shell" >&2; false; }

# 4. bytecode caches embed the source path (shown in tracebacks); drop them,
#    they are rebuilt lazily
find "$new" -type d -name __pycache__ -prune -exec rm -rf {} + 2>/dev/null || true

# leftovers: anything else still mentioning the old path (editable installs, RECORDs)
left=$({ grep -rl --exclude=RECORD --exclude=direct_url.json -F "$embedded" "$new" 2>/dev/null || true; } | wc -l)
trap - ERR
echo "moved  $embedded"
echo "   ->  $new   (shebangs fixed: $fixed; prompt: ($newbase); python: $("$new/bin/python" --version 2>&1))"
(( left == 0 )) || echo "   note: $left file(s) still mention the old path (likely editable-install finders pointing at source checkouts; harmless)"
