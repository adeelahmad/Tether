"""MkDocs hooks for the Tether docs site.

The Markdown in docs/ and the root community files are written to read well on
GitHub. This hook lets the site reuse them unchanged: it pulls root files into
pages that ask for them and rewrites repo-relative links to site pages.
"""

import os
import re

REPO = "https://github.com/adeelahmad/Tether/blob/main/"
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

INCLUDE = re.compile(r"<!--\s*include:\s*(\S+)\s*-->")

# Repo-relative link targets, as written in the root files or in docs/,
# mapped to where they live on the site.
LINKS = {
    "../SECURITY.md": "security.md",
    "SECURITY.md": "security.md",
    "CONTRIBUTING.md": "contributing.md",
    "CHANGELOG.md": "changelog.md",
    "CODE_OF_CONDUCT.md": REPO + "CODE_OF_CONDUCT.md",
    "LICENSE": REPO + "LICENSE",
}

LINK = re.compile(r"\]\(([^)#\s]+)(#[^)\s]*)?\)")


def _rewrite(match):
    target, anchor = match.group(1), match.group(2) or ""
    if target.startswith("docs/"):
        target = target[len("docs/"):]
    target = LINKS.get(target, target)
    return "](" + target + anchor + ")"


def on_page_markdown(markdown, page, config, files):
    # A page whose first line is an include marker is replaced by that file;
    # the rest of the page is a pointer for readers browsing docs/ on GitHub.
    include = INCLUDE.match(markdown)
    if include:
        with open(os.path.join(ROOT, include.group(1)), encoding="utf-8") as f:
            markdown = f.read()
    return LINK.sub(_rewrite, markdown)
