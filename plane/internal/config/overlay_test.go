package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal well-formed base table: Parse insists on at least one LLM
// upstream, so every merged result below has to carry one.
const overlayBase = `{
  "upstreams": {
    "ollama": {"base_url": "http://ollama.ollama.svc.cluster.local:11434", "path": "v1/chat/completions", "classification": "free"}
  },
  "tool_upstreams": {
    "kagent-tools": {"url": "http://kagent-tools.kagent:8084/mcp", "tools": {"k8s_get_events": {"policy_fields": ["namespace"]}}}
  }
}`

func mergeParse(t *testing.T, frags ...Fragment) (Config, error) {
	t.Helper()
	merged, err := Merge([]byte(overlayBase), frags)
	if err != nil {
		return Config{}, err
	}
	return Parse(merged)
}

func TestAnOverlayAddsAToolUpstreamWithoutTouchingTheCommittedOnes(t *testing.T) {
	cfg, err := mergeParse(t, Fragment{Name: "warehouse.json", Raw: []byte(`{
	  "tool_upstreams": {
	    "warehouse": {"url": "http://acme-warehouse.acme:8090/mcp",
	                  "tools": {"stock_get": {"policy_fields": ["sku"]}}}
	  }
	}`)})
	if err != nil {
		t.Fatalf("merge+parse: %v", err)
	}
	if got := cfg.ToolUpstreams["warehouse"].URL; got != "http://acme-warehouse.acme:8090/mcp" {
		t.Fatalf("overlay upstream url = %q", got)
	}
	// The committed entry survives unchanged — the whole point of the
	// overlay is that onboarding never rewrites this repo's four.
	committed, ok := cfg.ToolUpstreams["kagent-tools"]
	if !ok || committed.URL != "http://kagent-tools.kagent:8084/mcp" {
		t.Fatalf("committed upstream lost or changed: %+v", committed)
	}
	if _, ok := cfg.Policy().Declared("k8s_get_events"); !ok {
		t.Fatal("the committed policy declaration did not survive the merge")
	}
	if fields, ok := cfg.Policy().Declared("stock_get"); !ok || len(fields) != 1 || fields[0] != "sku" {
		t.Fatalf("overlay declaration not in the policy set: %v %v", fields, ok)
	}
}

func TestAnOverlayMayNotRedefineACommittedUpstream(t *testing.T) {
	_, err := mergeParse(t, Fragment{Name: "sneaky.json", Raw: []byte(`{
	  "tool_upstreams": {"kagent-tools": {"url": "http://elsewhere.acme:8090/mcp"}}
	}`)})
	if err == nil {
		t.Fatal("an overlay silently repointed a committed upstream")
	}
	for _, want := range []string{"redefines", "kagent-tools", "the committed table", "sneaky.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name both sources, got: %v", err)
		}
	}
}

func TestTwoOverlaysMayNotDefineTheSameName(t *testing.T) {
	_, err := mergeParse(t,
		Fragment{Name: "a.json", Raw: []byte(`{"tool_upstreams": {"warehouse": {"url": "http://a.acme:80/mcp"}}}`)},
		Fragment{Name: "b.json", Raw: []byte(`{"tool_upstreams": {"warehouse": {"url": "http://b.acme:80/mcp"}}}`)},
	)
	if err == nil || !strings.Contains(err.Error(), "overlay a.json") {
		t.Fatalf("want a collision naming the earlier fragment, got: %v", err)
	}
}

func TestAnOverlayMayNotSetTheSeamsItDoesNotOwn(t *testing.T) {
	// Each of these is entangled with a credential mount or a signing
	// secret. An onboarding path that could rewrite them would have a
	// blast radius far beyond the tool seam it exists for.
	for _, block := range []string{"upstreams", "inbound_hooks", "approval_notifier"} {
		_, err := mergeParse(t, Fragment{Name: "x.json", Raw: []byte(`{"` + block + `": {}}`)})
		if err == nil || !strings.Contains(err.Error(), block) {
			t.Fatalf("overlay set %q and was not refused: %v", block, err)
		}
	}
}

func TestADuplicateKeyInAnOverlayIsRefusedNotCollapsed(t *testing.T) {
	// Go's decoder takes the last occurrence; a reviewer reads the
	// first. A table where those disagree has not been reviewed.
	_, err := mergeParse(t, Fragment{Name: "dup.json", Raw: []byte(`{
	  "tool_upstreams": {
	    "warehouse": {"url": "http://real.acme:80/mcp", "url": "http://evil.acme:80/mcp"}
	  }
	}`)})
	if err == nil || !strings.Contains(err.Error(), `duplicate key "url"`) {
		t.Fatalf("want a duplicate-key refusal, got: %v", err)
	}
}

func TestADuplicateKeyInTheCommittedTableIsRefusedToo(t *testing.T) {
	dup := `{"upstreams": {}, "upstreams": {}}`
	if _, err := Merge([]byte(dup), nil); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("want a refusal for the base table, got: %v", err)
	}
}

func TestAnOverlayGoesThroughEveryRuleParseAlreadyEnforces(t *testing.T) {
	// The merge validates nothing but its own structure. These are all
	// Parse's rules, reached through the overlay — proof that onboarding
	// cannot land a table the proxy would not have accepted anyway.
	for _, tc := range []struct{ name, frag, want string }{
		{"a hosted url with no marker", `{"tool_upstreams": {"w": {"url": "https://example.com/mcp"}}}`, "in-cluster"},
		{"policy_fields omitted", `{"tool_upstreams": {"w": {"url": "http://w.acme:80/mcp", "tools": {"t": {}}}}}`, "policy_fields is required"},
		{"a tool declared differently elsewhere", `{"tool_upstreams": {"w": {"url": "http://w.acme:80/mcp", "tools": {"k8s_get_events": {"policy_fields": ["pod"]}}}}}`, "k8s_get_events"},
		{"an unknown key", `{"tool_upstreams": {"w": {"url": "http://w.acme:80/mcp", "nope": 1}}}`, "nope"},
		{"a constraint on an undeclared tool", `{"standing_constraints": {"c": {"nosuch": [{"field": "a", "op": "eq", "value": 1}]}}}`, "nosuch"},
	} {
		_, err := mergeParse(t, Fragment{Name: "f.json", Raw: []byte(tc.frag)})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want an error containing %q, got: %v", tc.name, tc.want, err)
		}
	}
}

func TestAConstraintInAnOverlayBindsTheOverlaysOwnTool(t *testing.T) {
	cfg, err := mergeParse(t, Fragment{Name: "w.json", Raw: []byte(`{
	  "tool_upstreams": {"warehouse": {"url": "http://acme-warehouse.acme:8090/mcp",
	     "tools": {"stock_adjust": {"policy_fields": ["sku", "delta"]}}}},
	  "standing_constraints": {"acme-agent": {"stock_adjust": [{"field": "delta", "op": "lte", "value": 10}]}}
	}`)})
	if err != nil {
		t.Fatalf("merge+parse: %v", err)
	}
	rules, ok := cfg.Policy().Constraints("acme-agent", "stock_adjust")
	if !ok || len(rules) != 1 {
		t.Fatalf("constraint did not survive the merge: %v %v", rules, ok)
	}
}

func TestReadSkipsTheSymlinksAConfigMapVolumePlants(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "upstreams.json")
	if err := os.WriteFile(base, []byte(overlayBase), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "upstreams.d")
	if err := os.MkdirAll(filepath.Join(overlay, "..2026_09_03"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"b.json":    `{"tool_upstreams": {"b": {"url": "http://b.acme:80/mcp"}}}`,
		"a.json":    `{"tool_upstreams": {"a": {"url": "http://a.acme:80/mcp"}}}`,
		"..data":    `not json at all`,
		"notes.txt": `not json either`,
	} {
		if err := os.WriteFile(filepath.Join(overlay, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, frags, err := Read(base, overlay)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(frags) != 2 || frags[0].Name != "a.json" || frags[1].Name != "b.json" {
		t.Fatalf("want a.json then b.json, got %+v", frags)
	}
	cfg, err := LoadDir(base, overlay)
	if err != nil {
		t.Fatalf("loaddir: %v", err)
	}
	if len(cfg.ToolUpstreams) != 3 {
		t.Fatalf("want the committed one plus two, got %d", len(cfg.ToolUpstreams))
	}
}

func TestAnAbsentOverlayDirectoryIsAnEmptyOverlayNotAnError(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "upstreams.json")
	if err := os.WriteFile(base, []byte(overlayBase), 0o600); err != nil {
		t.Fatal(err)
	}
	// The volume is optional: on a cluster where nobody has onboarded
	// anything, the directory does not exist and the plane must boot.
	cfg, err := LoadDir(base, filepath.Join(dir, "nothing-here"))
	if err != nil {
		t.Fatalf("an absent overlay must not stop the plane: %v", err)
	}
	if len(cfg.ToolUpstreams) != 1 {
		t.Fatalf("want the committed table alone, got %d entries", len(cfg.ToolUpstreams))
	}
}

func TestMergingNothingReproducesTheCommittedTable(t *testing.T) {
	merged, err := Merge([]byte(overlayBase), nil)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal([]byte(overlayBase), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if a, _ := json.Marshal(want); string(a) != mustMarshal(t, got) {
		t.Fatalf("an empty overlay changed the table:\n %s\n %s", a, mustMarshal(t, got))
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
