# Principles for the developer entry point

*Origin: [Sajay Antony's `kmx` proposal](https://gist.github.com/sajayantony/9c294f2f2f650fecd49dae1c2348d1c2),
relayed from leadership, accepted as P11 by D27 and shaped further by D28.
This document is not the proposal — the command set is settled and being
built. It is the reasoning underneath it: why an entry point exists at all,
what it must never become, and how we will know it is working.*

---

## The problem is not that Kubernetes is hard

It is that the distance between *wanting an agent* and *having one* is
measured in things you must already know.

Today that distance is a clone, a prerequisite list, and a Makefile. The
Makefile is good — it is the journey in glue form, and every step of it is
proven by CI on every pull request. But it asks for something before it
gives anything: `git clone`, then Docker or Podman, kind, kubectl, Helm,
make, curl, then `make up KIND_CLUSTER=<your-name>`, then a five-to-ten
minute wait during which nothing tells you whether it is working.

A developer who abandons at minute three has not rejected the idea. They
never got to it.

The entry point exists to close that distance. Not to make Kubernetes
easier — kagent already does the hard part — but to make the *first
successful agent* a matter of minutes and a few commands, on a machine
that had none of this yesterday.

## What it is

One binary. `kmx`, a single static Go program that shells out to kind,
kubectl, Helm and the kagent CLI exactly as the Makefile does, and does
nothing those tools already do.

That last clause is the load-bearing one, and it is why this is a set of
principles rather than a feature list.

## Six principles

### 1. Delegate everything that already exists

kagent ships `init`, `install`, `deploy`, `get`, `invoke`, `add-mcp`,
`run`, `build`, and a dashboard. `kmx agent chat` is a passthrough to
`kagent invoke`. It is not a reimplementation, a wrapper with extra
opinions, or a "better" invoke.

This is not politeness. Rebuilding what an upstream already ships is the
mistake that caused this project's only full restart, and it is written
into the prime directive as a result. A second implementation of someone
else's control surface is a maintenance liability that grows every time
they release.

The test: for any command, can you name what it does that the underlying
tool does not? If not, it should not exist — and when a developer asks for
it, the right answer is to print the command that already works.

### 2. The generated YAML is the product, not a by-product

`kmx agent create` writes `agents/<name>.yaml` and applies it. The file is
the point. It is the same YAML a developer would have written by hand, it
is theirs from that moment, and it is reviewable, diffable, committable
and portable to any conformant cluster.

An entry point that hides the artifact has built a black box with a nice
front door. The moment the tool disappears — because it is unmaintained,
or unavailable, or simply wrong for a case — the developer must still have
something that works. What they keep is the YAML.

This also sets the ceiling on cleverness: if a feature cannot be expressed
in the artifact, the artifact is the thing to fix.

### 3. Show where you are about to act, and fail closed when unsure

Every mutation names its target before it happens, and anything that is
not a local development cluster requires an explicit yes.

The repo already learned this the expensive way. An earlier scaffolder
decided "is this safe?" by matching `kind-` against the context *name* —
which is cosmetic, and sails straight through for a context called
`kind-prod` pointing at production. The replacement checks the API server
address as well, and refuses rather than guesses when it cannot tell.

The principle generalises: the dangerous state is not "wrong answer", it
is "confident wrong answer". Where the tool cannot establish the truth, it
stops.

### 4. Never hold a credential

The entry point does not accept an API key as a flag, an environment
variable, or a file path. Generated manifests carry Secret *references*,
never Secret values. Credential capture stays in dedicated scripts that
read from stdin and write to a Kubernetes Secret.

A scaffolder is exactly where a key wants to be typed, and exactly where
it must not be. Anything that can take a key can leak one into a file the
developer is about to commit.

### 5. Work with the cluster you have, or give you a disposable one

`kmx ctx <context>` selects and validates an existing cluster.
`kmx up` creates a local kind environment and installs the dependencies.
Both are first-class.

Assuming the disposable cluster patronises the platform engineer who
already has one. Assuming the existing cluster abandons the newcomer who
does not. Neither audience is the "real" one.

### 6. One implementation, not two that drift

`kmx` implements the journey; the Makefile's `up`, `cluster`, `agent`,
`chat`, `status` and `down` become thin aliases that call it (D27).

Two implementations of the same journey diverge — quietly, and usually in
the direction of the one CI does not exercise. Delegation means CI keeps
proving the code a developer actually runs, rather than a parallel path
that merely resembles it.

## What it must never become

- **A second control plane.** Governance lives in the plane and at
  kagent's seams. The entry point points agents at it; it does not
  enforce, meter or approve anything itself.
- **A `kubectl` with opinions.** Read, update and delete already exist and
  are better than anything shipped here would be. Scaffolding is the gap.
- **A place secrets live.** See principle 4.
- **A dependency you cannot remove.** The YAML keeps working when `kmx`
  does not.
- **A publishing commitment made by accident.** Distribution is a naming
  decision, not a convenience decision — see below.

## The uncomfortable part: the name is not ours yet

The original proposal opens with `brew install kaimahi-agents/tap/kmx`.
That line cannot ship, and the reason is worth stating plainly rather than
quietly dropping.

Publishing a package or a tap is an outward-facing claim on a name. As of
D27: PyPI `kmx` and the GitHub user `kmx` are already taken; npm, crates
and the Homebrew formula are free; and **KMX is CarMax's ticker symbol**,
which is now in the trademark counsel brief alongside the open questions
about *kaimahi* itself — a te reo Māori word still awaiting a cultural
appropriateness read.

So installation is `go install …/cmd/kmx@<sha>` until those gates clear.
That is less convenient, and it is the honest position. A tap created for
developer convenience is still a public claim, and reversing one is much
harder than delaying it.

## How we will know it is working

Not by command count. By these:

| Question | What a good answer looks like |
|---|---|
| How long from nothing to a first reply? | Minutes, on a machine that had none of this yesterday |
| What does the developer keep? | A YAML file they understand and can apply without the tool |
| What happens on an unfamiliar cluster? | It names the target and waits for a yes |
| What happens when it does not know? | It stops, and says what it could not determine |
| How much of it is ours? | As little as possible — the rest delegated |
| Would removing it break anything? | No. The artifacts and the Makefile still work |

The first successful agent is the metric. Everything else is a means.

## What is deliberately not here yet

The entry point is being built in slices, and the slices are chosen so that
each one is useful alone. What is missing is missing on purpose.

**Milestone 1 (in flight).** `ctx`, `up`, `agent create`, `agent chat`,
`status`, `down` — the runtime journey only: kind, kagent, Ollama, the
agents. No governance plane. A consequence was stated and accepted rather
than discovered: on a fresh cluster `agent create` scaffolds the keyless
preset and prints the ungoverned warning, because governed presets only
exist once the plane does.

**Milestone 2 (shaped, D28).** `plane`, `govern <name>`, and the read-only
views — `ledger`, `grants`, audit reads. Clone-free: the binary carries
the manifests and fetches the plane at its own revision. kind only, which
keeps it entirely keyless.

**Milestone 3 and beyond (not scheduled).** Budget, approvals,
backup/restore, and the connector families stay in `make` and `scripts`
for now. So does the AKS path — `kmx` is a local-development entry point
until there is a reason it is not.

**Publishing.** A Homebrew tap, or any package, waits on the naming gates.
Nothing about the design blocks it; the decision is not a technical one.

The ordering principle is worth stating because it is the part most likely
to be argued with: **capabilities become commands only after the quick
start is simple, reliable, and useful on its own.** A command added early
is a command that must keep working while the foundation under it is still
moving. It is easier to add the tenth command than to remove the third.

### Known rough edges

These are recorded because they will be met by whoever works on this next,
and finding them a second time is wasted effort:

- `go:embed` cannot cross into `plane/` while it carries its own
  `go.mod`, so the entry point can never carry the plane's *source* — it
  fetches a built binary instead. The nested module that blocks the first
  is what makes the second work.
- A module-proxy build does not set `vcs.revision`, so a build-info
  version read that relies on it silently loses the revision on this path.
- `go install` refuses to cross-compile while `GOBIN` is set, which some
  version managers set by default. It will bite contributors on a Mac
  targeting Linux.
- Rendering and context-guarding already exist as shell scripts. Porting
  them is reasonable; ending up with two implementations of either is not
  (principle 6).

## Status

`kmx` is accepted (D27) and milestone 1 is in flight. Milestone 2 —
`govern` and the plane, clone-free — is shaped (D28). This document
describes the reasoning those decisions encode; the decisions themselves,
with their conditions and dissents, are in
[COORDINATION.md](COORDINATION.md).
