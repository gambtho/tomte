package proxy

// P15: validating an overlay BEFORE it is applied.
//
// Until now a malformed upstream entry was discovered by applying it,
// rolling the proxy, and watching the new pod refuse to boot — a real
// control (maxUnavailable: 0 keeps the old replicas serving) but a poor
// one to learn from, because it happens after the mutation, in a
// rollout, to an operator who is no longer looking.
//
// This endpoint moves the same decision earlier WITHOUT writing a second
// validator: it merges the candidate overlay over the base table the
// running proxy booted from and calls config.Parse — the one function
// `main` loads with. If Parse accepts it here, the pod that reads it
// will accept it too, because it is literally the same code in the same
// binary. Nothing is stored and nothing is changed; this is a read.

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/kaimahi-agents/kaimahi/plane/internal/config"
)

// maxOverlayBytes bounds the submitted overlay. The committed table is
// ~4KB; an overlay is one entry per onboarded server.
const maxOverlayBytes = 256 << 10

type validateRequest struct {
	// Fragments is filename -> the fragment's JSON, exactly as the
	// overlay ConfigMap would carry it.
	Fragments map[string]json.RawMessage `json:"fragments"`
}

type validateResponse struct {
	OK bool `json:"ok"`
	// Error is Parse's own message, verbatim — the same line the pod
	// would have logged before exiting.
	Error string `json:"error,omitempty"`
	// ToolUpstreams is every tool upstream the merged table would carry,
	// so an operator can see their entry take its place beside the
	// committed ones rather than in place of one.
	ToolUpstreams []string `json:"tool_upstreams,omitempty"`
	// Declared echoes back what the plane understood each newly declared
	// tool's policy-relevant fields to be. An empty list is a real
	// answer (a verb-level binding); a tool absent from this map binds
	// its whole canonical argument object.
	Declared map[string][]string `json:"declared,omitempty"`
}

func (h *handler) validateConfig(w http.ResponseWriter, r *http.Request) {
	var req validateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOverlayBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, validateResponse{Error: "malformed request: " + err.Error()})
		return
	}
	frags := make([]config.Fragment, 0, len(req.Fragments))
	for name, raw := range req.Fragments {
		frags = append(frags, config.Fragment{Name: name, Raw: raw})
	}
	sort.Slice(frags, func(i, j int) bool { return frags[i].Name < frags[j].Name })

	merged, err := config.Merge(h.d.ConfigBase, frags)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, validateResponse{Error: err.Error()})
		return
	}
	cfg, err := config.Parse(merged)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, validateResponse{Error: err.Error()})
		return
	}
	resp := validateResponse{OK: true, Declared: map[string][]string{}}
	for name := range cfg.ToolUpstreams {
		resp.ToolUpstreams = append(resp.ToolUpstreams, name)
	}
	sort.Strings(resp.ToolUpstreams)
	// Only the tools the submitted overlay declares — the committed
	// table's declarations are not this answer's business.
	for _, f := range frags {
		var frag struct {
			ToolUpstreams map[string]struct {
				Tools map[string]json.RawMessage `json:"tools"`
			} `json:"tool_upstreams"`
		}
		if json.Unmarshal(f.Raw, &frag) != nil {
			continue
		}
		for _, up := range frag.ToolUpstreams {
			for tool := range up.Tools {
				fields, ok := cfg.Policy().Declared(tool)
				if !ok {
					continue
				}
				if fields == nil {
					fields = []string{}
				}
				resp.Declared[tool] = fields
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
