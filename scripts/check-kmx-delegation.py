#!/usr/bin/env python3
"""Fail when a Makefile recipe re-implements what kmx owns.

D27's first condition is that there is ONE implementation of the developer
journey: kmx implements it, and the Makefile's `up`, `cluster`, `ollama`,
`model`, `kagent`, `agent`, `tools-agent`, `chat`, `status` and `down` are
thin aliases that call it. The failure this guards against is not a missing
alias — that would be obvious — but the slow kind: someone fixes a wait or
adds a flag in the Makefile because that is where they were looking, and the
two implementations drift while both stay green.

The check asks make itself rather than reading the file, so it sees the
recipe after every conditional and variable expansion — the same lines a
developer's invocation would run. `make -n` runs nothing.

The rule for an owned target on the kind path: the recipe may build kmx,
may fetch the pinned kagent CLI, and must otherwise reach the cluster ONLY
through kmx. A bare kubectl, helm or kind command in one of these recipes is
a second implementation.

The managed path (TARGET=aks) is deliberately NOT checked: milestone 1 does
not cover AKS, so those recipes are still the Makefile's own.

Run:  python3 scripts/check-kmx-delegation.py [--selftest]
"""
from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

# Every target kmx owns, with the command it must delegate to.
OWNED = {
    "up": "kmx up",
    "cluster": "kmx up --step cluster",
    "ollama": "kmx up --step ollama",
    "model": "kmx up --step model",
    "kagent": "kmx up --step kagent",
    "agent": "kmx up --step agent",
    "tools-agent": "kmx up --step tools-agent",
    "chat": "kmx agent chat",
    "status": "kmx status",
    "down": "kmx down",
}

# A line that reaches the cluster itself. Anchored to a command position —
# the start of a line, or after a shell operator — so that `KIND_CLUSTER=x`
# in an environment prefix and `--kube-context` in an argument do not match.
CLUSTER_TOOL = re.compile(r"(?:^|[;&|(]\s*|^\s*)(kubectl|helm|kind)\s", re.M)

# Lines that legitimately appear in an owned recipe.
ALLOWED = (
    re.compile(r"\bgo build\b"),
    re.compile(r"\bcommand -v go\b"),
    # The pinned kagent CLI fetch: shared with slack-post and github-ask,
    # which are still make's, and handed to kmx as KAGENT=bin/kagent so a
    # checkout keeps one binary.
    re.compile(r"kagent-(dev|\$\(OS\)|linux|darwin)"),
    re.compile(r"^\s*(curl|chmod|mkdir|test|sum=|echo|rm -f bin/kagent)"),
)


def offending_lines(recipe: str) -> list[str]:
    """Lines in an owned recipe that reach the cluster without kmx."""
    bad = []
    for line in recipe.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if "kmx" in line:
            continue
        if any(pattern.search(line) for pattern in ALLOWED):
            continue
        if CLUSTER_TOOL.search(line):
            bad.append(line.strip())
    return bad


def dry_run(target: str) -> str:
    result = subprocess.run(
        ["make", "-n", target, "TARGET=kind"],
        capture_output=True,
        text=True,
        cwd=Path(__file__).resolve().parents[1],
    )
    if result.returncode != 0:
        raise SystemExit(f"make -n {target} failed:\n{result.stdout}{result.stderr}")
    return result.stdout


def check() -> int:
    problems = []
    for target, expected in OWNED.items():
        recipe = dry_run(target)
        if expected not in recipe.replace("bin/kmx", "kmx"):
            problems.append(f"{target}: does not delegate — expected `{expected}`")
        for line in offending_lines(recipe):
            problems.append(f"{target}: reaches the cluster without kmx: {line}")
    if problems:
        print("kmx delegation:", *problems, sep="\n  ", file=sys.stderr)
        print("\nThese targets are kmx's. Change cmd/kmx and internal/kmx, not the recipe.", file=sys.stderr)
        return 1
    print(f"kmx delegation: {len(OWNED)} targets delegate, none re-implement the journey")
    return 0


SELFTEST = [
    ("a delegating recipe", "KIND_CLUSTER='x' bin/kmx up --step agent", []),
    ("the kmx build", "go build -o bin/kmx ./cmd/kmx", []),
    ("the kagent fetch", "curl -sSfLo bin/kagent https://example/kagent-linux-amd64", []),
    ("a comment", "# kubectl apply -f k8s/ollama.yaml", []),
    ("a re-implemented apply", "kubectl --context kind-x apply -f k8s/ollama.yaml",
     ["kubectl --context kind-x apply -f k8s/ollama.yaml"]),
    ("a re-implemented helm install", "\thelm upgrade --install kagent oci://...",
     ["helm upgrade --install kagent oci://..."]),
    ("a re-implemented cluster create", "kind create cluster --name x",
     ["kind create cluster --name x"]),
    # An environment prefix is not a command: KIND_CLUSTER= must not match.
    ("an environment prefix", "KIND_CLUSTER='x' KUBE_CTX='kind-x' bin/kmx down", []),
    # A kubectl hidden behind a shell operator is still a kubectl.
    ("a chained kubectl", "true; kubectl -n kagent get pods", ["true; kubectl -n kagent get pods"]),
]


def selftest() -> int:
    failed = 0
    for name, line, expected in SELFTEST:
        got = offending_lines(line)
        if got != expected:
            failed += 1
            print(f"FAIL [{name}] -> {got}, want {expected}")
        else:
            print(f"ok   [{name}]")
    if failed:
        print(f"kmx delegation self-test: {failed} case(s) failed", file=sys.stderr)
        return 1
    print("kmx delegation self-test: all cases passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(selftest() if "--selftest" in sys.argv else check())
