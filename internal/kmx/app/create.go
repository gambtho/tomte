package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kaimahi-agents/kaimahi/internal/kmx/config"
	"github.com/kaimahi-agents/kaimahi/internal/kmx/scaffold"
)

// CreateOptions are `kmx agent create`'s flags. There is deliberately no
// option here that could carry a credential, in any form — no flag, no
// environment variable, no file. The generator emits Secret REFERENCES; a
// scaffolder that can take a key is a scaffolder that can leak one into a
// file you are about to commit.
type CreateOptions struct {
	Name         string
	Namespace    string
	Description  string
	ModelConfig  string // empty: resolved against the cluster
	Instructions string // path to a file
	Tools        string // server:tool1,tool2
	Out          string // empty: agents/<name>.yaml
	NoApply      bool
	DryRun       bool
	Image        string // non-empty: a BYO agent serving A2A on :8080
	Isolation    string // placement profile, or "none"
}

// CreateAgent scaffolds an agent, then applies it.
//
// The YAML is the artifact and it is written first, so the operator ends up
// with the file whether or not the cluster accepts it. Applying goes through
// the same context guard as every other mutation.
func (a *App) CreateAgent(opt CreateOptions) error {
	if err := scaffold.ValidateName(opt.Name); err != nil {
		return err
	}
	tools, err := scaffold.ParseTools(opt.Tools)
	if err != nil {
		return err
	}
	instructions := ""
	if opt.Instructions != "" {
		body, err := os.ReadFile(opt.Instructions)
		if err != nil {
			return fmt.Errorf("cannot read the instructions file: %w", err)
		}
		instructions = string(body)
	}

	namespace := opt.Namespace
	if namespace == "" {
		namespace = config.DefaultNamespace
	}
	// `--out -` prints and stops, so it is the same kind of action as
	// --no-apply: a pure generate that must still work with no cluster in
	// reach (a GitOps checkout, say).
	if opt.Out == "-" {
		opt.NoApply = true
	}

	modelConfig, governed, err := a.resolveModelConfig(opt, namespace)
	if err != nil {
		return err
	}

	placement, err := scaffold.ParsePlacement(opt.Isolation)
	if err != nil {
		return err
	}
	// A BYO image has no modelConfig and no tools to reference, so the seams
	// a declarative agent gets by reference are carried across as env.
	var governance []scaffold.EnvVar
	if opt.Image != "" {
		governance = scaffold.GovernanceEnv(governed)
	}

	document, err := scaffold.Generate(scaffold.Spec{
		Name:         opt.Name,
		Namespace:    namespace,
		Description:  opt.Description,
		ModelConfig:  modelConfig,
		Instructions: instructions,
		Tools:        tools,
		Governed:     governed,
		Image:        opt.Image,
		Placement:    placement,
		Governance:   governance,
	})
	if err != nil {
		return err
	}

	path := opt.Out
	if path == "" {
		path = filepath.Join("agents", opt.Name+".yaml")
	}
	if path == "-" {
		fmt.Fprint(a.Out, document)
		return nil
	}
	if err := scaffold.WriteNew(path, document); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "wrote %s\n", path)

	if opt.Image != "" {
		// The BYO cliff, said out loud. spec.byo has no modelConfig and no
		// tools, so what a declarative agent gets by reference is now
		// environment — and kmx cannot check the image reads it.
		a.notef("BYO agent: kagent will deploy %s and expect A2A on :8080.\n"+
			"         It has no modelConfig and no tools field — those exist only on\n"+
			"         declarative agents — so the governed seams travel as env instead.", opt.Image)
		if len(governance) > 0 {
			a.notef("Injected the governed seams into the pod's env:")
			for _, e := range governance {
				if e.SecretRef != "" {
					a.notef("           %s <- secret %s/%s", e.Name, e.SecretRef, e.Value)
					continue
				}
				a.notef("           %s = %s", e.Name, e.Value)
			}
			a.notef("CONFIGURED, NOT PROVEN: kmx cannot verify the image honours these.\n" +
				"         `kmx ledger` is the evidence — a row there means it did.")
		}
		if placement != nil {
			a.notef("%s", placement.Note)
		} else {
			a.notef("No placement profile: this pod schedules like any other. `--isolation\n" +
				"         virtual-node` puts it on an ACI virtual node instead.")
		}
	}

	if !governed {
		a.notef("WARNING: %q is ungoverned — no budget, no ledger, no audit in front of it.\n"+
			"         `make plane` then `make govern` puts the plane in front of an agent.", modelConfig)
	}
	if tools == nil {
		a.notef("This agent has no tools. Add an allowlisted MCP wiring with:\n" +
			"  kmx agent create <name> --tools <server>:<tool>[,<tool>...]")
	} else {
		a.notef("Tool calls are allowlisted at the AGENT, not at a gateway, until the\n" +
			"plane fronts this server (`make govern-tools`): there is no audit trail yet.")
	}

	if opt.NoApply {
		a.notef("\nNot applied (--no-apply). Review it, then:\n  kubectl --context %s apply -f %s", a.Cfg.KubeContext, path)
		return nil
	}

	if err := a.Guard("apply agent "+opt.Name+" from "+path, "kmx agent create "+opt.Name); err != nil {
		return err
	}
	if err := a.preflightModelConfig(modelConfig, namespace); err != nil {
		return err
	}
	if opt.DryRun {
		return a.kubectlRun("apply", "--dry-run=server", "-f", path)
	}
	if err := a.kubectlRun("apply", "-f", path); err != nil {
		return err
	}
	return a.waitAgentReady(opt.Name)
}

// resolveModelConfig decides what the agent thinks with.
//
// Governed by default WHERE A PLANE EXISTS: if the plane's governed preset is
// on the cluster, the scaffolded agent is metered, budgeted and ledgered from
// its first call. On a fresh `kmx up` cluster there is no plane (milestone 1
// does not deploy one, D27), so the keyless in-cluster preset is used and the
// ungoverned warning is printed. An explicit --model always wins.
// An unreachable cluster is NOT "no plane". Getting that wrong is the whole
// failure mode this project exists to prevent: a governed agent silently
// scaffolded onto the keyless preset, with no budget, no ledger and no audit,
// because an API call blipped. So the read distinguishes a genuine NotFound
// from any other failure, and only the former means "no plane here".
func (a *App) resolveModelConfig(opt CreateOptions, namespace string) (string, bool, error) {
	if opt.ModelConfig != "" {
		return opt.ModelConfig, opt.ModelConfig == config.GovernedModelConfig, nil
	}
	governed, err := a.modelConfigExists(config.GovernedModelConfig, namespace)
	switch {
	case err != nil && !opt.NoApply:
		return "", false, err
	case err != nil:
		// Scaffolding to a file without applying is a legitimate offline
		// action; say plainly that the governance choice was a guess.
		a.notef("NOTE: could not ask the cluster whether a governed preset exists (%v).\n"+
			"      Scaffolding against %q — check the modelConfig before you apply this.",
			err, config.KeylessModelConfig)
		return config.KeylessModelConfig, false, nil
	case governed:
		return config.GovernedModelConfig, true, nil
	}
	return config.KeylessModelConfig, false, nil
}

func (a *App) modelConfigExists(name, namespace string) (bool, error) {
	_, err := a.kubectlCapture("-n", namespace, "get", "modelconfig", name, "-o", "name")
	switch {
	case err == nil:
		return true, nil
	case isNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("cannot read modelconfig %q in namespace %s — refusing to guess whether this cluster has a governance plane: %w",
			name, namespace, err)
	}
}

// preflightModelConfig checks the ModelConfig before applying.
//
// A missing ModelConfig is ADMITTED by the API server and then fails to
// reconcile in silence: the Agent exists, never becomes Ready, and nothing
// says why. Check first, and print the fix (#16 hit exactly this).
func (a *App) preflightModelConfig(name, namespace string) error {
	exists, err := a.modelConfigExists(name, namespace)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	extra := ""
	if name == config.KeylessModelConfig {
		extra = "\n  On a fresh machine that is `kmx up`."
	}
	if name == config.GovernedModelConfig {
		extra = "\n  The governed presets come with the plane: `make plane` then `make govern`."
	}
	return fmt.Errorf("ModelConfig %q does not exist in namespace %s.\n"+
		"  The API server would accept the Agent and then never reconcile it, silently.\n"+
		"  Existing presets:  kubectl --context %s -n %s get modelconfigs%s",
		name, namespace, a.Cfg.KubeContext, namespace, extra)
}

// RefuseUnknownAgentVerb is the CRUD boundary made concrete: `create` is the
// only letter with a real gap, and R/U/D print the tool that already does the
// job rather than growing a worse copy of it.
func RefuseUnknownAgentVerb(verb, kubeContext string) error {
	ctx := ""
	if kubeContext != "" {
		ctx = " --context " + kubeContext
	}
	return fmt.Errorf("kmx: unknown command 'agent %s'.\n"+
		"Only 'agent create' and 'agent chat' exist. Reading, updating and deleting\n"+
		"agents is kubectl and the kagent CLI's job:\n"+
		"  kubectl%s -n kagent get agents\n"+
		"  kubectl%s -n kagent edit agent <name>\n"+
		"  kubectl%s -n kagent delete agent <name>\n"+
		"  kubectl%s apply -f agents/<name>.yaml",
		verb, ctx, ctx, ctx, ctx)
}
