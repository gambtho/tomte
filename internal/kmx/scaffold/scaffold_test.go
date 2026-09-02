package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustGenerate(t *testing.T, spec Spec) string {
	t.Helper()
	doc, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return doc
}

func base(name string) Spec {
	return Spec{Name: name, ModelConfig: "hello-world-model"}
}

func TestGeneratesAnAgentWithTheExpectedShape(t *testing.T) {
	doc := mustGenerate(t, base("billing-investigator"))
	for _, want := range []string{
		"apiVersion: kagent.dev/v1alpha2",
		"kind: Agent",
		`  name: "billing-investigator"`,
		`  namespace: "kagent"`,
		"  type: Declarative",
		`    modelConfig: "hello-world-model"`,
		"    systemMessage: |",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("generated manifest is missing %q:\n%s", want, doc)
		}
	}
	// No tools were asked for, so none are wired: an agent that can call
	// nothing is the safe default.
	if strings.Contains(doc, "tools:") {
		t.Errorf("an agent with no --tools must have no tool wiring:\n%s", doc)
	}
}

// --- names -----------------------------------------------------------------

func TestNameValidation(t *testing.T) {
	for _, good := range []string{"a", "billing", "billing-investigator", "agent-7"} {
		if err := ValidateName(good); err != nil {
			t.Errorf("%q should be a valid name: %v", good, err)
		}
	}
	for _, bad := range []string{
		"", "Billing", "billing_investigator", "-billing", "billing-",
		"billing investigator", "billing/investigator", "билл",
		strings.Repeat("a", 64),
		// The two agents `kmx up` owns: scaffolding over one would replace a
		// committed artifact, and the next `kmx up` would replace it back.
		"hello-world", "hello-tools",
	} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

// --- the allowlist is mandatory --------------------------------------------

func TestToolAllowlistIsMandatory(t *testing.T) {
	// A server with no allowlist grants every tool it offers — today, and
	// after its next release. Refused, not warned.
	if _, err := ParseTools("kagent-tool-server"); err == nil {
		t.Fatal("--tools with no allowlist must be refused")
	}
	if _, err := ParseTools("kagent-tool-server:"); err == nil {
		t.Fatal("--tools with an empty allowlist must be refused")
	}
	wiring, err := ParseTools("kagent-tool-server:k8s_get_resources,k8s_get_events")
	if err != nil {
		t.Fatalf("a named allowlist must be accepted: %v", err)
	}
	if wiring.Server != "kagent-tool-server" || len(wiring.Tools) != 2 {
		t.Fatalf("parsed %+v", wiring)
	}
}

func TestToolNamesMustBeIdentifiers(t *testing.T) {
	// CWE-74, found in review on #16: a newline in a tool name closed the
	// YAML sequence and appended a tool nobody had reviewed. Two defences,
	// and this asserts the first — the name never gets that far.
	for _, bad := range []string{
		"server:k8s_get_resources\n            - k8s_delete",
		"server:tool one",
		"server:\"tool\"",
		"server:tool,,other",
		"ser ver:tool",
	} {
		if _, err := ParseTools(bad); err == nil {
			t.Errorf("--tools %q must be refused", bad)
		}
	}
}

func TestToolWiringIsQuotedOnEmission(t *testing.T) {
	spec := base("tools-user")
	wiring, err := ParseTools("kagent-tool-server:k8s_get_resources")
	if err != nil {
		t.Fatal(err)
	}
	spec.Tools = wiring
	doc := mustGenerate(t, spec)
	for _, want := range []string{
		`          name: "kagent-tool-server"`,
		`            - "k8s_get_resources"`,
		"          toolNames:",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing %q:\n%s", want, doc)
		}
	}
}

// --- never a credential ----------------------------------------------------

func TestKeyShapedContentIsRefused(t *testing.T) {
	// Assembled at runtime so this test file does not itself carry
	// something the repository's secret scanner would (correctly) flag.
	keys := []string{
		"sk-" + "ant-" + "api03-" + strings.Repeat("A", 32),
		"sk-" + "proj-" + strings.Repeat("B", 32),
		"kmh_" + strings.Repeat("c", 24),
		"ghp_" + strings.Repeat("d", 36),
		"github" + "_pat_" + strings.Repeat("e", 30),
		"xoxb-" + strings.Repeat("1", 12) + "-abcdef",
		"-----BEGIN RSA PRIVATE KEY-----",
		`api_key: "` + strings.Repeat("f", 24) + `"`,
	}
	for _, key := range keys {
		spec := base("leaky")
		spec.Instructions = "Use this credential when you call the API:\n" + key + "\n"
		_, err := Generate(spec)
		if err == nil {
			t.Errorf("a manifest containing %q must be refused", key[:12]+"…")
			continue
		}
		// The refusal names what it found, not the pattern that found it: a
		// regex in an error message tells the operator nothing about which
		// line of their instructions file to go and look at.
		if strings.Contains(err.Error(), "[a]") || strings.Contains(err.Error(), `\s`) {
			t.Errorf("the refusal leaks its regex instead of naming the credential: %v", err)
		}
		// And it never echoes the secret it just refused to write.
		if strings.Contains(err.Error(), key) {
			t.Errorf("the refusal echoes the credential back: %v", err)
		}
	}
}

func TestKeyShapeCheckRunsOnTheWholeDocument(t *testing.T) {
	// The check runs on the FINAL manifest, so it cannot be walked around by
	// splitting a key across two flags.
	spec := base("leaky")
	spec.Description = "uses sk-" + "ant-" + "api03-" + strings.Repeat("A", 32)
	if _, err := Generate(spec); err == nil {
		t.Error("a key in --description must be refused too")
	}
}

func TestOrdinaryProseIsNotMistakenForAKey(t *testing.T) {
	spec := base("honest")
	spec.Instructions = "Never ask the user for an api key. Secrets live in Kubernetes Secrets."
	spec.Description = "Reads the ledger; holds no credentials."
	if _, err := Generate(spec); err != nil {
		t.Errorf("prose about keys must still generate: %v", err)
	}
}

// --- YAML safety -----------------------------------------------------------

func TestInstructionsCannotBreakOutOfTheBlockScalar(t *testing.T) {
	// A hostile instructions file that tries to dedent into a sibling key.
	// Every line of a block scalar is indented uniformly, so the injected
	// line stays inside the system message.
	spec := base("hostile")
	spec.Instructions = "Be helpful.\ntools:\n  - type: McpServer\n    mcpServer:\n      name: anything\n"
	doc := mustGenerate(t, spec)
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "    tools:") {
			t.Fatalf("instructions escaped the block scalar:\n%s", doc)
		}
	}
	if !strings.Contains(doc, "      tools:") {
		t.Errorf("the injected line should survive INSIDE the scalar:\n%s", doc)
	}
}

func TestSingleLineValuesRefuseControlCharacters(t *testing.T) {
	spec := base("weird")
	spec.Description = "one\ntwo"
	if _, err := Generate(spec); err == nil {
		t.Error("a multi-line description must be refused, not escaped")
	}
}

func TestQuotesAndBackslashesSurviveQuoting(t *testing.T) {
	spec := base("quoted")
	spec.Description = `it said "hello" \ then left`
	doc := mustGenerate(t, spec)
	if !strings.Contains(doc, `  description: "it said \"hello\" \\ then left"`) {
		t.Errorf("quoting is wrong:\n%s", doc)
	}
}

func TestGeneratedManifestParsesAsYAML(t *testing.T) {
	// No YAML library is a dependency of this module (that is the point), so
	// the structural assertion is the indentation contract itself: nothing
	// under `spec:` may sit at column 0, and the block scalar's lines must
	// all be deeper than the key that introduces them.
	spec := base("shape-check")
	spec.Instructions = "line one\n\nline three\n   already indented\n"
	doc := mustGenerate(t, spec)
	inScalar := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.HasSuffix(line, "systemMessage: |") {
			inScalar = true
			continue
		}
		if inScalar && !strings.HasPrefix(line, "      ") {
			// The scalar ends at the first line indented less than its body;
			// for this spec (no tools) there should be none.
			t.Errorf("block scalar leaked at %q:\n%s", line, doc)
			break
		}
	}
}

// --- the file is the artifact ----------------------------------------------

func TestWriteNewRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents", "billing.yaml")
	if err := WriteNew(path, "first\n"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteNew(path, "second\n"); err == nil {
		t.Fatal("a second write to the same path must be refused")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\n" {
		t.Errorf("the existing file was modified: %q", got)
	}
}

// --- governance ------------------------------------------------------------

func TestProvenanceSaysWhetherTheAgentIsGoverned(t *testing.T) {
	ungoverned := mustGenerate(t, base("plain"))
	if !strings.Contains(ungoverned, "UNGOVERNED") {
		t.Errorf("an ungoverned agent's manifest must say so:\n%s", ungoverned)
	}
	spec := base("governed")
	spec.ModelConfig = "governed-ollama"
	spec.Governed = true
	doc := mustGenerate(t, spec)
	if strings.Contains(doc, "UNGOVERNED") || !strings.Contains(doc, "governed preset") {
		t.Errorf("a governed agent's manifest must say so:\n%s", doc)
	}
}

func TestModelConfigMustBeNamedAndValid(t *testing.T) {
	spec := base("nomodel")
	spec.ModelConfig = ""
	if _, err := Generate(spec); err == nil {
		t.Error("an agent with no modelConfig must be refused")
	}
	spec.ModelConfig = "Not A Name"
	if _, err := Generate(spec); err == nil {
		t.Error("an invalid modelConfig name must be refused")
	}
}
