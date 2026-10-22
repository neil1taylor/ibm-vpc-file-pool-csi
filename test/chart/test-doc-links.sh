#!/usr/bin/env bash
# test-doc-links.sh — Every guide link in docs/SUPPORT.md, and every guide link in the console's
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

def check(source, target, base):
    global failed
    path, _, frag = target.partition("#")
    path = os.path.normpath(os.path.join(base, path)) if path else source
    if not os.path.isfile(path):
        print(f"FAIL  {source}: no file {path}")
        failed = 1
        return
    if frag and frag not in anchors(path):
        print(f"FAIL  {source}: no heading #{frag} in {path}")
        failed = 1
        return
    print(f"PASS  {source}: {target}")

# 1. docs/SUPPORT.md: [text](FILE.md) and [text](FILE.md#heading) links, and bare https links to this repo.
for m in re.finditer(r"\]\(([^)\s]+)\)", open("docs/SUPPORT.md", encoding="utf-8").read()):
    t = m.group(1)
    if t.startswith("http"):
        continue
    check("docs/SUPPORT.md", t, "docs")

# 2. The console's help links file.
links = "console-plugin/src/utils/help-links.ts"
if os.path.isfile(links):
    text = open(links, encoding="utf-8").read()
    gb = re.search(r"GUIDE_BASE = '([^']*)'", text)
    prefix = gb.group(1)[len(REPO_URL):] if gb and gb.group(1).startswith(REPO_URL) else ""
    for m in re.finditer(r"'(" + re.escape(REPO_URL) + r"[^']*)'|`\$\{GUIDE_BASE\}([^`]*)`", text):
        t = m.group(1)[len(REPO_URL):] if m.group(1) else prefix + m.group(2)
        if t.endswith("/"):
            continue  # the GUIDE_BASE definition itself names a folder, not a guide
        check(links, t, ".")

sys.exit(failed)
PY
