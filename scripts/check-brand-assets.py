#!/usr/bin/env python3
from __future__ import annotations

import struct
import sys
import xml.etree.ElementTree as ET
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

PNG_REQUIREMENTS = {
    "brand/mascot.png": (1536, 1536, True),
    "brand/mark.png": (1024, 1024, True),
    "brand/hero.png": (1600, 600, False),
    "brand/social-preview.png": (1280, 640, False),
}

SVG_REQUIREMENTS = {
    "brand/mark.svg": "Kaimahi compact mark",
    "brand/wordmark.svg": "Kaimahi wordmark",
    "docs/assets/architecture.svg": "Kaimahi governance architecture",
}


def png_metadata(path: Path) -> tuple[int, int, bool]:
    data = path.read_bytes()
    if data[:8] != b"\x89PNG\r\n\x1a\n" or data[12:16] != b"IHDR":
        raise ValueError("not a PNG with an IHDR header")
    width, height, _depth, color_type = struct.unpack(">IIBB", data[16:26])
    return width, height, color_type in {4, 6}


def validate_png(relative: str, expected: tuple[int, int, bool]) -> list[str]:
    path = ROOT / relative
    if not path.is_file():
        return [f"{relative}: missing"]
    try:
        actual = png_metadata(path)
    except (OSError, ValueError, struct.error) as error:
        return [f"{relative}: {error}"]
    width, height, must_have_alpha = expected
    problems = []
    if actual[:2] != (width, height):
        problems.append(f"{relative}: dimensions {actual[:2]} != {(width, height)}")
    if must_have_alpha and not actual[2]:
        problems.append(f"{relative}: alpha channel required")
    return problems


def validate_svg(relative: str, expected_title: str) -> list[str]:
    path = ROOT / relative
    if not path.is_file():
        return [f"{relative}: missing"]
    try:
        root = ET.parse(path).getroot()
    except (OSError, ET.ParseError) as error:
        return [f"{relative}: invalid XML: {error}"]
    problems = []
    if not root.tag.endswith("svg"):
        problems.append(f"{relative}: root element is not svg")
    if not root.attrib.get("viewBox"):
        problems.append(f"{relative}: viewBox required")
    titles = [node.text.strip() for node in root.iter() if node.tag.endswith("title") and node.text]
    if expected_title not in titles:
        problems.append(f"{relative}: title must contain exactly {expected_title!r}")
    return problems


def main() -> int:
    problems = []
    for relative, expected in PNG_REQUIREMENTS.items():
        problems.extend(validate_png(relative, expected))
    for relative, expected_title in SVG_REQUIREMENTS.items():
        problems.extend(validate_svg(relative, expected_title))
    brand_readme = ROOT / "brand/README.md"
    if not brand_readme.is_file():
        problems.append("brand/README.md: missing")
    if problems:
        print("brand asset validation failed:", file=sys.stderr)
        for problem in problems:
            print(f"- {problem}", file=sys.stderr)
        return 1
    print("brand assets: dimensions, alpha, and SVG metadata valid")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
