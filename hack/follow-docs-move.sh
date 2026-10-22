#!/usr/bin/env bash
# After `git merge origin/main` stops on the docs move (root *.md files moved
# into docs/), carry this branch's edits of each moved file into docs/<file>.
# Git reports these as "deleted by them" (UD), because docs/<file> was a
# symlink before the move, so the move is not seen as a rename.
set -euo pipefail

moved=0
while IFS= read -r f; do
  [ -f "docs/$f" ] || continue
  base=$(mktemp)
  ours=$(mktemp)
  git show ":1:$f" > "$base"
  git show ":2:$f" > "$ours"
  if git merge-file -L "docs/$f" -L base -L "this branch" "docs/$f" "$base" "$ours"; then
    echo "carried over: $f -> docs/$f"
  else
    echo "CONFLICT: docs/$f has conflict markers; fix them by hand"
  fi
  find "$base" "$ours" -delete
  git rm -q --cached "$f"
  find . -maxdepth 1 -name "$f" -delete
  git add "docs/$f"
  moved=$((moved + 1))
done < <(git status --porcelain | awk '$1 == "UD" && $2 ~ /^[^\/]+\.md$/ {print $2}')

echo "$moved file(s) carried over. Check 'git status', then commit the merge."
