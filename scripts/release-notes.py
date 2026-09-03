#!/usr/bin/env python3
"""Extract one version's section from CHANGELOG.md.

The release job's notes come from the changelog rather than from generated
commit titles, and the job FAILS when the section is missing. That is the
whole enforcement mechanism behind "a release has notes": the tag cannot
become a release until someone has written down what changed.

A pre-release tag (v0.1.0-rc.1) falls back to its base version's section
(v0.1.0), because a release candidate is a candidate FOR that release and
does not get a changelog entry of its own.

Usage:
  release-notes.py <version> [CHANGELOG.md]   print the section, or fail
  release-notes.py --selftest                 run the checks below
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

HEADING = re.compile(r"^## +(?P<version>\S+)(?P<rest>.*)$", re.M)


def base_version(version: str) -> str:
    """v0.1.0-rc.1 -> v0.1.0; v0.1.0 -> v0.1.0."""
    return version.split("-", 1)[0]


def sections(text: str) -> dict[str, str]:
    """Map each `## <version>` heading to the body under it."""
    found = {}
    matches = list(HEADING.finditer(text))
    for i, match in enumerate(matches):
        end = matches[i + 1].start() if i + 1 < len(matches) else len(text)
        found[match.group("version")] = text[match.end() : end].strip("\n")
    return found


def notes(text: str, version: str) -> str:
    """The notes for version, or raise ValueError naming what is missing."""
    found = sections(text)
    for candidate in (version, base_version(version)):
        body = found.get(candidate)
        if body is None:
            continue
        if not body.strip():
            raise ValueError(f"CHANGELOG.md section for {candidate} is empty")
        if candidate != version:
            body = f"Release candidate for {candidate}.\n\n{body}"
        return body.strip() + "\n"
    raise ValueError(
        f"CHANGELOG.md has no section for {version} "
        f"(looked for '## {version}' and '## {base_version(version)}'); "
        "add one before tagging — the release notes come from there"
    )


def selftest() -> int:
    changelog = """# Changelog

## Unreleased

- nothing yet

## v0.2.0 — 2026-10-01

- second

## v0.1.0 — 2026-09-03

- first
- also first
"""
    cases = [
        ("v0.2.0", "- second\n"),
        ("v0.1.0", "- first\n- also first\n"),
        ("v0.1.0-rc.1", "Release candidate for v0.1.0.\n\n- first\n- also first\n"),
    ]
    for version, want in cases:
        got = notes(changelog, version)
        assert got == want, f"{version}: got {got!r}, want {want!r}"
    # An unknown version fails rather than producing empty notes.
    for missing in ("v9.9.9", "v9.9.9-rc.1"):
        try:
            notes(changelog, missing)
        except ValueError:
            pass
        else:  # pragma: no cover - the assertion is the test
            print(f"{missing} should have failed", file=sys.stderr)
            return 1
    # An empty section is a missing section: a release with a heading and no
    # body would ship notes that say nothing.
    try:
        notes("## v0.1.0\n\n## v0.0.9\n\n- x\n", "v0.1.0")
    except ValueError:
        pass
    else:  # pragma: no cover
        print("an empty section should have failed", file=sys.stderr)
        return 1
    # The heading may carry a date or anything else after the version.
    assert "second" in notes(changelog, "v0.2.0")
    print("release-notes self-test: extraction, pre-release fallback, and both refusals hold")
    return 0


def main(argv: list[str]) -> int:
    if argv[:1] == ["--selftest"]:
        return selftest()
    if not argv:
        print(__doc__, file=sys.stderr)
        return 2
    version = argv[0]
    path = Path(argv[1]) if len(argv) > 1 else Path(__file__).resolve().parents[1] / "CHANGELOG.md"
    try:
        sys.stdout.write(notes(path.read_text(), version))
    except ValueError as problem:
        print(problem, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
