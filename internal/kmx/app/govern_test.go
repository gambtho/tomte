package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kaimahi-agents/kaimahi/internal/kmx/config"
	"github.com/kaimahi-agents/kaimahi/internal/kmx/run"
)

// A fake kubectl on PATH, so `kmx govern` can be driven end to end without a
// cluster: every argument list and everything piped into it is recorded.
const fakeKubectl = `#!/bin/sh
printf '%s\n' "$*" >> "$KMX_TEST_ARGS"
case "$*" in
  *"config view"*)
    cat <<'JSON'
{"clusters":[{"name":"kind-kaimahi-p1","cluster":{"server":"https://127.0.0.1:6443"}}],
 "contexts":[{"name":"kind-kaimahi-p1","context":{"cluster":"kind-kaimahi-p1"}}]}
JSON
    exit 0 ;;
  *"get secret kaimahi-admin"*) printf '%s' "$KMX_TEST_ADMIN_B64"; exit 0 ;;
  *port-forward*) exec sleep 30 ;;
  *"get agent hello-world"*)
    if [ -n "$KMX_TEST_AGENT_ERR" ]; then
      printf '%s\n' "$KMX_TEST_AGENT_ERR" >&2
      exit 1
    fi
    printf 'agent.kagent.dev/hello-world\n'; exit 0 ;;
  *"apply -f -"*) cat >> "$KMX_TEST_STDIN"; exit 0 ;;
esac
exit 0
`

type governFixture struct {
	app     *App
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	argsLog string
	stdin   string
}

func newGovernFixture(t *testing.T, agentErr string, issue http.HandlerFunc) *governFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake kubectl is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(bin, []byte(fakeKubectl), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		issue(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	f := &governFixture{
		out:     &bytes.Buffer{},
		errOut:  &bytes.Buffer{},
		argsLog: filepath.Join(dir, "args"),
		stdin:   filepath.Join(dir, "stdin"),
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KMX_TEST_ARGS", f.argsLog)
	t.Setenv("KMX_TEST_STDIN", f.stdin)
	t.Setenv("KMX_TEST_AGENT_ERR", agentErr)
	t.Setenv("KMX_TEST_ADMIN_B64", base64.StdEncoding.EncodeToString([]byte("admin-bearer")))

	cfg := &config.Config{
		KindCluster: "kaimahi-p1",
		KubeContext: "kind-kaimahi-p1",
		AdminPort:   u.Port(),
		Credential:  "hello-world",
	}
	r := run.Default()
	r.Stdout, r.Stderr = f.out, f.errOut
	f.app = &App{Cfg: cfg, Run: r, Out: f.out, Err: f.errOut}
	return f
}

func (f *governFixture) args() string {
	b, _ := os.ReadFile(f.argsLog)
	return string(b)
}

func (f *governFixture) piped() string {
	b, _ := os.ReadFile(f.stdin)
	return string(b)
}

func issued(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}
}

func governOptions() GovernOptions {
	return GovernOptions{
		Agent:           config.DefaultAgent,
		Preset:          config.GovernedModelConfig,
		Secret:          config.GovernedSecret,
		SecretNamespace: config.DefaultNamespace,
	}
}

// The rule `make govern` states in a comment and enforces with a grep, here
// enforced by construction: ONLY a genuine NotFound may skip the switch.
// Every other failure — an unreachable API server, an expired credential, an
// RBAC denial, a wrong context — must abort. Collapsing them prints a
// reassuring NOTE, exits 0, and leaves the agent on an UNGOVERNED preset,
// spending outside the plane.
func TestGovernRefusesToSkipTheSwitchOnAnAmbiguousRead(t *testing.T) {
	for _, ambiguous := range []string{
		"The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?",
		`Error from server (Forbidden): agents.kagent.dev is forbidden: User "x" cannot get resource "agents"`,
		`error: the server doesn't have a resource type "agent"`,
		"error: You must be logged in to the server (Unauthorized)",
	} {
		t.Run(ambiguous[:20], func(t *testing.T) {
			f := newGovernFixture(t, ambiguous, issued("kmh_"+strings.Repeat("a", 64)))
			err := f.app.Govern("hello-world", governOptions())
			if err == nil {
				t.Fatal("govern succeeded without switching the agent")
			}
			if !strings.Contains(err.Error(), "refusing to leave it ungoverned") {
				t.Errorf("wrong refusal: %v", err)
			}
			if strings.Contains(f.errOut.String(), "does not exist yet") {
				t.Errorf("an unreadable agent was reported as absent:\n%s", f.errOut.String())
			}
		})
	}
}

// A genuine NotFound is the managed-cluster ordering, where governance is
// stood up before the agents exist. It proceeds, says so, and still applies
// the presets — so the agent is created governed when it arrives.
func TestGovernNotesAGenuinelyAbsentAgentAndStillAppliesThePresets(t *testing.T) {
	f := newGovernFixture(t,
		`Error from server (NotFound): agents.kagent.dev "hello-world" not found`,
		issued("kmh_"+strings.Repeat("b", 64)))
	if err := f.app.Govern("hello-world", governOptions()); err != nil {
		t.Fatalf("govern: %v", err)
	}
	if !strings.Contains(f.errOut.String(), "does not exist yet") {
		t.Errorf("the absent agent was not reported:\n%s", f.errOut.String())
	}
	piped := f.piped()
	for _, want := range []string{"governed-ollama", "governed-copilot"} {
		if !strings.Contains(piped, want) {
			t.Errorf("preset %s was not applied:\n%s", want, piped)
		}
	}
}

// Custody, proven rather than commented: the issued token reaches the
// cluster through kubectl's STDIN and appears in no argument, no environment
// listing, no file kmx wrote, and nothing printed. This is the property the
// shell script needed a 0600 file and a dry-run pipe to approximate.
func TestTheIssuedTokenTravelsOnlyThroughThePipe(t *testing.T) {
	token := "kmh_" + strings.Repeat("c", 64)
	f := newGovernFixture(t,
		`Error from server (NotFound): agents.kagent.dev "hello-world" not found`,
		issued(token))
	if err := f.app.Govern("hello-world", governOptions()); err != nil {
		t.Fatalf("govern: %v", err)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(token))
	if !strings.Contains(f.piped(), "api-key: "+encoded) {
		t.Fatalf("the token was not piped into kubectl:\n%s", f.piped())
	}
	if !strings.Contains(f.piped(), `kaimahi.dev/credential: "hello-world"`) {
		t.Errorf("the Secret is not bound to its credential:\n%s", f.piped())
	}
	for name, body := range map[string]string{
		"kubectl arguments": f.args(),
		"stdout":            f.out.String(),
		"stderr":            f.errOut.String(),
	} {
		if strings.Contains(body, token) || strings.Contains(body, encoded) {
			t.Errorf("the token appears in %s:\n%s", name, body)
		}
	}
	// ...and the admin bearer, which gates issuing every credential, is not
	// in an argument list either.
	if strings.Contains(f.args(), "admin-bearer") {
		t.Errorf("the admin token appears in kubectl's arguments:\n%s", f.args())
	}
}

// A credential name is interpolated into JSON and a query string. The
// script's check_name is kept because the plane validating again is a second
// line, not the first.
func TestCredentialNamesAreValidated(t *testing.T) {
	for _, bad := range []string{"", "Hello", "hello world", "hello/../x", `hello"`, "hello.world"} {
		if err := validCredentialName(bad); err == nil {
			t.Errorf("credential name %q was accepted", bad)
		}
	}
	for _, good := range []string{"hello-world", "kaimahi-plane", "a1"} {
		if err := validCredentialName(good); err != nil {
			t.Errorf("credential name %q was refused: %v", good, err)
		}
	}
}

// An already-issued credential whose Secret is bound elsewhere must refuse.
// Reusing that Secret would hand one credential's token to another
// credential's agent, and the ledger would attribute the spend to the wrong
// one — silently, because both tokens are opaque.
func TestAnAlreadyIssuedCredentialIsReconciledNotOverwritten(t *testing.T) {
	conflict := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"credential exists"}`))
	}
	f := newGovernFixture(t, "", conflict)
	// The fake kubectl answers the annotation read with an empty string,
	// which is the "exists in the plane, Secret missing or unlabeled" case:
	// the token cannot be recovered, so the operator is told exactly how to
	// clear the row rather than being handed a half-governed agent.
	err := f.app.Govern("hello-world", governOptions())
	if err == nil {
		t.Fatal("a 409 with no bound Secret was accepted")
	}
	if !strings.Contains(err.Error(), "shown exactly once") ||
		!strings.Contains(err.Error(), "DELETE FROM credential") {
		t.Errorf("the refusal does not tell the operator how to recover: %v", err)
	}
}
