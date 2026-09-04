package blueprint_test

// The refusals. Each one is a way the four hand-written layers could
// disagree with each other, made into a parse error.
//
// These are the reason a blueprint is worth having at all: the layers
// W32 spread its intent over cannot check each other, and every case
// below is one that a human got right by remembering to.

import (
	"strings"
	"testing"

	"github.com/kaimahi-agents/kaimahi/internal/kmx/blueprint"
)

// base is a minimal well-formed blueprint the cases below vary.
const base = `
blueprint: v1
name: demo
summary: a demo workflow
credential: demo-agent
agent: demo-agent
parameters:
  repo:
    type: github_repo
    required: true
    help: the repository
seams:
  demo-server:
    requires:
      thing_read:  [owner, repo]
      thing_write: [owner, repo, amount]
    allow: [thing_read]
    bound:
      thing_read:
        - {field: owner, op: eq, value: "${repo.owner}"}
steps:
  - name: look
    kind: read
    prompt: Call thing_read with owner ${repo.owner} and repo ${repo.name}.
  - name: change
    kind: consequential
    call:
      upstream: demo-server
      tool: thing_write
      args: {owner: "${repo.owner}", repo: "${repo.name}", amount: "100"}
    prompt: Call thing_write.
`

func TestRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(string) string
		expect string
	}{{
		// THE P13 PROPERTY. A model that proposed a different call files
		// a request too, and it looks identical in `make approvals`.
		name: "a policy-bound argument may not come from an agent turn",
		edit: func(s string) string {
			s = strings.Replace(s, `  - name: look
    kind: read`, `  - name: look
    kind: propose
    capture: amount`, 1)
			return strings.Replace(s, `amount: "100"`, `amount: "${capture.amount}"`, 1)
		},
		expect: "never something an agent turn produced",
	}, {
		name: "a consequential tool may not also be allowlisted",
		edit: func(s string) string {
			return strings.Replace(s, "allow: [thing_read]", "allow: [thing_read, thing_write]", 1)
		},
		expect: "the approval this step performs would be theatre",
	}, {
		// A standing constraint ADMITS. A consequential step on a
		// bounded tool approves a call that was already permitted —
		// which is the disagreement between scripts/release-bind.sh and
		// scripts/release-run.sh, caught.
		name: "a consequential tool may not also carry a standing bound",
		edit: func(s string) string {
			return strings.Replace(s, `      thing_read:
        - {field: owner, op: eq, value: "${repo.owner}"}`,
				`      thing_read:
        - {field: owner, op: eq, value: "${repo.owner}"}
      thing_write:
        - {field: amount, op: lte, value: "100"}`, 1)
		},
		expect: "A standing constraint ADMITS",
	}, {
		name: "a bounded step with no bound is a call that is simply denied",
		edit: func(s string) string {
			return strings.Replace(s, "    kind: consequential", "    kind: bounded", 1)
		},
		expect: "no seam gives thing_write a standing bound",
	}, {
		name: "a constraint on an undeclared field is refused before the plane refuses the whole table",
		edit: func(s string) string {
			return strings.Replace(s, `{field: owner, op: eq, value: "${repo.owner}"}`,
				`{field: nonesuch, op: eq, value: "x"}`, 1)
		},
		expect: "does not declare \"nonesuch\" as policy-relevant",
	}, {
		name: "one tool allowlisted by two seams would admit it on both servers",
		edit: func(s string) string {
			return strings.Replace(s, "steps:", `  other-server:
    requires:
      thing_read: [owner, repo]
    allow: [thing_read]
steps:`, 1)
		},
		expect: "per-credential, not per-upstream",
	}, {
		// D27, applied to a file format.
		name: "a blueprint may not carry a credential-shaped key",
		edit: func(s string) string {
			return strings.Replace(s, "credential: demo-agent", "credential: demo-agent\ntoken: abc", 1)
		},
		expect: "kmx accepts no credential material",
	}, {
		name: "a blueprint may not carry something shaped like a token",
		edit: func(s string) string {
			// Assembled at run time, never written out. A literal here
			// would be a token-shaped string in the tree, and this
			// repository's own scanner cannot tell a fixture from the
			// real thing — which is the scanner working, not failing.
			fake := "github" + "_pat_" + strings.Repeat("A1b2C3d4", 4)
			return strings.Replace(s, "summary: a demo workflow", "summary: a demo workflow "+fake, 1)
		},
		expect: "shaped like a credential",
	}, {
		name: "a step that runs a command must mark the transfer ungoverned",
		edit: func(s string) string {
			return s + `    exec:
      command: [gh, release, create]
`
		},
		expect: "must say `ungoverned:`",
	}, {
		name: "a bundled script may not have a directory part",
		edit: func(s string) string {
			return s + `    exec:
      script: ../../etc/passwd
`
		},
		expect: "with no directory part",
	}, {
		name: "a poll step must be bounded",
		edit: func(s string) string {
			return s + `  - name: wait
    kind: poll
    prompt: how is it going
    poll:
      interval_seconds: 60
      timeout_seconds: 0
      done: "STATE: DONE"
      failed: "STATE: FAILED"
`
		},
		expect: "positive interval_seconds and timeout_seconds",
	}, {
		name:   "an unknown blueprint version is not interpreted optimistically",
		edit:   func(s string) string { return strings.Replace(s, "blueprint: v1", "blueprint: v2", 1) },
		expect: "this kmx understands",
	}, {
		name: "an unanchored pattern is not the check it looks like",
		edit: func(s string) string {
			return strings.Replace(s, "    help: the repository", "    help: the repository\n    pattern: '[a-z]+'", 1)
		},
		expect: "must be anchored",
	}, {
		name: "an unknown key is refused rather than ignored",
		edit: func(s string) string {
			return strings.Replace(s, "  - name: look", "  - name: look\n    kinds: read", 1)
		},
		expect: "field kinds not found",
	}, {
		name: "a step may not reference a capture no earlier step made",
		edit: func(s string) string {
			return strings.Replace(s, "prompt: Call thing_write.", "prompt: Use ${capture.notes}.", 1)
		},
		expect: "which no earlier step captured",
	}, {
		name: "a call argument may not read a list",
		edit: func(s string) string {
			s = strings.Replace(s, `  repo:`, `  amounts:
    type: int_list
    help: amounts
  repo:`, 1)
			return strings.Replace(s, `amount: "100"`, `amount: "${amounts}"`, 1)
		},
		expect: "One call takes one value",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := blueprint.Parse([]byte(tc.edit(base)))
			if err == nil {
				t.Fatalf("accepted a blueprint it must refuse (%s)", tc.expect)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("refused for the wrong reason.\n want: %s\n  got: %v", tc.expect, err)
			}
		})
	}
}

// TestTheBaseBlueprintIsAccepted keeps the table above honest: if the
// base document stopped parsing, every case would "pass" for the wrong
// reason.
func TestTheBaseBlueprintIsAccepted(t *testing.T) {
	if _, err := blueprint.Parse([]byte(base)); err != nil {
		t.Fatal(err)
	}
}

func TestParametersAreCheckedTogetherAndByShape(t *testing.T) {
	b, err := blueprint.Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Bind(map[string]string{}, nil); err == nil ||
		!strings.Contains(err.Error(), "--set repo=… is required") {
		t.Fatalf("a missing required parameter was not reported: %v", err)
	}
	if _, err := b.Bind(map[string]string{"repo": "not-a-repo"}, nil); err == nil ||
		!strings.Contains(err.Error(), "want owner/name") {
		t.Fatalf("a malformed github_repo was accepted: %v", err)
	}
	if _, err := b.Bind(map[string]string{"repo": "a/b", "nope": "x"}, nil); err == nil ||
		!strings.Contains(err.Error(), "is not a parameter of blueprint") {
		t.Fatalf("an unknown --set was accepted: %v", err)
	}
}

// TestTheReleaseBlueprintNeedsItsRunTimeParametersOnlyWhenTheStepRuns is
// the answer to "arguments known only at run time": the publish step's
// build ids are an operator parameter, demanded when that step runs and
// not before, so the value a human approves is one a human typed.
func TestTheReleaseBlueprintNeedsItsRunTimeParametersOnlyWhenTheStepRuns(t *testing.T) {
	b := loadRelease(t)
	set := map[string]string{"repo": "Contoso/widget", "version": "v1.2.3"}
	if _, err := b.Bind(set, []string{"propose", "compose", "cut"}); err != nil {
		t.Fatalf("a run that does not publish should not need the build ids: %v", err)
	}
	_, err := b.Bind(set, []string{"publish"})
	if err == nil || !strings.Contains(err.Error(), `--set ado_builds=… is required to run step "publish"`) {
		t.Fatalf("a publish run was allowed without the build ids: %v", err)
	}
}
