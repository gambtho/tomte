package app

// The driver's safety properties, tested where they can be tested
// without a cluster. The rest — that a denial is really a denial, that a
// grant is really welded to a digest — is the plane's, and CI exercises
// it against the synthetic hosted upstream with no credential anywhere
// (D14).

import (
	"strings"
	"testing"

	kaimahi "github.com/kaimahi-agents/kaimahi"
	"github.com/kaimahi-agents/kaimahi/internal/kmx/blueprint"
)

// TestTheDriverWaitsForTheRequestItFiledAndNotAnotherOne is the P13
// property at the selector.
//
// The driver files a request for the call the operator's parameters
// name. If the agent's own turn had filed a request too — for a
// different branch, at the same tool — the two would sit side by side in
// `kmx approvals`. What tells them apart is the summary, which names
// every policy-relevant field. A selector that matched on tool name, or
// on position, would hand a human the wrong one.
func TestTheDriverWaitsForTheRequestItFiledAndNotAnotherOne(t *testing.T) {
	mine := "create_branch: owner Contoso, repo widget, branch release/v1.2.3, from_branch main"
	theirs := "create_branch: owner Contoso, repo widget, branch release/v9.9.9, from_branch main"
	requests := []map[string]any{
		// The agent's, filed first, and it must not be chosen.
		{"id": "aaaa", "credential": "release-agent", "kind": "tool", "subject": "create_branch", "arg_summary": theirs},
		{"id": "bbbb", "credential": "release-agent", "kind": "tool", "subject": "create_branch", "arg_summary": mine},
	}
	if got := selectRequest(requests, "release-agent", "create_branch", mine); got != "bbbb" {
		t.Fatalf("selected %q; the request naming this exact call is bbbb", got)
	}
	// And when only the other one is there, nothing is selected. Picking
	// the "closest" would be picking a call nobody described.
	if got := selectRequest(requests[:1], "release-agent", "create_branch", mine); got != "" {
		t.Fatalf("selected %q for a call that was never filed", got)
	}
	// Another credential's request for the same call is not this one.
	other := []map[string]any{
		{"id": "cccc", "credential": "ap-agent", "kind": "tool", "subject": "create_branch", "arg_summary": mine},
	}
	if got := selectRequest(other, "release-agent", "create_branch", mine); got != "" {
		t.Fatalf("selected another credential's request: %q", got)
	}
}

// TestTheSummaryReadsTheDeclaredFieldsInTheDeclaredOrder keeps the
// driver's selector and the plane's audit line in step. The order is the
// table's, not the blueprint's, because the audit renders the declared
// order and that is what the driver has to match on.
func TestTheSummaryReadsTheDeclaredFieldsInTheDeclaredOrder(t *testing.T) {
	s := blueprint.RenderedStep{
		Tool:         "actions_run_trigger",
		PolicyFields: []string{"method", "owner", "repo", "workflow_id", "ref"},
		Args: map[string]string{
			"ref": "release/v1.2.3", "owner": "Contoso", "method": "run_workflow",
			"workflow_id": "build.yml", "repo": "widget",
		},
	}
	want := "actions_run_trigger: method run_workflow, owner Contoso, repo widget, workflow_id build.yml, ref release/v1.2.3"
	if got := s.Summary(); got != want {
		t.Fatalf("summary is\n %s\nwant\n %s", got, want)
	}
}

// TestAnIntegerArgumentIsSentAsAnInteger. The gateway canonicalises what
// it receives and the digest is over that, so a pipeline id filed as
// "41" and called as 41 are two different calls — and the approval a
// human gave for one would not admit the other.
func TestAnIntegerArgumentIsSentAsAnInteger(t *testing.T) {
	s := blueprint.RenderedStep{
		Tool:     "pipelines_write",
		Args:     map[string]string{"action": "run_pipeline", "pipelineId": "41"},
		ArgTypes: map[string]string{"pipelineId": "int"},
	}
	got, err := s.ArgsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"pipelineId":41`) {
		t.Fatalf("pipelineId was not sent as an integer: %s", got)
	}
	bad := blueprint.RenderedStep{
		Tool: "pipelines_write", Args: map[string]string{"pipelineId": "not-a-number"},
		ArgTypes: map[string]string{"pipelineId": "int"},
	}
	if _, err := bad.ArgsJSON(); err == nil {
		t.Fatal("a non-integer was accepted for an argument declared an integer")
	}
}

// TestTheDriverKeepsItsOwnAdminPort. The operator's `kmx approve` needs
// the default one while the driver is waiting for them; on the same port
// they collide and the operator meets "address already in use" on the
// one command they were just told to run.
func TestTheDriverKeepsItsOwnAdminPort(t *testing.T) {
	if DefaultWorkflowAdminPort == "19091" {
		t.Fatal("the driver's admin port is the default one; `kmx approve` would collide with a waiting run")
	}
}

// TestAResumedRunCannotUseACaptureItDidNotMake. `--step publish` skips
// the step that composes the notes, and a driver that silently passed
// the literal "${capture.notes.file}" to a publish script would create a
// release whose body is that string.
func TestAResumedRunCannotUseACaptureItDidNotMake(t *testing.T) {
	r := &workflowRun{captures: map[string]string{}, files: map[string]string{}}
	_, err := r.resolveCaptures("--notes ${capture.notes.file}")
	if err == nil || !strings.Contains(err.Error(), "no step in THIS run captured it") {
		t.Fatalf("an unresolved capture was passed through: %v", err)
	}
	r.captures["notes"] = "hello"
	r.files["notes"] = "/tmp/notes.txt"
	got, err := r.resolveCaptures("--notes ${capture.notes.file} says ${capture.notes}")
	if err != nil {
		t.Fatal(err)
	}
	if got != "--notes /tmp/notes.txt says hello" {
		t.Fatalf("resolved to %q", got)
	}
}

// TestAPollMarkerIsMatchedOnItsOwnLine. The prompt asks for the marker on
// a line of its own; a substring match anywhere would fire on the agent
// quoting the instruction back at it, and report a running build done.
func TestAPollMarkerIsMatchedOnItsOwnLine(t *testing.T) {
	if hasMarker("I will end with STATE: DONE when it finishes.\nStill building.", "STATE: DONE") {
		t.Fatal("the agent quoting the instruction was read as a finished build")
	}
	if !hasMarker("Two runs succeeded.\nSTATE: DONE\n", "STATE: DONE") {
		t.Fatal("a marker on its own line was not recognised")
	}
}

// TestTheCarriedBlueprintsAllParse. A blueprint that ships broken is a
// command that fails on somebody's first use of it, and `go test ./...`
// is where that should be found.
func TestTheCarriedBlueprintsAllParse(t *testing.T) {
	bundles, err := blueprint.Carried(kaimahi.Blueprints)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) == 0 {
		t.Fatal("kmx carries no blueprints; the test proves nothing")
	}
	for _, b := range bundles {
		if len(b.Scripts()) == 0 {
			continue
		}
		for _, name := range b.Scripts() {
			body, err := b.Script(name)
			if err != nil {
				t.Fatalf("blueprint %q: %v", b.Name, err)
			}
			if len(body) == 0 {
				t.Fatalf("blueprint %q carries an empty %s", b.Name, name)
			}
		}
	}
}
