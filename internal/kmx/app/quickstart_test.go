package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kaimahi-agents/kaimahi/internal/kmx/config"
	"github.com/kaimahi-agents/kaimahi/internal/kmx/run"
)

// An unrecognised --output is refused BEFORE a cluster is created. Being told
// "unknown output" four minutes into a bring-up would be the worst possible
// moment to find out.
func TestQuickstartRejectsAnUnknownOutputBeforeDoingAnything(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("KMX_TOOLCHAIN", "off")
	a := &App{Cfg: &config.Config{ContainerEngine: "docker"}, Run: &run.Runner{}, Err: &bytes.Buffer{}, Out: &bytes.Buffer{}}
	err := a.Quickstart(QuickstartOptions{Output: "yaml"})
	if err == nil || !strings.Contains(err.Error(), "unknown --output") {
		t.Fatalf("got %v, want a refusal naming the output", err)
	}
}

// The structured result is what an agent in a harness reads. Two fields carry
// weight beyond their type: `ok` is the whole answer to "did this work", and
// `governed` must be false, because the fast path deliberately deploys no
// plane and a machine-readable claim of governance would be a lie in JSON.
func TestQuickstartResultSaysPlainlyThatNothingIsGoverned(t *testing.T) {
	result := QuickstartResult{OK: true, Agent: "hello-world", Answer: "hello", Governed: false}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ok", "context", "cluster", "agent", "question", "answer", "governed", "elapsed_seconds", "next"} {
		if _, present := decoded[key]; !present {
			t.Errorf("the structured output has no %q field: %s", key, raw)
		}
	}
	if decoded["governed"] != false {
		t.Errorf("the fast path must report itself ungoverned: %s", raw)
	}
}

// The answer is read out of the A2A task the same way `chat` reads it, and a
// response with no readable reply must not be reported as an answer.
func TestParseTaskFindsTheReplyAndRefusesRubbish(t *testing.T) {
	good := `some kagent logging
{"artifacts":[{"parts":[{"kind":"text","text":"I am a declarative kagent agent."}]}],"status":{"state":"completed"}}`
	if got := strings.TrimSpace(firstText(parseTask(good))); got != "I am a declarative kagent agent." {
		t.Errorf("reply not found: %q", got)
	}
	for _, bad := range []string{"", "no json here", "{not json}"} {
		if got := firstText(parseTask(bad)); got != "" {
			t.Errorf("%q yielded a reply %q", bad, got)
		}
	}
}

// The first-answer profile must turn off exactly the components the first
// question cannot reach — and must not touch the model provider, which is
// the one thing the answer depends on.
func TestTheFirstAnswerProfileOnlyDefersUnreachableComponents(t *testing.T) {
	joined := strings.Join(quickstartValues, " ")
	for _, off := range []string{"kagent-tools.enabled=false", "kmcp.enabled=false", "ui.replicas=0"} {
		if !strings.Contains(joined, off) {
			t.Errorf("the first-answer profile does not defer %s: %s", off, joined)
		}
	}
	if strings.Contains(joined, "providers") || strings.Contains(joined, "ollama") {
		t.Errorf("the first-answer profile must not touch the model provider: %s", joined)
	}
}
