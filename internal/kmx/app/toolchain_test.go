package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaimahi-agents/kaimahi/internal/kmx/config"
	"github.com/kaimahi-agents/kaimahi/internal/kmx/run"
)

// A machine with a container engine and nothing else must still get past
// preflight: the tools kmx shells out to are fetched, verified and put on
// PATH, and the command carries on instead of printing four install pages.
func TestPreflightFetchesAMissingClusterTool(t *testing.T) {
	body := []byte("#!/bin/sh\necho stub\n")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") || strings.HasSuffix(r.URL.Path, ".sha256sum") {
			fmt.Fprintf(w, "%s\n", hex.EncodeToString(sum[:]))
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("KMX_HOME", home)
	t.Setenv("KMX_TOOLCHAIN", "")
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	toolchainBase = srv.URL
	defer func() { toolchainBase = "" }()

	a := &App{Cfg: &config.Config{ContainerEngine: "docker"}, Run: &run.Runner{}, Err: os.Stderr}
	if err := a.preflight(depKind); err != nil {
		t.Fatalf("preflight refused a machine kmx could have equipped: %v", err)
	}
	if len(a.provisioned) != 1 || a.provisioned[0].Name != "kind" {
		t.Fatalf("nothing was recorded as provisioned: %+v", a.provisioned)
	}
	path, err := exec.LookPath("kind")
	if err != nil {
		t.Fatalf("kind was fetched but is not on PATH: %v", err)
	}
	if !strings.HasPrefix(path, home) {
		t.Errorf("kind resolved to %q, which is not inside kmx's own directory %q", path, home)
	}
	// The cache is versioned, and the thing on PATH is a plain name pointing
	// into it — that is what makes a pin bump impossible to serve stale.
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.Base(target), "0.33.0") {
		t.Errorf("PATH entry does not point at a versioned cache entry: %s", target)
	}
}

// The opt-out is a real opt-out: nothing is downloaded, and the operator is
// told what to install.
func TestKmxToolchainOffRestoresTheInstallInstructions(t *testing.T) {
	t.Setenv("KMX_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("KMX_TOOLCHAIN", "off")
	toolchainBase = "http://127.0.0.1:1"
	defer func() { toolchainBase = "" }()

	a := &App{Cfg: &config.Config{ContainerEngine: "docker"}, Run: &run.Runner{}, Err: os.Stderr}
	err := a.preflight(depKind)
	if err == nil {
		t.Fatal("KMX_TOOLCHAIN=off still equipped the machine")
	}
	if !strings.Contains(err.Error(), "kind is not on PATH") {
		t.Errorf("the refusal does not name the missing tool:\n%v", err)
	}
}

// A container engine is a daemon and a privileged system package. kmx must
// say so rather than pretend it can be dropped into a cache directory.
func TestTheContainerEngineIsNeverFetched(t *testing.T) {
	t.Setenv("KMX_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	toolchainBase = "http://127.0.0.1:1"
	defer func() { toolchainBase = "" }()

	a := &App{Cfg: &config.Config{ContainerEngine: "docker"}, Run: &run.Runner{}, Err: os.Stderr}
	err := a.preflight(a.engineDependency())
	if err == nil || !strings.Contains(err.Error(), "docker is not on PATH") {
		t.Fatalf("the engine was not reported as the operator's own to install: %v", err)
	}
}
