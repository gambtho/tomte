# W35 — D42 Option A: a blueprint, plus one generic driver

Lane branch `w35/workflow-blueprint`. Worktree `.claude/worktrees/w35-blueprint`.

## Confirmed facts (read, not assumed)

- **W32's intent is spread over four layers**: `k8s/release-agent.yaml`
  (systemMessage + `toolNames`), `k8s/plane/upstreams.yaml`
  (`tool_upstreams`, `extra_headers`, `policy_fields`),
  `make release-allow` (the credential allowlist) + `scripts/release-bind.sh`
  (standing constraints as a P15 overlay fragment), and
  `scripts/release-run.sh` (556 lines of orchestration).
- **The equivalence target is exactly three artifacts**, produced by
  `make govern-release` + `make release-allow` + `make release-bind`:
  1. the credential allowlist for `release-agent` = `RELEASE_TOOLS`
     (Makefile:212) — 12 read tools;
  2. the overlay ConfigMap fragment `release-bind.json` =
     `{"standing_constraints": {"release-agent": {...}}}` — 8 read tools
     bound to owner/repo, plus `pipelines_write` bound to
     action/orgName/project/pipelineId when ADO_* are given;
  3. nothing else. The two hosted upstream ENTRIES and their
     `policy_fields` are COMMITTED (`k8s/plane/upstreams.yaml`), applied by
     `make plane`, and are not produced by those three targets.
- **An overlay may not declare a hosted or keyed upstream.**
  `plane/internal/config/overlay.go:74` denies `credential_file`,
  `credential_header`, `internet`, `ca_file` and `extra_headers` on an
  overlay `tool_upstreams` entry, with an exfiltration argument. So a
  blueprint CANNOT create the `github-release`/`ado` seams; it can only
  NAME them and ASSERT their declarations. This is the single biggest
  constraint on milestone 1 and it is a security property, not an
  oversight — it is not to be relaxed.
- **`kmx tools add` only onboards IN-CLUSTER servers**
  (`internal/kmx/app/toolsadd.go:229` refuses anything else), for the same
  reason.
- `k8s/plane/upstreams.yaml` IS embedded in the kmx binary (`embed.go`);
  `k8s/release-agent.yaml` and `k8s/kaimahi-release-*.yaml` are NOT.
- The plane already exposes what a blueprint needs to check itself:
  `POST /admin/config/validate` returns `declared` (tool -> policy fields,
  in declared order) and `already_allowlisted`
  (`internal/kmx/app/toolsadd.go:420`).
- Constraint vocabulary: `eq ne lt lte gt gte in not_in`, literals are
  string or int64 (`plane/internal/config/policy.go:39`).
