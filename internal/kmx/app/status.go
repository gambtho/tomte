package app

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// statusRequestTimeout bounds every read status makes.
//
// Re-landed from #37, whose reasoning holds and had nowhere to live once
// scripts/status.py went: this is the command people run when something is
// wrong, and an API server that is unreachable rather than refusing leaves a
// bare `kubectl get` waiting on TCP. A status command that hangs is the same
// failure as one that needs the thing being diagnosed.
const statusRequestTimeout = "--request-timeout=15s"

// StatusOptions controls the human table or the structured document.
type StatusOptions struct {
	Output string
}

type objectList[T any] struct {
	Items []T `json:"items"`
}

type statusCondition struct {
	Type, Status string
}

type agentStatus struct {
	Metadata struct{ Name string } `json:"metadata"`
	Spec     struct {
		Declarative struct {
			ModelConfig string `json:"modelConfig"`
			Tools       []struct {
				MCPServer struct{ Name string } `json:"mcpServer"`
			} `json:"tools"`
		} `json:"declarative"`
	} `json:"spec"`
	Status struct{ Conditions []statusCondition } `json:"status"`
}

type modelStatus struct {
	Metadata struct{ Name string } `json:"metadata"`
	Spec     struct {
		Provider, Model string
		// The governed presets differ from the direct ones in exactly one
		// place: baseUrl is the plane's proxy. That is what makes the model
		// seam classifiable from the cluster alone.
		OpenAI struct {
			BaseURL string `json:"baseUrl"`
		} `json:"openAI"`
		APIKeySecret string `json:"apiKeySecret"`
	} `json:"spec"`
	Status struct{ Conditions []statusCondition } `json:"status"`
}

// toolServerStatus is the tool seam: a RemoteMCPServer's URL is either the
// plane's MCP gateway or it is not, and headersFrom names the Secret the
// governed one authenticates with.
type toolServerStatus struct {
	Metadata struct{ Name string } `json:"metadata"`
	Spec     struct {
		URL         string `json:"url"`
		HeadersFrom []struct {
			ValueFrom struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"valueFrom"`
		} `json:"headersFrom"`
	} `json:"spec"`
}

type podStatus struct {
	Metadata struct{ Name string } `json:"metadata"`
	Status   struct {
		Phase             string
		Conditions        []statusCondition
		ContainerStatuses []struct {
			RestartCount int `json:"restartCount"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func condition(conditions []statusCondition, name string) string {
	for _, value := range conditions {
		if value.Type == name {
			if value.Status == "True" {
				return "yes"
			}
			return "no"
		}
	}
	return "-"
}

func table(out io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = len(header)
	}
	for _, row := range rows {
		for i, value := range row {
			if len(value) > widths[i] {
				widths[i] = len(value)
			}
		}
	}
	for rowIndex, row := range append([][]string{headers}, rows...) {
		fmt.Fprint(out, "  ")
		for i, value := range row {
			if i > 0 {
				fmt.Fprint(out, "  ")
			}
			fmt.Fprintf(out, "%-*s", widths[i], value)
		}
		if rowIndex < len(rows) {
			fmt.Fprintln(out)
		}
	}
	fmt.Fprintln(out)
}

// statusTolerant reads a population that may not exist on this cluster at
// all: a CRD kagent has not installed, a namespace that was never created,
// an RBAC denial. It returns a REASON rather than an error, and never turns
// a failure into an empty list — the caller reports `unknown`, which is a
// different answer from "0" and the one W30 fixed the word for.
//
// The reason is the first line of kubectl's own complaint, so an operator
// reads what kubectl said rather than a paraphrase of it.
func (a *App) statusTolerant(namespace, resource string, target any) string {
	raw, err := a.kubectlCapture("-n", namespace, "get", resource, "-o", "json", statusRequestTimeout)
	if err == nil {
		if err := json.Unmarshal([]byte(raw), target); err != nil {
			return firstLine(err.Error())
		}
		return ""
	}
	if isNotFound(err) {
		// The namespace or the resource is genuinely absent, which for
		// these reads means an empty population rather than a mystery.
		return ""
	}
	return firstLine(err.Error())
}

func firstLine(message string) string {
	message = strings.TrimSpace(message)
	if i := strings.IndexByte(message, '\n'); i >= 0 {
		message = strings.TrimSpace(message[:i])
	}
	return message
}

// secretNames lists the Secret NAMES in a namespace. Names only: `-o name`
// returns metadata, never a value, and status reads no Secret value
// anywhere. This exists so a governed seam whose token Secret is missing is
// reported as missing instead of failing at the next call.
func (a *App) secretNames(namespace string) ([]string, string) {
	raw, err := a.kubectlCapture("-n", namespace, "get", "secrets", "-o", "name", statusRequestTimeout)
	if err != nil {
		if isNotFound(err) {
			return nil, ""
		}
		return nil, firstLine(err.Error())
	}
	var names []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		names = append(names, strings.TrimPrefix(line, "secret/"))
	}
	return names, ""
}

func (a *App) statusJSON(namespace, resource string, optional bool, target any) error {
	raw, err := a.kubectlCapture("-n", namespace, "get", resource, "-o", "json", statusRequestTimeout)
	if err != nil {
		if optional && isNotFound(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal([]byte(raw), target)
}

func podSummary(pods []podStatus) (ready, restarts int, rows [][]string) {
	sort.Slice(pods, func(i, j int) bool { return pods[i].Metadata.Name < pods[j].Metadata.Name })
	for _, pod := range pods {
		isReady := condition(pod.Status.Conditions, "Ready") == "yes"
		if isReady {
			ready++
		}
		podRestarts := 0
		for _, container := range pod.Status.ContainerStatuses {
			podRestarts += container.RestartCount
		}
		restarts += podRestarts
		rows = append(rows, []string{pod.Metadata.Name, map[bool]string{true: "yes", false: "no"}[isReady], pod.Status.Phase, fmt.Sprint(podRestarts)})
	}
	return
}

func statusReady(allAgents, allModels bool, kReady, kTotal, oReady, oTotal, pReady, pTotal int) bool {
	ready := allAgents && allModels && kTotal > 0 && kReady == kTotal
	if oTotal > 0 {
		ready = ready && oReady == oTotal
	}
	if pTotal > 0 {
		ready = ready && pReady == pTotal
	}
	return ready
}

// Status prints a grouped human view or kubectl-native JSON/YAML.
func (a *App) StatusWithOptions(opt StatusOptions) error {
	format := strings.ToLower(strings.TrimSpace(opt.Output))
	if format != "" && format != "table" && format != "json" && format != "yaml" {
		return fmt.Errorf("status output %q is not supported — use table, json, or yaml", opt.Output)
	}
	if err := a.preflight(depKubectl); err != nil {
		return err
	}
	if format == "" || format == "table" {
		return a.statusTable()
	}
	return a.statusStructured(format)
}

// statusDocument is what `kmx status -o json|yaml` publishes.
//
// CHANGED, deliberately: this output used to be kubectl's own List, and the
// governance count has to survive automation or it is only a message on a
// screen — the same reasoning as #71, where the machine-readable half was
// the load-bearing one. The kubectl objects are still here, VERBATIM, under
// `items`, so the idiom that reads them (`jq '.items[]'`) is unchanged; what
// is new is the envelope around them.
type statusDocument struct {
	Context       string     `json:"context"`
	ContextSource string     `json:"contextSource"`
	Governance    governance `json:"governance"`
	Items         []any      `json:"items"`
}

func (a *App) statusStructured(format string) error {
	data, err := a.collectStatus()
	if err != nil {
		return err
	}
	// The objects are re-read through the SAME combined get the old output
	// used, so `items` is byte-for-byte what kubectl would have printed
	// rather than kmx's narrower view of the same objects.
	raw, err := a.kubectlCapture("-n", "kagent", "get", "agents,modelconfigs,pods", "-o", "json", statusRequestTimeout)
	if err != nil {
		return err
	}
	var list struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return err
	}
	if list.Items == nil {
		// An empty cluster publishes `[]`, not `null`: a consumer that
		// iterates items should find nothing there, not fall over.
		list.Items = []any{}
	}
	document := statusDocument{
		Context:       a.Cfg.KubeContext,
		ContextSource: a.Cfg.ContextSource,
		Governance:    data.governance(),
		Items:         list.Items,
	}
	var encoded []byte
	if format == "yaml" {
		// json.Marshal first so the struct tags decide the field names
		// once: one shape, two encodings.
		intermediate, err := json.Marshal(document)
		if err != nil {
			return err
		}
		var generic any
		if err := json.Unmarshal(intermediate, &generic); err != nil {
			return err
		}
		encoded, err = yaml.Marshal(generic)
		if err != nil {
			return err
		}
	} else {
		encoded, err = json.MarshalIndent(document, "", "  ")
		if err != nil {
			return err
		}
		encoded = append(encoded, '\n')
	}
	_, err = a.Out.Write(encoded)
	return err
}

func (a *App) Status() error { return a.StatusWithOptions(StatusOptions{}) }

// statusData is everything one `kmx status` reads, gathered once so the
// human table and the structured document are the same facts.
type statusData struct {
	agents     objectList[agentStatus]
	models     objectList[modelStatus]
	servers    objectList[toolServerStatus]
	kagentPods objectList[podStatus]
	ollamaPods objectList[podStatus]
	planePods  objectList[podStatus]
	secrets    []string
	serverErr  string
	planeErr   string
	secretErr  string
}

func (a *App) collectStatus() (*statusData, error) {
	d := &statusData{}
	if err := a.statusJSON("kagent", "agents", false, &d.agents); err != nil {
		return nil, err
	}
	if err := a.statusJSON("kagent", "modelconfigs", false, &d.models); err != nil {
		return nil, err
	}
	if err := a.statusJSON("kagent", "pods", false, &d.kagentPods); err != nil {
		return nil, err
	}
	if err := a.statusJSON("ollama", "pods", true, &d.ollamaPods); err != nil {
		return nil, err
	}
	// The plane, the tool servers and the Secret names are read
	// TOLERANTLY: none of them exists on the fast path D36 made the
	// default, and a status command that fails because the thing it is
	// diagnosing is absent is worthless. Each failure becomes a stated
	// `unknown`, never a silent zero.
	d.planeErr = a.statusTolerant("kaimahi", "pods", &d.planePods)
	d.serverErr = a.statusTolerant("kagent", "remotemcpservers", &d.servers)
	d.secrets, d.secretErr = a.secretNames("kagent")
	return d, nil
}

// governance assembles the three counts and the plane's presence.
func (d *statusData) governance() governance {
	plane := planePresence{State: stateNone}
	switch {
	case d.planeErr != "":
		plane = planePresence{State: stateUnknown, Reason: d.planeErr}
	case len(d.planePods.Items) > 0:
		ready, _, _ := podSummary(append([]podStatus(nil), d.planePods.Items...))
		plane = planePresence{State: stateInstalled, Ready: ready, Pods: len(d.planePods.Items)}
	}
	return governance{
		Plane:       plane,
		ModelSeams:  modelSeams(d.agents.Items, d.models.Items),
		ToolSeams:   toolSeams(d.servers.Items, d.serverErr),
		Credentials: credentialSeams(d.models.Items, d.servers.Items, d.secrets, credentialReason(d)),
	}
}

// credentialReason keeps the credential count honest about its inputs: the
// Secret listing is only half of it, and a tool-server read that failed
// means the required set itself is unknown.
func credentialReason(d *statusData) string {
	if d.secretErr != "" {
		return d.secretErr
	}
	if d.serverErr != "" {
		return "the tool seams could not be read, so what they require is unknown"
	}
	return ""
}

func (a *App) statusTable() error {
	data, err := a.collectStatus()
	if err != nil {
		return err
	}
	agents, models := data.agents, data.models
	kagentPods, ollamaPods, planePods := data.kagentPods, data.ollamaPods, data.planePods

	agentRows := make([][]string, 0, len(agents.Items))
	allAgents := len(agents.Items) > 0
	for _, agent := range agents.Items {
		servers := make([]string, 0, len(agent.Spec.Declarative.Tools))
		for _, tool := range agent.Spec.Declarative.Tools {
			if tool.MCPServer.Name != "" {
				servers = append(servers, tool.MCPServer.Name)
			}
		}
		ready, accepted := condition(agent.Status.Conditions, "Ready"), condition(agent.Status.Conditions, "Accepted")
		allAgents = allAgents && ready == "yes" && accepted == "yes"
		agentRows = append(agentRows, []string{agent.Metadata.Name, ready, accepted, agent.Spec.Declarative.ModelConfig, valueOr(strings.Join(servers, ","), "none")})
	}
	sort.Slice(agentRows, func(i, j int) bool { return agentRows[i][0] < agentRows[j][0] })

	modelRows := make([][]string, 0, len(models.Items))
	allModels := len(models.Items) > 0
	for _, model := range models.Items {
		accepted := condition(model.Status.Conditions, "Accepted")
		allModels = allModels && accepted == "yes"
		modelRows = append(modelRows, []string{model.Metadata.Name, model.Spec.Provider, model.Spec.Model, accepted})
	}
	sort.Slice(modelRows, func(i, j int) bool { return modelRows[i][0] < modelRows[j][0] })

	kReady, kRestarts, podRows := podSummary(kagentPods.Items)
	oReady, oRestarts, _ := podSummary(ollamaPods.Items)
	pReady, pRestarts, _ := podSummary(planePods.Items)
	overall := statusReady(allAgents, allModels,
		kReady, len(kagentPods.Items), oReady, len(ollamaPods.Items), pReady, len(planePods.Items))

	fmt.Fprintln(a.Out, "Kaimahi status")
	fmt.Fprintf(a.Out, "  context: %s (from %s)\n", a.Cfg.KubeContext, a.Cfg.ContextSource)
	if overall {
		fmt.Fprintf(a.Out, "  result:  ready (%d agents available)\n", len(agents.Items))
	} else {
		fmt.Fprintln(a.Out, "  result:  attention required")
	}
	fmt.Fprintln(a.Out, "\nAgents")
	table(a.Out, []string{"NAME", "READY", "ACCEPTED", "MODEL CONFIG", "TOOL SERVER"}, agentRows)
	fmt.Fprintln(a.Out, "  Ready = can serve requests; Accepted = kagent accepted the configuration.")
	fmt.Fprintln(a.Out, "\nModels")
	table(a.Out, []string{"CONFIG", "PROVIDER", "MODEL", "ACCEPTED"}, modelRows)
	fmt.Fprintln(a.Out, "\nRuntime")
	fmt.Fprintf(a.Out, "  kagent:     %d/%d pods ready, %d restarts\n", kReady, len(kagentPods.Items), kRestarts)
	if len(ollamaPods.Items) > 0 {
		fmt.Fprintf(a.Out, "  ollama:     %d/%d pods ready, %d restarts\n", oReady, len(ollamaPods.Items), oRestarts)
	} else {
		fmt.Fprintln(a.Out, "  ollama:     not installed")
	}
	switch {
	case data.planeErr != "":
		// Not "not installed": we could not look. Saying the plane is
		// absent here would be the same false zero the governance counts
		// below refuse to print.
		fmt.Fprintf(a.Out, "  governance: unknown — %s\n", data.planeErr)
	case len(planePods.Items) > 0:
		fmt.Fprintf(a.Out, "  governance: %d/%d pods ready, %d restarts\n", pReady, len(planePods.Items), pRestarts)
	default:
		fmt.Fprintln(a.Out, "  governance: not installed (run `kmx plane` for budgets and audit)")
	}
	fmt.Fprintln(a.Out, "\nRuntime pods")
	table(a.Out, []string{"NAME", "READY", "PHASE", "RESTARTS"}, podRows)
	writeGovernance(a.Out, data.governance())
	fmt.Fprintln(a.Out, "\nNext")
	if overall {
		fmt.Fprintf(a.Out, "  kmx agent chat %s\n", agentRows[0][0])
	} else {
		fmt.Fprintf(a.Out, "  kubectl --context %s -n kagent get agents,pods\n", a.Cfg.KubeContext)
	}
	return nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
