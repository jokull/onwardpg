#!/usr/bin/env python3
"""Print the exact tagged changelog section; refuse unreviewed release notes."""

import pathlib
import re
import sys


def release_notes(tag: str, changelog: str) -> str:
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:[.-][0-9A-Za-z.-]+)?", tag):
        raise ValueError("expected a SemVer-shaped release tag")
    sections = re.split(r"(?m)^## ", changelog)
    matches = [section for section in sections[1:] if section.splitlines()[0].split(" ")[0] == tag]
    if len(matches) != 1:
        raise ValueError(f"expected exactly one changelog section for {tag}")
    _, _, body = matches[0].partition("\n")
    if not body.strip():
        raise ValueError(f"empty changelog section for {tag}")
    return body.strip() + "\n"


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: release-notes.py vX.Y.Z[-preview.N]")
    try:
        print(release_notes(sys.argv[1], pathlib.Path("CHANGELOG.md").read_text()), end="")
    except ValueError as error:
        sys.exit(str(error))
