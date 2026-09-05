package app

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
)

// What `kmx status` can say about governance, and how it refuses to guess.
//
// W29 asked for the ungoverned state to be COUNTABLE rather than warned
// about once: "3 tool servers, 0 governed" is harder to ignore than a
// message that scrolls past. D36 makes it matter more, not less — the fast
// path is ungoverned by design, so the ungoverned path is the DEFAULT one
// and nothing else in the tree tells an operator how much of their system
// is on it.
//
// Two rules hold this together:
//
//  1. **The test is local.** A seam is governed when it POINTS AT the
//     plane's Service. That is read off the cluster objects themselves, so
//     the count works with no plane, no credential and no network — which
//     is the whole point of a command people run when something is wrong.
//     It is a claim about wiring, never about a plane answering: the plane
//     line beside the counts is what says whether anything is actually in
//     front of them.
//
//  2. **A zero is never invented.** W30 fixed the vocabulary for exactly
//     this on `acted_for`: `none` is a known nothing, `unknown` is "we
//     cannot say". A missing CRD, an RBAC denial or a dangling ModelConfig
//     reference produces `unknown` with the reason, never "0 governed".

// The plane's in-cluster seams, mirrored from k8s/plane/proxy.yaml: the
// model seam is the proxy, the tool seam is the MCP gateway. They are
// constants rather than a lookup because status must classify wiring on a
// cluster where the plane is not installed at all.
const (
	planeNamespace      = "kaimahi"
	planeProxyService   = "kaimahi-proxy"
	planeGatewayService = "kaimahi-mcp-gateway"
)

// The three answers a population can carry. `none` and `unknown` are W30's
// words, kept rather than reinvented.
const (
	stateCounted   = "counted"
	stateNone      = "none"
	stateUnknown   = "unknown"
	stateInstalled = "installed"
)

// seamPopulation counts one KIND of governable thing. Agents, tool servers
// and credentials are three different populations and a single number that
// mixed them would be worse than three honest ones, so each is counted and
// printed on its own.
type seamPopulation struct {
	// State is counted, none (nothing of this kind exists) or unknown.
	State string `json:"state"`
	// Total, Governed and Direct are meaningful only when State is
	// counted; Unresolved counts members whose seam could not be read —
	// they are excluded from Governed and Direct rather than assumed.
	Total      int `json:"total"`
	Governed   int `json:"governed"`
	Direct     int `json:"direct"`
	Unresolved int `json:"unresolved"`
	// Reason says why, whenever State is unknown or Unresolved is not 0.
	Reason string `json:"reason,omitempty"`
}

// credentialPopulation is the third population: the Secrets the governed
// seams NAME. Only names are read — never a Secret's value — because a
// governed seam whose token Secret is missing is a real broken state that
// otherwise surfaces as a runtime failure much later.
type credentialPopulation struct {
	State    string   `json:"state"`
	Required int      `json:"required"`
	Present  int      `json:"present"`
	Missing  []string `json:"missing,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// planePresence is whether anything is in front of the governed seams.
type planePresence struct {
	// State is installed, none (no plane on this cluster) or unknown.
	State  string `json:"state"`
	Ready  int    `json:"ready"`
	Pods   int    `json:"pods"`
	Reason string `json:"reason,omitempty"`
}

// governance is the whole answer, and the shape `-o json` publishes.
type governance struct {
	Plane       planePresence        `json:"plane"`
	ModelSeams  seamPopulation       `json:"modelSeams"`
	ToolSeams   seamPopulation       `json:"toolSeams"`
	Credentials credentialPopulation `json:"credentials"`
}

// planeHost reports whether a URL's host is the named plane Service.
//
// kagent resolves these through the cluster DNS, so every form of the same
// Service has to count: `svc.ns`, `svc.ns.svc` and the fully qualified
// `svc.ns.svc.cluster.local`, with or without a port. Anything else — a
// vendor endpoint, another namespace, an IP — is not the plane, and saying
// so is the point of the count.
func planeHost(rawURL, service string) bool {
	if strings.TrimSpace(rawURL) == "" {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	labels := strings.Split(strings.ToLower(parsed.Hostname()), ".")
	if len(labels) < 2 || labels[0] != service || labels[1] != planeNamespace {
		return false
	}
	switch len(labels) {
	case 2:
		return true
	case 3:
		return labels[2] == "svc"
	case 5:
		return labels[2] == "svc" && labels[3] == "cluster" && labels[4] == "local"
	}
	return false
}

// modelSeams counts agents by where their model calls actually go.
//
// The agent names a ModelConfig; the ModelConfig names a base URL. An agent
// whose ModelConfig is not on the cluster is UNRESOLVED, not ungoverned:
// the object it points at is missing, and "direct" would be a claim about
// a seam nobody can read.
func modelSeams(agents []agentStatus, models []modelStatus) seamPopulation {
	byName := make(map[string]modelStatus, len(models))
	for _, model := range models {
		byName[model.Metadata.Name] = model
	}
	population := seamPopulation{State: stateCounted, Total: len(agents)}
	if len(agents) == 0 {
		population.State = stateNone
		return population
	}
	var dangling []string
	for _, agent := range agents {
		name := strings.TrimSpace(agent.Spec.Declarative.ModelConfig)
		model, ok := byName[name]
		if !ok {
			population.Unresolved++
			dangling = append(dangling, fmt.Sprintf("%s→%s", agent.Metadata.Name, valueOr(name, "(none)")))
			continue
		}
		if planeHost(model.Spec.OpenAI.BaseURL, planeProxyService) {
			population.Governed++
		} else {
			population.Direct++
		}
	}
	if len(dangling) > 0 {
		sort.Strings(dangling)
		population.Reason = "no ModelConfig on this cluster for " + strings.Join(dangling, ", ")
	}
	return population
}

// toolSeams counts tool servers by whether their URL is the MCP gateway.
// A read that failed arrives as a reason and produces unknown.
func toolSeams(servers []toolServerStatus, reason string) seamPopulation {
	if reason != "" {
		return seamPopulation{State: stateUnknown, Reason: reason}
	}
	population := seamPopulation{State: stateCounted, Total: len(servers)}
	if len(servers) == 0 {
		population.State = stateNone
		return population
	}
	for _, server := range servers {
		if planeHost(server.Spec.URL, planeGatewayService) {
			population.Governed++
		} else {
			population.Direct++
		}
	}
	return population
}

// credentialSeams checks that every Secret the governed seams name is
// actually there. `present` is the list of Secret NAMES in the namespace —
// no value is read, and none is needed to answer this.
func credentialSeams(models []modelStatus, servers []toolServerStatus, present []string, reason string) credentialPopulation {
	if reason != "" {
		return credentialPopulation{State: stateUnknown, Reason: reason}
	}
	required := map[string]bool{}
	for _, model := range models {
		if planeHost(model.Spec.OpenAI.BaseURL, planeProxyService) && model.Spec.APIKeySecret != "" {
			required[model.Spec.APIKeySecret] = true
		}
	}
	for _, server := range servers {
		if !planeHost(server.Spec.URL, planeGatewayService) {
			continue
		}
		for _, header := range server.Spec.HeadersFrom {
			if strings.EqualFold(header.ValueFrom.Type, "Secret") && header.ValueFrom.Name != "" {
				required[header.ValueFrom.Name] = true
			}
		}
	}
	population := credentialPopulation{State: stateCounted, Required: len(required)}
	if len(required) == 0 {
		// A known nothing: no governed seam names a credential. This is
		// `none`, and it is not the same answer as `unknown`.
		population.State = stateNone
		return population
	}
	have := make(map[string]bool, len(present))
	for _, name := range present {
		have[name] = true
	}
	for name := range required {
		if have[name] {
			population.Present++
		} else {
			population.Missing = append(population.Missing, name)
		}
	}
	sort.Strings(population.Missing)
	return population
}

// writeGovernance prints the section. Every line is a count or a stated
// "cannot say"; none of it changes the readiness verdict above it, because
// D36 made the ungoverned fast path the supported one — an ungoverned
// cluster is not a broken cluster, it is an ungoverned one, and status says
// which without calling it a fault.
func writeGovernance(out io.Writer, g governance) {
	fmt.Fprintln(out, "\nGovernance")
	switch g.Plane.State {
	case stateUnknown:
		fmt.Fprintf(out, "  plane:        unknown — %s\n", g.Plane.Reason)
	case stateNone:
		fmt.Fprintln(out, "  plane:        not installed — nothing is enforced in front of these seams (`kmx plane`)")
	default:
		fmt.Fprintf(out, "  plane:        installed (%d/%d pods ready)\n", g.Plane.Ready, g.Plane.Pods)
	}
	fmt.Fprintf(out, "  model seams:  %s\n", seamLine(g.ModelSeams, "agents", "agent"))
	fmt.Fprintf(out, "  tool seams:   %s\n", seamLine(g.ToolSeams, "tool servers", "tool server"))
	fmt.Fprintf(out, "  credentials:  %s\n", credentialLine(g.Credentials))
	fmt.Fprintln(out, "  Governed = the seam points at the plane. It is read from the cluster, so it")
	fmt.Fprintln(out, "  holds with no plane and no network; the plane line says whether one is there.")
}

func seamLine(p seamPopulation, plural, singular string) string {
	switch p.State {
	case stateUnknown:
		return "unknown — " + p.Reason
	case stateNone:
		return fmt.Sprintf("none — no %s on this cluster", plural)
	}
	noun := plural
	if p.Total == 1 {
		noun = singular
	}
	line := fmt.Sprintf("%d of %d %s governed, %d direct", p.Governed, p.Total, noun, p.Direct)
	if p.Unresolved > 0 {
		line += fmt.Sprintf(", %d unknown (%s)", p.Unresolved, p.Reason)
	}
	return line
}

func credentialLine(p credentialPopulation) string {
	switch p.State {
	case stateUnknown:
		return "unknown — " + p.Reason
	case stateNone:
		return "none — no governed seam names one"
	}
	line := fmt.Sprintf("%d of %d present", p.Present, p.Required)
	if len(p.Missing) > 0 {
		line += ", missing: " + strings.Join(p.Missing, ", ")
	}
	return line
}
