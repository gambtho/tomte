package app

// P15: `kmx tools add` — onboarding an MCP server this repo did not
// write.
//
// Before this, every governed agent in the repo was one we shipped, and
// pointing Kaimahi at somebody else's tool server meant hand-writing
// five things that were documented nowhere as a general procedure: a
// NetworkPolicy pair, an entry in the upstream table, a RemoteMCPServer
// whose URL is a string that must be exactly right, a credential, and an
// allowlist. Four of the five are mechanical, and the mechanical ones
// are where the silent failures live — a mistyped gateway URL points an
// agent at a different upstream or at a 404 and says nothing; a policy
// written against a Service port when the container listens on another
// blocks every call while reading as correct.
//
// So kmx owns those, and the operator owns policy: which tools exist,
// and what their arguments MEAN. The artifact is reviewable YAML, as
// `kmx agent create` already established (D19) — kmx scaffolds it, the
// operator reads it, kmx applies it behind the guard.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kaimahi-agents/kaimahi/internal/kmx/admin"
	"github.com/kaimahi-agents/kaimahi/internal/kmx/scaffold"
)

// AddUpstreamOptions is `kmx tools add`'s surface. There is deliberately
// no flag, environment variable or file here that can carry a credential
// (D27): the generator emits Secret REFERENCES, and `kmx tools govern`
// is what mints a token into one.
type AddUpstreamOptions struct {
	Name string
	// URL is the server's own in-cluster MCP endpoint.
	URL string
	// Tools is the repeated --tool value: "<name>:<fields>", "<name>:"
	// for a verb-level binding, "<name>:*" for no declaration.
	Tools []string
	// PodPort overrides the Service's resolved targetPort, for the one
	// case kmx cannot resolve on its own (a NAMED target port).
	PodPort int
	// ServerEgress is one of scaffold.ServerEgressModes.
	ServerEgress string
	// Secret names the agent-side Secret the seam resolves its
	// credential from. A NAME, never a value.
	Secret  string
	Out     string
	NoApply bool
	DryRun  bool
}

// AddUpstream scaffolds, validates and applies one upstream.
func (a *App) AddUpstream(opt AddUpstreamOptions) error {
	if err := scaffold.ValidateUpstreamName(opt.Name); err != nil {
		return err
	}
	tools, err := scaffold.ParseToolDecls(opt.Tools)
	if err != nil {
		return err
	}
	if opt.ServerEgress == "" {
		opt.ServerEgress = scaffold.EgressNone
	}
	switch opt.ServerEgress {
	case scaffold.EgressNone, scaffold.EgressDNS, scaffold.EgressKeep:
	default:
		return fmt.Errorf("--server-egress %q: want one of %s",
			opt.ServerEgress, strings.Join(scaffold.ServerEgressModes, ", "))
	}
	if opt.Secret == "" {
		opt.Secret = "kaimahi-" + opt.Name + "-token"
	}
	// A pure generate must still be a generate: `--out -` implies
	// --no-apply, exactly as `kmx agent create` does.
	if opt.Out == "-" {
		opt.NoApply = true
	}

	svc, ns, port, err := parseUpstreamURL(opt.URL)
	if err != nil {
		return err
	}
	spec := scaffold.UpstreamSpec{
		Name:             opt.Name,
		URL:              opt.URL,
		Service:          svc,
		ServiceNamespace: ns,
		Tools:            tools,
		Secret:           opt.Secret,
		ServerDNS:        opt.ServerEgress == scaffold.EgressDNS,
		ServerEgressKeep: opt.ServerEgress == scaffold.EgressKeep,
	}
	// The pod selector and the container port come from the LIVE
	// Service, never from its name: those two values are what the
	// NetworkPolicy pair is pinned to, and a guess that is wrong fails
	// silently in the direction of blocking everything.
	if err := a.resolveService(&spec, opt.PodPort, port); err != nil {
		return err
	}
	frag, err := spec.Fragment()
	if err != nil {
		return err
	}
	// The overlay ConfigMap is emitted WHOLE — every fragment already on
	// the cluster plus this one — because a ConfigMap apply replaces
	// `data`, so an emitted map missing an existing key would silently
	// un-onboard somebody else's server.
	spec.Fragments, err = a.readOverlay()
	if err != nil {
		return err
	}
	if _, exists := spec.Fragments[spec.FragmentKey()]; exists {
		return fmt.Errorf("upstream %q is already in the overlay (%s key %q).\n"+
			"  Read it back with:\n"+
			"    kubectl --context %s -n %s get configmap %s -o jsonpath='{.data.%s}'\n"+
			"  and remove it deliberately if you mean to replace it.",
			opt.Name, scaffold.OverlayConfigMap, spec.FragmentKey(),
			a.Cfg.KubeContext, scaffold.PlaneNamespace, scaffold.OverlayConfigMap, spec.FragmentKey())
	}
	spec.Fragments[spec.FragmentKey()] = frag

	document, err := scaffold.GenerateUpstream(spec)
	if err != nil {
		return err
	}

	// Validate BEFORE anything is written or applied, and validate with
	// the plane's own parser rather than a second copy of it: the
	// candidate overlay goes to the running proxy, which merges it over
	// the committed table and calls the same config.Parse it booted
	// with. If it says yes, the pod that reads this will boot.
	if err := a.validateOverlay(spec.Fragments); err != nil {
		return err
	}

	if opt.Out == "-" {
		_, err := a.Out.Write([]byte(document))
		return err
	}
	path := opt.Out
	if path == "" {
		path = filepath.Join("upstreams", opt.Name+".yaml")
	}
	if err := scaffold.WriteNew(path, document); err != nil {
		return err
	}
	a.notef("Wrote %s — four documents: the overlay fragment, the proxy's egress to this server,", path)
	a.notef("this server's ingress from the proxy alone, and the RemoteMCPServer whose URL is the gateway.")
	for _, t := range tools {
		if t.VerbLevel() {
			a.notef("WARNING: %s declares `policy_fields: []` — a VERB-LEVEL binding, the weakest setting.", t.Name)
			a.notef("  An approval for it covers ANY arguments until the grant is spent, and the audit names only the verb.")
		}
	}
	if opt.NoApply {
		a.notef("Not applied (--no-apply). Review it, then:")
		a.notef("  kubectl --context %s apply -f %s", a.Cfg.KubeContext, path)
		return nil
	}

	if err := a.Guard(fmt.Sprintf("onboard MCP upstream %q from %s", opt.Name, path),
		"kmx tools add "+opt.Name); err != nil {
		return err
	}
	if opt.DryRun {
		return a.kubectlRun("apply", "--dry-run=server", "-f", path)
	}
	if err := a.kubectlRun("apply", "-f", path); err != nil {
		return err
	}
	// The gateway reads its table at boot and the overlay mounts by
	// directory, so the entry is not live until the proxy restarts.
	if err := a.rollProxy(); err != nil {
		return err
	}
	a.notef("")
	a.notef("Upstream %q is in the table and reachable. It has no credential yet, so nothing can call it.", opt.Name)
	a.notef("Issue one and point an agent at it:")
	a.notef("  kmx tools govern --server %s --secret %s \\", spec.ServerName(), opt.Secret)
	a.notef("      --credential <credential> --agent <agent> --tools <the tools that agent may call>")
	return nil
}

// parseUpstreamURL takes the server's own MCP endpoint apart. The host
// has to be in-cluster shaped, which is the same rule the plane applies
// at load — checked here so the message names the URL rather than
// arriving later as a config refusal.
func parseUpstreamURL(raw string) (service, namespace string, port int, err error) {
	u, parseErr := url.Parse(raw)
	if parseErr != nil || u.Scheme != "http" || u.Host == "" || u.Path == "" {
		return "", "", 0, fmt.Errorf("--url %q: want the server's own in-cluster endpoint, "+
			"e.g. http://<service>.<namespace>:<port>/mcp", raw)
	}
	if u.User != nil {
		return "", "", 0, fmt.Errorf("--url %q: a URL carrying credentials is refused", raw)
	}
	host := u.Hostname()
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	} else {
		port = 80
	}
	host = strings.TrimSuffix(host, ".svc.cluster.local")
	host = strings.TrimSuffix(host, ".svc")
	parts := strings.Split(host, ".")
	switch len(parts) {
	case 1:
		// A bare Service name resolves in the caller's own namespace,
		// and the caller here is the proxy. Stated rather than assumed.
		return parts[0], scaffold.PlaneNamespace, port, nil
	case 2:
		return parts[0], parts[1], port, nil
	default:
		return "", "", 0, fmt.Errorf("--url %q: %q is not an in-cluster Service name "+
			"(want <service>, <service>.<namespace> or <service>.<namespace>.svc.cluster.local). "+
			"A server outside the cluster is the hosted path — see docs/hosted-upstreams.md", raw, u.Hostname())
	}
}

// resolveService reads the Service the URL names and takes from it the
// two facts a NetworkPolicy needs and a name cannot supply: the labels
// that actually route to those pods, and the port those pods listen on.
func (a *App) resolveService(spec *scaffold.UpstreamSpec, override, urlPort int) error {
	out, err := a.kubectlCapture("-n", spec.ServiceNamespace, "get", "service", spec.Service, "-o", "json")
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("no Service %q in namespace %q.\n"+
				"  kmx reads the Service to learn which pods the NetworkPolicy must name and which port they listen on;\n"+
				"  neither can be guessed from a URL. Deploy the server first, then onboard it.",
				spec.Service, spec.ServiceNamespace)
		}
		return err
	}
	var svc struct {
		Spec struct {
			Selector map[string]string `json:"selector"`
			Ports    []struct {
				Port       int             `json:"port"`
				TargetPort json.RawMessage `json:"targetPort"`
				Protocol   string          `json:"protocol"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &svc); err != nil {
		return fmt.Errorf("reading Service %s/%s: %w", spec.ServiceNamespace, spec.Service, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return fmt.Errorf("Service %s/%s has no selector.\n"+
			"  A NetworkPolicy pinned to no labels selects every pod in the namespace, which is not a boundary.\n"+
			"  Onboard a Service that selects its own pods, or write the pair by hand from docs/govern-your-agent.md.",
			spec.ServiceNamespace, spec.Service)
	}
	spec.PodLabels = svc.Spec.Selector

	if override > 0 {
		spec.PodPort = override
		return nil
	}
	for _, p := range svc.Spec.Ports {
		if p.Port != urlPort {
			continue
		}
		if p.Protocol != "" && p.Protocol != "TCP" {
			return fmt.Errorf("Service %s/%s port %d is %s; the MCP seam is TCP",
				spec.ServiceNamespace, spec.Service, urlPort, p.Protocol)
		}
		// NetworkPolicy is evaluated on the post-NAT POD address, so the
		// rule needs the CONTAINER port. An unset targetPort defaults to
		// the Service port; a NAMED one resolves per pod and kmx will
		// not guess which number it is.
		var num int
		if len(p.TargetPort) > 0 && string(p.TargetPort) != "null" {
			if err := json.Unmarshal(p.TargetPort, &num); err != nil {
				var name string
				_ = json.Unmarshal(p.TargetPort, &name)
				return fmt.Errorf("Service %s/%s port %d targets the NAMED port %q.\n"+
					"  A NetworkPolicy needs the number the container listens on; kmx will not guess it.\n"+
					"  Name it: kmx tools add … --pod-port <number>",
					spec.ServiceNamespace, spec.Service, urlPort, name)
			}
		}
		if num == 0 {
			num = p.Port
		}
		spec.PodPort = num
		return nil
	}
	return fmt.Errorf("Service %s/%s publishes no port %d (the port in --url)",
		spec.ServiceNamespace, spec.Service, urlPort)
}

// readOverlay returns the fragments the cluster's overlay ConfigMap
// already holds. An absent ConfigMap is an empty overlay — the state of
// every cluster where nobody has onboarded anything.
func (a *App) readOverlay() (map[string]string, error) {
	out, err := a.kubectlCapture("-n", scaffold.PlaneNamespace, "get", "configmap",
		scaffold.OverlayConfigMap, "-o", "jsonpath={.data}")
	if err != nil {
		if isNotFound(err) {
			return map[string]string{}, nil
		}
		// Anything but a genuine NotFound — an unreachable API server,
		// an RBAC denial, the wrong context — must NOT read as "the
		// overlay is empty": that would emit a ConfigMap dropping every
		// upstream somebody else onboarded.
		return nil, fmt.Errorf("reading the overlay ConfigMap %s: %w", scaffold.OverlayConfigMap, err)
	}
	data := map[string]string{}
	out = strings.TrimSpace(out)
	if out == "" {
		return data, nil
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return nil, fmt.Errorf("reading the overlay ConfigMap %s: %w", scaffold.OverlayConfigMap, err)
	}
	return data, nil
}

// validateOverlay asks the RUNNING plane whether this overlay would
// load. Nothing is stored and nothing changes; this is a read.
func (a *App) validateOverlay(fragments map[string]string) error {
	return a.session(func(c *admin.Client) error {
		body := map[string]any{"fragments": map[string]json.RawMessage{}}
		frags := body["fragments"].(map[string]json.RawMessage)
		for name, raw := range fragments {
			frags[name] = json.RawMessage(raw)
		}
		status, out, err := c.Do("POST", "/admin/config/validate", body)
		if err != nil {
			return err
		}
		var resp struct {
			OK            bool                `json:"ok"`
			Error         string              `json:"error"`
			ToolUpstreams []string            `json:"tool_upstreams"`
			Declared      map[string][]string `json:"declared"`
		}
		_ = json.Unmarshal(out, &resp)
		if status != 200 || !resp.OK {
			msg := resp.Error
			if msg == "" {
				msg = strings.TrimSpace(string(out))
			}
			return fmt.Errorf("the plane refused this upstream table — nothing has been applied:\n  %s", msg)
		}
		a.notef("The plane validated the table: %s.", strings.Join(resp.ToolUpstreams, ", "))
		for tool, fields := range resp.Declared {
			if len(fields) == 0 {
				a.notef("  %s: verb-level binding (no argument is policy-relevant).", tool)
				continue
			}
			a.notef("  %s: an approval binds %s.", tool, strings.Join(fields, ", "))
		}
		return nil
	})
}

// rollProxy restarts the plane so the merged table is the one being
// enforced. maxUnavailable: 0 means a table the new pod refuses leaves
// the old replicas serving — but validateOverlay has already asked the
// same parser, so this should never be the thing that discovers it.
func (a *App) rollProxy() error {
	if err := a.kubectlRun("-n", scaffold.PlaneNamespace, "rollout", "restart", "deploy/kaimahi-proxy"); err != nil {
		return err
	}
	return a.kubectlRun("-n", scaffold.PlaneNamespace, "rollout", "status",
		"deploy/kaimahi-proxy", "--timeout=300s")
}
