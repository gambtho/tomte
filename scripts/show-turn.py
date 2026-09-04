#!/usr/bin/env python3
"""Print one agent turn readably: the tool calls it made, then its reply.

`make chat` emits a whole A2A task object, which is unreadable when what
you want to know is "what did it do, and what did it say". This is the
rendering scripts/release-run.sh shows a person at each step.

  show-turn.py FILE [--calls-only|--reply-only]

Exit 1 if the file carries no task object, so a caller can tell "the turn
did not happen" from "the turn said nothing".
"""
import json
import re
import sys


def task(raw):
    m = re.search(r"^\{.*\}$", raw, re.M | re.S)
    if not m:
        return None
    try:
        return json.loads(m.group(0))
    except json.JSONDecodeError:
        return None


def calls(d):
    out = []
    for msg in d.get("history", []):
        for p in msg.get("parts", []):
            if (p.get("metadata") or {}).get("kagent_type") == "function_call":
                data = p.get("data", {})
                out.append((data.get("name", "?"), data.get("args", {})))
    return out


def reply(d):
    texts = [p.get("text", "") for a in d.get("artifacts", [])
             for p in a.get("parts", []) if p.get("text")]
    return "\n".join(t for t in texts if t.strip()).strip()


def main(argv):
    if len(argv) < 2:
        sys.exit("usage: show-turn.py FILE [--calls-only|--reply-only]")
    mode = argv[2] if len(argv) > 2 else ""
    d = task(open(argv[1], encoding="utf-8", errors="replace").read())
    if d is None:
        print("show-turn: no task object in the agent's output", file=sys.stderr)
        return 1
    if mode != "--reply-only":
        for name, args in calls(d):
            print("   tool call: %s %s" % (name, json.dumps(args, sort_keys=True)))
        print("   state: %s" % d.get("status", {}).get("state"))
    if mode != "--calls-only":
        text = reply(d)
        if text:
            print("\n" + text + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
