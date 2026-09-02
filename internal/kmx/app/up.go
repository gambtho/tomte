package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kaimahi-agents/kaimahi/internal/kmx/config"
	"github.com/kaimahi-agents/kaimahi/internal/kmx/run"
)

// Steps of `kmx up`, in order. They are addressable individually so the
// Makefile's `cluster`, `ollama`, `model`, `kagent`, `agent` and
// `tools-agent` targets can delegate to the same code rather than keeping a
// second copy of it.
var UpSteps = []string{"cluster", "ollama", "model", "kagent", "agent", "tools-agent"}

// Up runs the whole journey, or the single named step.
//
// This is the Makefile's kind `UP_STEPS` — cluster, ollama, model, kagent,
// agent, tools-agent, status. RUNTIME ONLY: milestone 1 does not deploy the
// governance plane (D27), and says so at the end rather than leaving anyone
// to discover it from an empty ledger.
func (a *App) Up(step string) error {
	steps := UpSteps
	if step != "" {
		found := false
		for _, s := range UpSteps {
			if s == step {
				found, steps = true, []string{s}
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown step %q — one of: %s", step, strings.Join(UpSteps, ", "))
		}
	}

	action := "bring up the kmx runtime (kind, Ollama, kagent, agents)"
	command := "kmx up"
	if step != "" {
		action, command = "run the '"+step+"' step", "kmx up --step "+step
	}
	if err := a.Guard(action, command); err != nil {
		return err
	}

	for _, s := range steps {
		var err error
		switch s {
		case "cluster":
			err = a.stepCluster()
		case "ollama":
			err = a.stepOllama()
		case "model":
			err = a.stepModel()
		case "kagent":
			err = a.stepKagent()
		case "agent":
			err = a.stepAgent()
		case "tools-agent":
			err = a.stepToolsAgent()
		}
		if err != nil {
			return err
		}
	}

	if step == "" {
		if err := a.Status(); err != nil {
			return err
		}
		// One line, per D27: `kmx up` is the runtime, and the governance
		// plane is not part of milestone 1. Saying nothing here would leave
		// an operator to infer it from an empty ledger.
		a.notef("\nRuntime only: the Kaimahi governance plane is NOT deployed (kmx milestone 1).\n" +
			"Budgets, the spend ledger, the tool gateway and approvals come from the\n" +
			"Makefile for now: `make plane` then `make govern` (docs/spend.md).")
		a.notef("\nTalk to the agent:  kmx agent chat %s \"%s\"", config.DefaultAgent, config.DefaultTask)
	}
	return nil
}

// ---- cluster --------------------------------------------------------------

// stepCluster creates the kind cluster if it is not already there.
//
// `kind get clusters` failing is NOT "no clusters": a broken or absent kind,
// or a docker socket the user cannot reach, would otherwise read as "the
// cluster is missing" and send us straight into a create that fails later
// and less clearly.
func (a *App) stepCluster() error {
	if err := run.MustExist("kind", "to create the local Kubernetes cluster",
		"https://kind.sigs.k8s.io/docs/user/quick-start/#installation"); err != nil {
		return err
	}
	if err := run.MustExist("kubectl", "to talk to the cluster",
		"https://kubernetes.io/docs/tasks/tools/"); err != nil {
		return err
	}
	existing, err := a.Run.Capture("kind", "get", "clusters")
	if err != nil {
		return fmt.Errorf("`kind get clusters` failed — refusing to guess whether %q exists (is the %s daemon running?): %w",
			a.Cfg.KindCluster, a.Cfg.ContainerEngine, err)
	}
	listed := false
	for _, line := range strings.Split(existing, "\n") {
		if strings.TrimSpace(line) == a.Cfg.KindCluster {
			listed = true
			break
		}
	}

	// The Docker path, unchanged: create it if it is not there.
	if a.Cfg.ContainerEngine != "podman" {
		if listed {
			a.notef("kind cluster %q already exists", a.Cfg.KindCluster)
			return nil
		}
		return a.Run.Run("kind", "create", "cluster", "--name", a.Cfg.KindCluster)
	}

	// The Podman path (#53, by @sajayantony): restarting the podman machine
	// stops kind's node containers, after which `kind get clusters` still
	// lists the cluster and nothing can reach it. Listed-but-stopped is
	// therefore not "already exists" — start the nodes back up.
	if listed {
		nodes, err := a.Run.Capture("podman", "ps", "-a",
			"--filter", "label=io.x-k8s.kind.cluster="+a.Cfg.KindCluster,
			"--format", "{{.Names}}")
		if err != nil {
			return fmt.Errorf("cannot list podman's nodes for kind cluster %q: %w", a.Cfg.KindCluster, err)
		}
		if strings.TrimSpace(nodes) == "" {
			// Fail closed rather than proceed into a cluster that cannot be
			// reached: kind's record and podman's reality disagree.
			return fmt.Errorf("kind lists cluster %q, but podman has no nodes for it", a.Cfg.KindCluster)
		}
		args := append([]string{"start"}, strings.Fields(nodes)...)
		if err := a.Run.Run("podman", args...); err != nil {
			return err
		}
	} else if err := a.Run.Run("kind", "create", "cluster", "--name", a.Cfg.KindCluster); err != nil {
		return err
	}

	// Restarted or freshly created, the API server and CoreDNS have to be up
	// before anything else runs: without this the Ollama model pull could
	// start into a cluster that was not yet serving.
	ready := run.Poll(60, 2*time.Second, func() bool {
		return a.kubectlQuiet("get", "--raw=/readyz")
	})
	if !ready {
		return fmt.Errorf("kind cluster %q API did not become ready after 120s", a.Cfg.KindCluster)
	}
	return a.kubectlRun("-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=180s")
}

// ---- ollama ---------------------------------------------------------------

func (a *App) stepOllama() error {
	if err := a.apply("ollama.yaml"); err != nil {
		return err
	}
	return a.kubectlRun("-n", "ollama", "rollout", "status", "deploy/ollama", "--timeout=300s")
}

// stepModel pulls the pinned model into the running Ollama.
func (a *App) stepModel() error {
	return a.kubectlRun("-n", "ollama", "exec", "deploy/ollama", "--", "ollama", "pull", a.Cfg.Model)
}

// ---- kagent ---------------------------------------------------------------

func (a *App) stepKagent() error {
	if err := run.MustExist("helm", "to install kagent", "https://helm.sh/docs/intro/install/"); err != nil {
		return err
	}
	version := a.Cfg.KagentVersion
	if err := a.Run.Run("helm", "upgrade", "--install", "kagent-crds",
		"oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds",
		"--version", version, "--namespace", "kagent", "--create-namespace",
		"--kube-context", a.Cfg.KubeContext); err != nil {
		return err
	}

	// helm -f wants a path, and the values file lives inside the binary.
	values, err := manifest("kagent-values.yaml")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "kagent-values-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(values); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := a.Run.Run("helm", "upgrade", "--install", "kagent",
		"oci://ghcr.io/kagent-dev/kagent/helm/kagent",
		"--version", version, "--namespace", "kagent",
		"--kube-context", a.Cfg.KubeContext, "-f", tmp.Name()); err != nil {
		return err
	}
	return a.kubectlRun("-n", "kagent", "wait", "--for=condition=Ready", "pods", "--all", "--timeout=420s")
}

// ---- the agents -----------------------------------------------------------

// liveModelConfig reads an agent's current modelConfig.
//
// Re-applying the committed YAML must not silently drop governance (or any
// preset switch) from a live agent, so the current value is captured first
// and a non-default one is restored after the apply, with a warning. Only a
// NotFound (fresh cluster) may skip the capture — ANY other read failure
// aborts rather than risk silently un-governing an agent.
func (a *App) liveModelConfig(agent string) (string, error) {
	out, err := a.kubectlCapture("-n", "kagent", "get", "agent", agent,
		"-o", "jsonpath={.spec.declarative.modelConfig}")
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found") {
			return "", nil
		}
		return "", fmt.Errorf("cannot read %s's live modelConfig (refusing to risk un-governing it): %w", agent, err)
	}
	return strings.TrimSpace(out), nil
}

// desiredModelConfig applies the preservation rule: a live non-default
// modelConfig wins over the committed one.
func (a *App) desiredModelConfig(agent, current string) (string, bool) {
	if current != "" && current != config.KeylessModelConfig {
		a.notef("NOTE: %s was on modelConfig %q — preserving it ('make use PRESET=ollama' resets)", agent, current)
		return current, true
	}
	return config.KeylessModelConfig, false
}

func (a *App) patchModelConfig(agent, modelConfig string) error {
	patch := fmt.Sprintf(`{"spec":{"declarative":{"modelConfig":%q}}}`, modelConfig)
	return a.kubectlRun("-n", "kagent", "patch", "agent", agent, "--type", "merge", "-p", patch)
}

func (a *App) waitAgentReady(agent string) error {
	return a.kubectlRun("-n", "kagent", "wait",
		`--for=jsonpath={.status.conditions[?(@.type=="Ready")].status}=True`,
		"agent/"+agent, "--timeout=300s")
}

func (a *App) stepAgent() error {
	current, err := a.liveModelConfig("hello-world")
	if err != nil {
		return err
	}
	desired, changed := a.desiredModelConfig("hello-world", current)
	if err := a.apply("hello-world.yaml"); err != nil {
		return err
	}
	if changed {
		if err := a.patchModelConfig("hello-world", desired); err != nil {
			return err
		}
	}
	return a.waitAgentReady("hello-world")
}

// agentJSON is the sliver of an Agent kmx reads back.
type agentJSON struct {
	Spec struct {
		Declarative struct {
			ModelConfig string          `json:"modelConfig"`
			Tools       json.RawMessage `json:"tools"`
		} `json:"declarative"`
	} `json:"spec"`
}

// stepToolsAgent applies the tools agent, preserving both a non-default
// modelConfig and a live gateway wiring.
//
// The gateway restore is P4c's governance-preservation guard: once
// `make govern-tools` has pointed hello-tools at the kaimahi-tools seam,
// re-applying the committed YAML would point it back at the ungoverned
// server. Re-applying a manifest must never be the thing that un-governs an
// agent.
func (a *App) stepToolsAgent() error {
	// The RemoteMCPServer the chart publishes has to be accepted before an
	// Agent can wire to it.
	if err := a.kubectlRun("-n", "kagent", "wait",
		`--for=jsonpath={.status.conditions[?(@.type=="Accepted")].status}=True`,
		"remotemcpserver/kagent-tool-server", "--timeout=300s"); err != nil {
		return err
	}

	var live agentJSON
	raw, err := a.kubectlCapture("-n", "kagent", "get", "agent", "hello-tools", "-o", "json")
	switch {
	case err != nil && (strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found")):
		// Fresh cluster: nothing to preserve.
	case err != nil:
		return fmt.Errorf("cannot read hello-tools' live tool wiring (refusing to risk un-governing it): %w", err)
	default:
		if err := json.Unmarshal([]byte(raw), &live); err != nil {
			return fmt.Errorf("cannot parse hello-tools: %w", err)
		}
	}

	server := ""
	if len(live.Spec.Declarative.Tools) > 0 {
		var tools []struct {
			McpServer struct {
				Name string `json:"name"`
			} `json:"mcpServer"`
		}
		if err := json.Unmarshal(live.Spec.Declarative.Tools, &tools); err == nil && len(tools) > 0 {
			server = tools[0].McpServer.Name
		}
	}

	desired, changed := a.desiredModelConfig("hello-tools", live.Spec.Declarative.ModelConfig)
	if err := a.apply("tools-agent.yaml"); err != nil {
		return err
	}
	if changed {
		if err := a.patchModelConfig("hello-tools", desired); err != nil {
			return err
		}
	}
	if server == "kaimahi-tools" && len(live.Spec.Declarative.Tools) > 0 {
		a.notef("NOTE: hello-tools was governed via kaimahi-tools — restoring gateway wiring ('make ungovern-tools' opts out)")
		patch := fmt.Sprintf(`{"spec":{"declarative":{"tools":%s}}}`, string(live.Spec.Declarative.Tools))
		if err := a.kubectlRun("-n", "kagent", "patch", "agent", "hello-tools", "--type", "merge", "-p", patch); err != nil {
			return err
		}
	}
	return a.waitAgentReady("hello-tools")
}
