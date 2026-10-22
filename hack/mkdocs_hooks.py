"""MkDocs hooks for the documentation site.

Links in the Markdown files are written for GitHub, where the files sit in
their real folders. On the site, everything sits in docs/. These hooks change
the few link forms that differ:

- README.md, CHANGELOG.md and CONTRIBUTING.md stay in the repository root and
  reach the site through symlinks in docs/. Their links point into docs/
  (for example `docs/INSTALL.md`); on the site the `docs/` prefix goes.
- Pages in docs/ link to those root files as `../CHANGELOG.md`; on the site
  the same file is `CHANGELOG.md`.
- docs/examples is a symlink to examples/. Its pages link to `../docs/X.md`;
  on the site that is `../X.md`.
"""

import re

ROOT_PAGES = {"README.md", "CHANGELOG.md", "CONTRIBUTING.md"}
_DOCS_LINK = re.compile(r"\]\(docs/")
_ROOT_FILE_LINK = re.compile(r"\]\(\.\./(README\.md|CHANGELOG\.md|CONTRIBUTING\.md|LICENSE)")
_EXAMPLES_DOCS_LINK = re.compile(r"\]\(\.\./docs/")


def on_page_markdown(markdown, page, config, files):
    src = page.file.src_uri
    if src in ROOT_PAGES:
        return _DOCS_LINK.sub("](", markdown)
    if src.startswith("examples/"):
        return _EXAMPLES_DOCS_LINK.sub("](../", markdown)
    if "/" not in src:
        return _ROOT_FILE_LINK.sub(r"](\1", markdown)
    return markdown
