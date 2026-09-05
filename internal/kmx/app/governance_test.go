package app

import (
	"bytes"
	"strings"
	"testing"
)

func agentOn(name, modelConfig string) agentStatus {
	var a agentStatus
	a.Metadata.Name = name
	a.Spec.Declarative.ModelConfig = modelConfig
	return a
}

func modelAt(name, baseURL, secret string) modelStatus {
	var m modelStatus
	m.Metadata.Name = name
	m.Spec.OpenAI.BaseURL = baseURL
	m.Spec.APIKeySecret = secret
	return m
}

func serverAt(name, url, secret string) toolServerStatus {
	var s toolServerStatus
	s.Metadata.Name = name
	s.Spec.URL = url
	if secret != "" {
		s.Spec.HeadersFrom = append(s.Spec.HeadersFrom, struct {
			ValueFrom struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"valueFrom"`
		}{})
		s.Spec.HeadersFrom[0].ValueFrom.Type = "Secret"
		s.Spec.HeadersFrom[0].ValueFrom.Name = secret
	}
	return s
}

const (
	governedModelURL = "http://kaimahi-proxy.kaimahi.svc.cluster.local:8080/upstream/ollama/v1"
	governedToolURL  = "http://kaimahi-mcp-gateway.kaimahi:8081/upstream/kagent-tools/mcp"
	directToolURL    = "http://kagent-tool-server.kagent:8084/mcp"
)

// Every DNS form of the same Service is the plane; nothing else is, however
// much it looks like it.
func TestPlaneHostAcceptsEveryServiceFormAndNothingElse(t *testing.T) {
	governed := []string{
		"http://kaimahi-proxy.kaimahi:8080/upstream/ollama/v1",
		"http://kaimahi-proxy.kaimahi.svc:8080/v1",
		governedModelURL,
		"https://KAIMAHI-PROXY.KAIMAHI.svc.cluster.local/v1",
	}
	for _, url := range governed {
		if !planeHost(url, planeProxyService) {
			t.Errorf("%q is the plane's proxy and was not counted as governed", url)
		}
	}
	direct := []string{
		"",
		"https://api.openai.com/v1",
		"http://kaimahi-proxy.other-namespace:8080/v1",
		"http://kaimahi-proxy.kaimahi.example.com/v1",
		"http://kaimahi-proxy-evil.kaimahi:8080/v1",
		"http://10.0.0.5:8080/v1",
	}
	for _, url := range direct {
		if planeHost(url, planeProxyService) {
			t.Errorf("%q is not the plane and was counted as governed", url)
		}
	}
}

func TestModelSeamsCountGovernedAndDirect(t *testing.T) {
	agents := []agentStatus{agentOn("hello-world", "governed-ollama"), agentOn("hello-tools", "ollama")}
	models := []modelStatus{
		modelAt("governed-ollama", governedModelURL, "kaimahi-governed-token"),
		modelAt("ollama", "", ""),
	}
	got := modelSeams(agents, models)
	if got.State != stateCounted || got.Total != 2 || got.Governed != 1 || got.Direct != 1 || got.Unresolved != 0 {
		t.Fatalf("model seams miscounted: %+v", got)
	}
}

// A dangling ModelConfig reference is `unknown`, never `direct`: the object
// that would answer the question is not on the cluster.
func TestModelSeamsRefuseToCallADanglingReferenceDirect(t *testing.T) {
	got := modelSeams([]agentStatus{agentOn("orphan", "gone")}, nil)
	if got.Direct != 0 || got.Unresolved != 1 {
		t.Fatalf("a dangling ModelConfig was resolved anyway: %+v", got)
	}
	if !strings.Contains(got.Reason, "orphan→gone") {
		t.Errorf("the reason does not name the dangling pair: %q", got.Reason)
	}
	if line := seamLine(got, "agents", "agent"); !strings.Contains(line, "1 unknown") {
		t.Errorf("the printed line hides the unknown: %q", line)
	}
}

// No agents at all is a known nothing — W30's `none` — and not a zero
// pretending to be a count.
func TestEmptyPopulationsAreNoneNotZeroGoverned(t *testing.T) {
	if got := modelSeams(nil, nil); got.State != stateNone {
		t.Errorf("an empty agent population is not `none`: %+v", got)
	}
	if got := toolSeams(nil, ""); got.State != stateNone {
		t.Errorf("an empty tool-server population is not `none`: %+v", got)
	}
	if line := seamLine(toolSeams(nil, ""), "tool servers", "tool server"); strings.Contains(line, "0 of 0") {
		t.Errorf("an empty population printed as a count: %q", line)
	}
}

// The branch this lane exists for: a read that failed says so. "0 governed"
// would be a claim about a population nobody managed to look at.
func TestUnreadablePopulationIsUnknownNotZero(t *testing.T) {
	got := toolSeams(nil, `the server doesn't have a resource type "remotemcpservers"`)
	if got.State != stateUnknown || got.Governed != 0 || got.Total != 0 {
		t.Fatalf("an unreadable population was counted: %+v", got)
	}
	line := seamLine(got, "tool servers", "tool server")
	if !strings.HasPrefix(line, "unknown — ") || !strings.Contains(line, "remotemcpservers") {
		t.Errorf("the unknown line does not carry kubectl's reason: %q", line)
	}
}

func TestToolSeamsCountGovernedAndDirect(t *testing.T) {
	servers := []toolServerStatus{
		serverAt("kaimahi-tools", governedToolURL, "kaimahi-tools-token"),
		serverAt("kagent-tool-server", directToolURL, ""),
		serverAt("kagent-querydoc", "http://querydoc.kagent:8080/mcp", ""),
	}
	got := toolSeams(servers, "")
	if got.Total != 3 || got.Governed != 1 || got.Direct != 2 {
		t.Fatalf("tool seams miscounted: %+v", got)
	}
	if line := seamLine(got, "tool servers", "tool server"); line != "1 of 3 tool servers governed, 2 direct" {
		t.Errorf("unexpected line: %q", line)
	}
}

// Only the GOVERNED seams name a credential, and a named Secret that is not
// there is reported rather than left to fail at the next call.
func TestCredentialsCountOnlyWhatGovernedSeamsName(t *testing.T) {
	models := []modelStatus{
		modelAt("governed-ollama", governedModelURL, "kaimahi-governed-token"),
		modelAt("openai", "https://api.openai.com/v1", "openai-key"),
	}
	servers := []toolServerStatus{serverAt("kaimahi-tools", governedToolURL, "kaimahi-tools-token")}

	got := credentialSeams(models, servers, []string{"kaimahi-governed-token", "openai-key"}, "")
	if got.Required != 2 || got.Present != 1 || len(got.Missing) != 1 || got.Missing[0] != "kaimahi-tools-token" {
		t.Fatalf("credential population wrong: %+v", got)
	}
	if line := credentialLine(got); !strings.Contains(line, "missing: kaimahi-tools-token") {
		t.Errorf("the missing credential is not named: %q", line)
	}
	// The ungoverned upstream key is deliberately NOT required: it is not a
	// credential the plane issued and status makes no claim about it.
	if got.Required == 3 {
		t.Error("an ungoverned seam's own key was counted as a governed credential")
	}
}

func TestCredentialsAreNoneWhenNoGovernedSeamNamesOne(t *testing.T) {
	got := credentialSeams([]modelStatus{modelAt("ollama", "", "")}, nil, nil, "")
	if got.State != stateNone {
		t.Fatalf("no governed seam should be `none`, got %+v", got)
	}
	if credentialLine(got) != "none — no governed seam names one" {
		t.Errorf("unexpected line: %q", credentialLine(got))
	}
}

func TestCredentialsAreUnknownWhenTheToolSeamsCouldNotBeRead(t *testing.T) {
	d := &statusData{serverErr: "connection refused"}
	if reason := credentialReason(d); reason == "" {
		t.Fatal("an unreadable tool-seam population left the credential count claiming to be complete")
	}
	got := credentialSeams(nil, nil, nil, credentialReason(d))
	if got.State != stateUnknown {
		t.Fatalf("expected unknown, got %+v", got)
	}
}

// The no-plane branch: the counts still work, and the output SAYS the plane
// is absent rather than reporting a bare zero an operator would read as
// "you have not got round to it yet".
func TestGovernanceWithNoPlaneSaysSoAndStillCounts(t *testing.T) {
	d := &statusData{}
	d.agents.Items = []agentStatus{agentOn("hello-world", "ollama")}
	d.models.Items = []modelStatus{modelAt("ollama", "", "")}
	d.servers.Items = []toolServerStatus{serverAt("kagent-tool-server", directToolURL, "")}

	g := d.governance()
	if g.Plane.State != stateNone {
		t.Fatalf("no plane pods should read as none: %+v", g.Plane)
	}
	var out bytes.Buffer
	writeGovernance(&out, g)
	text := out.String()
	for _, want := range []string{"not installed", "0 of 1 agent governed, 1 direct", "0 of 1 tool server governed, 1 direct", "none — no governed seam names one"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

// The cannot-tell branch: an unreadable plane namespace is not an absent
// plane, and status must not say "not installed" about a namespace it could
// not read.
func TestGovernanceCannotTellIsNotNotInstalled(t *testing.T) {
	d := &statusData{planeErr: "Error from server (Forbidden): pods is forbidden"}
	g := d.governance()
	if g.Plane.State != stateUnknown {
		t.Fatalf("an unreadable plane namespace was reported as absent: %+v", g.Plane)
	}
	var out bytes.Buffer
	writeGovernance(&out, g)
	if text := out.String(); !strings.Contains(text, "plane:        unknown — Error from server (Forbidden)") || strings.Contains(text, "not installed") {
		t.Errorf("cannot-tell was conflated with not-installed:\n%s", text)
	}
}

func TestGovernanceWithAPlaneCountsGovernedSeams(t *testing.T) {
	d := &statusData{}
	d.agents.Items = []agentStatus{agentOn("hello-world", "governed-ollama"), agentOn("hello-tools", "ollama")}
	d.models.Items = []modelStatus{
		modelAt("governed-ollama", governedModelURL, "kaimahi-governed-token"),
		modelAt("ollama", "", ""),
	}
	d.servers.Items = []toolServerStatus{
		serverAt("kaimahi-tools", governedToolURL, "kaimahi-tools-token"),
		serverAt("kagent-tool-server", directToolURL, ""),
	}
	d.secrets = []string{"kaimahi-governed-token", "kaimahi-tools-token"}
	var pod podStatus
	pod.Metadata.Name = "kaimahi-proxy-0"
	pod.Status.Conditions = []statusCondition{{Type: "Ready", Status: "True"}}
	d.planePods.Items = []podStatus{pod}

	var out bytes.Buffer
	writeGovernance(&out, d.governance())
	text := out.String()
	for _, want := range []string{"installed (1/1 pods ready)", "1 of 2 agents governed, 1 direct", "1 of 2 tool servers governed, 1 direct", "2 of 2 present"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestFirstLineKeepsKubectlsOwnComplaint(t *testing.T) {
	if got := firstLine("exit status 1: Error from server (NotFound)\ntrailing detail"); got != "exit status 1: Error from server (NotFound)" {
		t.Errorf("unexpected reason: %q", got)
	}
}
