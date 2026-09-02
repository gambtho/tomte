#!/usr/bin/env python3
from pathlib import Path

readme = Path(__file__).resolve().parents[1] / "README.md"
text = readme.read_text()

# Required in this order: identity, the three governance outcomes,
# architecture, then the working quickstart before status.
order = [
    "brand/hero.png",
    "Governance for AI agents running on Kubernetes.",
    "### Control model spend",
    "### Constrain tool calls",
    "### Approve consequential actions",
    "docs/assets/architecture.svg",
    "## Quickstart",
    "make up",
    "make chat",
    "## Status",
]

missing = [item for item in order if item not in text]
if missing:
    raise SystemExit("README front door missing: " + ", ".join(missing))

positions = [text.index(item) for item in order]
if positions != sorted(positions):
    raise SystemExit("README front-door sections are out of order")

# The proposed CLI may not appear anywhere before the end of the working
# quickstart, including between `make up` and `make chat`.
proposed_cli = text.find("npx kaimahi create")
if proposed_cli != -1 and proposed_cli < text.index("make chat"):
    raise SystemExit("proposed CLI appears before the working quickstart")

print("README front door: identity, outcomes, architecture, quickstart, and status order valid")
