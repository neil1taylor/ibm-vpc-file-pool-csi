#!/usr/bin/env bash
# test-doc-links.sh — Every guide link in SUPPORT.md, and every guide link in the console's
# help-links file, must point at a file and a heading that exist in this repository.
#
# Usage: bash test/chart/test-doc-links.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
exec python3 -I - <<'PY'
import os, re, sys

REPO_URL = "https://github.com/neil1taylor/ibm-vpc-file-pool-csi/blob/main/"
failed = 0

def slug(heading):
    # GitHub's rule: lower case, drop everything but letters, digits, space, - and _, then space -> -.
    h = re.sub(r"[`*_]", lambda m: "_" if m.group(0) == "_" else "", heading.strip().lower())
    h = re.sub(r"[^a-z0-9 _-]", "", h)
    return h.replace(" ", "-")

def anchors(path):
    out = set()
    in_code = False
    for line in open(path, encoding="utf-8"):
        if line.startswith("```"):
            in_code = not in_code
        elif not in_code and line.startswith("#"):
            out.add(slug(line.lstrip("#")))
    return out

def check(source, target):
    global failed
    path, _, frag = target.partition("#")
    if not path:
        path = source
    if not os.path.isfile(path):
        print(f"FAIL  {source}: no file {path}")
        failed = 1
        return
    if frag and frag not in anchors(path):
        print(f"FAIL  {source}: no heading #{frag} in {path}")
        failed = 1
        return
    print(f"PASS  {source}: {target}")

# 1. SUPPORT.md: [text](FILE.md) and [text](FILE.md#heading) links, and bare https links to this repo.
for m in re.finditer(r"\]\(([^)\s]+)\)", open("SUPPORT.md", encoding="utf-8").read()):
    t = m.group(1)
    if t.startswith("http"):
        continue
    check("SUPPORT.md", t)

# 2. The console's help links file.
links = "console-plugin/src/utils/help-links.ts"
if os.path.isfile(links):
    for m in re.finditer(r"'(" + re.escape(REPO_URL) + r"[^']*)'|`\$\{GUIDE_BASE\}([^`]*)`", open(links, encoding="utf-8").read()):
        t = m.group(1)[len(REPO_URL):] if m.group(1) else m.group(2)
        check(links, t)

sys.exit(failed)
PY
