# W32 — the release agent: implementation notes

Temporary. Folded into the PR body (FINDINGS + delta material) at the end.

## Confirmed facts (measured this lane, 2026-09-03)

1. **The ADO remote MCP server is NOT the P10 shape D38(a) assumed.**
   `POST https://mcp.dev.azure.com/` answers `401` with
   `www-authenticate: Bearer resource_metadata="https://mcp.dev.azure.com/.well-known/oauth-protected-resource/"`.
   That document declares:
   ```json
   {"resource":"https://mcp.dev.azure.com",
    "authorization_servers":["https://login.microsoftonline.com/organizations/v2.0"],
    "bearer_methods_supported":["header"],
    "scopes_supported":["https://mcp.dev.azure.com/.default"]}
   ```
   So: streamable HTTP yes, PAT no — Microsoft Entra ID only. Microsoft's own
   docs say Claude Desktop and Codex cannot use it at all, and Claude Code and
   Cursor need a custom Entra app registration (the ADO MCP enterprise app is
   `<the ADO MCP enterprise application id>`).

2. **The local `@azure-devops/mcp` is stdio-only**, so the gateway cannot relay
   to it without a shim. Read at v2.9.0: `src/index.ts:122` constructs
   `StdioServerTransport` and there is no other transport in the package.
   It does accept a PAT (`--authentication pat`, `PERSONAL_ACCESS_TOKEN` =
   base64 `email:pat`) or a bearer (`--authentication envvar`,
   `ADO_MCP_AUTH_TOKEN`) — `src/auth.ts:146-168`.

   Consequence: "configure it, do not build one" and "reach it through the P10
   hardened dialer" cannot both hold. Ruled by the user: reach the REMOTE
   endpoint with an Entra bearer in plane custody. `bearer_methods_supported:
   ["header"]` is what makes that possible at all — the gateway's existing
   `credential_file` injection puts the token in exactly that header.

3. **The hosted endpoint takes tool-narrowing headers.** `X-MCP-Toolsets`,
   `X-MCP-Tools`, `X-MCP-Readonly`. The upstream table already carries
   `extra_headers` (Copilot uses it), so the ADO server can be made to expose
   only the pipelines toolset — narrowing at the SERVER, before the gateway's
   allowlist ever runs.

4. **The repository is `Azure/aks-desktop`** — public, releases roughly
   monthly, tags `vX.Y.Z` with `rc-vX.Y.Z` pre-releases, `target_commitish`
   `main`, notes are short hand-written bullets plus GitHub's generated
   "Full Changelog" line. The user's access there is `push`, not `admin`.

5. **Precedent that does the waiting.** `scripts/ap-await-approval.sh` already
   implements "print the line, wait for a named human to type it in Slack,
   fail closed if the decision never comes, comes from someone else, or is a
   denial" — including the three checks that matter (decided != approved;
   approved != approved by that person; approved by that person != approved
   FOR THIS CALL). `scripts/ap-demo.sh` is the multi-step driver shape.

## Rulings taken this lane

### R1 — the operator does not drive; the driver drives

User, verbatim: *"The entire point of this is for me to not run commands
myself, otherwise what is the point of the agent?"* and *"if I have to drive
this is a waste of time"*.

So the lane does NOT ship four `make` verbs the human sequences by hand. It
ships one driver that runs the whole release, does its own waiting, and
interrupts a human exactly once — at the approval, which arrives in Slack
(P8b) and is answered with `@kaimahi approve <id>`.

The one thing that stays human is capturing a credential, and that is by
design, not by omission: every secret in this repo is stdin-only and
human-driven (D27 — kmx accepts no credential material).

### R2 — long-running builds: the driver polls, the agent never blocks

Each agent turn is one short step, well inside the 5-minute invoke budget.
The WAITING is the driver's, in shell, not the model's.

Rejected, with reasons:
- **The P7b inbound bridge for build-completion callbacks.** It needs a
  public HTTPS edge — a `type: LoadBalancer` Service, an Azure DNS label and
  ACME (`k8s/inbound-edge.yaml`, `scripts/inbound-expose.sh` calls
  `az aks show`) — which exists on AKS only. A weekly personal release
  process will not stand that up. It also buys latency, not capability.
- **A polling loop inside one agent turn.** A model re-deciding "has it
  finished yet" every 30 seconds burns budget on a question `pipelines_build
  get_status` answers exactly, and it puts an unbounded wait inside a
  request/response turn.
- **Generic-hook callbacks** additionally: on `bearer`/`kaimahi-hmac` hooks
  the webhook body becomes the agent's prompt VERBATIM
  (`plane/internal/inbound/verify.go:170-182`) — an ADO build payload is not
  something to paste into a prompt.

### R3 — what the approval binds, and why those fields

An approval must name the ARTIFACT, not the verb. Bound (declared
`policy_fields`), for each consequential call:

- creating the release branch: `owner`, `repo`, `branch`, `from_branch`
  — approving "cut release/v1.2.3 from main on this repo" must not authorize
  cutting it from an arbitrary commit, nor on another repo.
- publishing the release: `owner`, `repo`, `tag_name`, `target_commitish`,
  `draft`, `prerelease` — approving a DRAFT must not authorize publishing
  live, and approving a prerelease must not authorize a stable one.
- running the pipeline: the dispatcher's `action` FIRST, then the project,
  the pipeline id and the ref.

NOT bound: `name` and `body` — the release title and notes. They are prose
the model regenerates, and an LLM re-emitting semantically identical prose is
not byte-stable; binding them would make "approve, then it proceeds" fail
nondeterministically, which is precisely the failure D29 anticipated. Stated
honestly: the human approves *which release, on which repo, at which
commit, draft or not*, and reads the notes in the proposal step — an agent
that got approval on v1.2.3 could publish different prose under it. The blast
radius of that is editable text; the blast radius of the alternative is an
approval flow that fails at random.

**Finding worth its own line:** the ADO tools are CONSOLIDATED DISPATCHERS —
one tool `pipelines_write` with an `action` of `run_pipeline`,
`create_pipeline` or `update_build_stage`. Verb-level policy is therefore
meaningless against them: allowlisting `pipelines_write` allows creating
pipelines and cancelling builds too. P12's argument binding is not a nicety
here, it is the only thing that makes the tool governable at all. This is the
strongest evidence to date for D29 and it came from a server we did not write.

## Open / to verify

- [ ] Whether `az account get-access-token --scope https://mcp.dev.azure.com/.default`
      yields a token the endpoint accepts. Blocked in-session by the sandbox
      classifier; asked of the user.
- [ ] GitHub hosted MCP: exact tool names + argument field names for
      create-branch / create-release / list-merged-PRs / compare.
- [ ] Whether a GitHub fine-grained token can create refs without also being
      able to delete them (expected: no — a finding if so).

## Findings that landed after the notes above were written

### F1 — GitHub's MCP server cannot create a release, or a tag

Read at `github/github-mcp-server` commit `9205304`, tags `v1.12.0` /
`latest-release`. The complete registry is `AllTools()`
(`pkg/github/tools.go:216-386`). Its release and tag tools are
`list_releases`, `get_latest_release`, `get_release_by_tag`, `list_tags`,
`get_tag` — **all read-only**. Grep for `create_release`, `create_tag`,
`create_ref`, `update_ref`, `delete_ref`, `publish_release`,
`upload_release` across the repo returns zero hits. `create_branch`
(`pkg/github/repositories.go:1499`) is the only ref-creating tool, and it
calls `Git.CreateRef` — create only, never force-update.

So "publish the binaries to a GitHub release" cannot be done by the agent
through this server. That turns out to be the RIGHT answer rather than a
gap, and it is D38(b) restated by the tool surface itself: the agent
dispatches the workflow that publishes, and CI moves the bytes. The
in-band path is `actions_run_trigger` with `method: "run_workflow"`.

`Azure/aks-desktop` fits this exactly: `.github/workflows/build-app-{win,
mac,linux}.yml` carry `workflow_dispatch`, and `1es-pipeline*.yml` are
Azure DevOps pipeline definitions (`trigger: none`, extending the 1ES
templates) that ADO runs — the "several Azure DevOps builds".

### F2 — no compare tool either

`GET /repos/{o}/{r}/compare/{basehead}` is not wrapped. Collating notes
therefore goes: `get_latest_release` (or `list_tags`) for the previous
tag, then `search_pull_requests` with
`repo:<o>/<r> is:merged base:main merged:>=<date>`, or `list_commits`
with `sha` + `since`. `list_pull_requests` has no `merged` state and no
date filter — `state: "closed"` returns unmerged PRs too, so merged-ness
has to be read per PR from `merged_at`.

### F3 — the ADO ref a build runs on CANNOT be bound by an approval

Exact argument names, read from `@azure-devops/mcp` v2.9.0
`src/tools/pipelines.dto.ts:56-102`. `pipelines_write` composes four
actions into one shape; `run_pipeline` takes `project`, `pipelineId`,
`pipelineVersion`, `previewRun`, `resources`, `stagesToSkip`,
`templateParameters`, `variables`, `yamlOverride`.

The branch a run builds is **not** a top-level field. It lives at
`resources.repositories.<name>.refName` (`pipelines.dto.ts:37-47`).
Kaimahi's policy vocabulary is top-level argument names only —
`plane/internal/config/policy.go` says so and enforces it with
`policyField = ^[A-Za-z0-9_-]{1,64}$`. `templateParameters` and
`variables` are nested records for the same reason.

Consequence, stated rather than papered over: an approval for an ADO
build binds *which pipeline in which project*, and cannot bind *which
ref it builds*. That is a real hole and the first one this project has
found by pointing the plane at a server it did not write. What would
close it is dotted-path policy fields (`resources.repositories.self.refName`),
which is a plane change and out of this lane's scope.

### F4 — consolidated dispatchers make verb-level policy meaningless

Both servers do it. `pipelines_write` is one tool whose `action` selects
`run_pipeline`, `create_pipeline`, `rename_pipeline` or
`update_build_stage` (`pipelines.ts:525-545`). So allowlisting
"pipelines_write" allows creating pipelines and cancelling in-flight
builds. `action` MUST be a bound policy field, and must be listed first
so the audit summary reads as the verb it actually is.

The same shape on the GitHub side: `actions_run_trigger` has
`method` ∈ {`run_workflow`, `rerun_workflow_run`, `rerun_failed_jobs`,
`cancel_workflow_run`, `delete_workflow_run_logs`} — an
arbitrary-workflow-execution primitive. `method`, `workflow_id` and `ref`
all get bound.

This is the strongest evidence to date for D29's ruling, and it came
from outside.

### F5 — the GitHub token cannot be scoped below "can delete refs"

Confirmed against GitHub's fine-grained permission reference. Creating a
branch (`POST /git/refs`), creating a release (`POST /releases`) and
DELETING a ref (`DELETE /git/refs/{ref}`) all require the same
permission: **Contents: write**. There is no separate Releases
permission, and no way to grant ref creation without ref deletion.

So the token cannot be the guarantee against a destructive git
operation. The gateway is. Written up honestly in the PR under the
guardrail question rather than claimed away.

### F6 — no new NetworkPolicy is needed

`k8s/egress-hosted.yaml` is already "TCP 443 to any public address, for
the proxy pod, minus the private ranges" — not a per-host rule
(NetworkPolicy has no hostname selector, and the file says so). It
therefore covers `mcp.dev.azure.com` unchanged. The coupling to note:
`make github-revoke` deletes that shared allowance, so revoking GitHub
also closes ADO's route out. The lane adds `make release-revoke` which
removes both tokens and the allowance together.

## Findings from building it (the FINDINGS section's raw material)

### G1 — the tool seam had no way to narrow a server it does not own

The LLM seam has had `extra_headers` since P4a. The tool seam had none, so
there was no way to tell a hosted MCP server "offer only the pipelines
toolset". Added in this lane, with two rules the LLM seam does not have:
a header naming the credential slot is refused at LOAD, and the
credential is injected last. (The LLM seam applies extra headers AFTER
the credential and could therefore overwrite it — noticed here, not
changed here, because that seam's only extra headers are Copilot's two
and changing the ordering is a separate decision.)

### G2 — P15 merged mid-lane, and it was the right home for the binding

The repository binding was patching the committed ConfigMap in place,
with a documented "re-run after `make plane`". P15's overlay landed and
made that unnecessary. Also forced a decision this lane owed: `extra_headers`
is DENIED to an overlay.

Cost of the mid-lane merge: a rebase, one moved test, and one CI failure
(`TestEveryToolUpstreamFieldIsClassifiedAsSafeOrDenied`) that was
*exactly* the guard doing its job — a field added to `ToolUpstream` in a
branch that never saw the overlay was refused by a test written to catch
that. Worth recording as a thing that worked.

### G3 — verifying against a stale cluster nearly produced a false pass

The one-repository constraint appeared not to be enforced. It was: the
cluster was running the pre-rebase proxy, which has no overlay mount.
`make plane` from the rebased tree fixed it. The lesson is small and
sharp: verify against the tree you are shipping, and a pod that predates
your change will lie to you convincingly.

### G4 — `plane-admin.sh issue` needs the `kagent` namespace to exist

A plane-only bring-up (no `make up`) fails at the Secret write with
`namespaces "kagent" not found`, AFTER the credential row is created —
so the retry then reports "exists in the plane but Secret is missing" and
tells you to delete the row. Two commands where one would do. Minor, and
exactly the kind of thing a first-time user hits.

### G5 — the driver's intent check earned its place immediately

The stub that proposes a different branch than the one asked for was
refused, with the pending list printed and nothing approved. That check
is ~15 lines and it is the difference between "a human approved a call"
and "a human approved THE call".
