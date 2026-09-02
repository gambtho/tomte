package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/kaimahi-agents/kaimahi/plane/internal/config"
	"github.com/kaimahi-agents/kaimahi/plane/internal/egress"
	"github.com/kaimahi-agents/kaimahi/plane/internal/gateway"
	"github.com/kaimahi-agents/kaimahi/plane/internal/proxy"
)

// hardenedClient builds the ONE hardened client (P10, D25) over every
// upstream marked `internet: true` in both tables, and vets each host at
// boot: a host that resolves to a private, link-local, loopback,
// carrier-NAT, multicast or metadata address refuses the config LOUDLY
// here, not at first use. A host that cannot be resolved at boot is a
// warning, not a refusal: the per-call check is the real gate (a record
// can change after boot anyway — DNS rebinding — which is why every call
// re-resolves), and a kind cluster with no route to the internet must
// still boot the keyless in-cluster path. The same client goes to the
// LLM proxy and the MCP gateway (wireInternet), so nothing about the
// Copilot path's hardening is implicit.
func hardenedClient(ctx context.Context, cfg config.Config) (*http.Client, error) {
	hosts := cfg.InternetHosts()
	client, err := egress.NewClient(egress.Policy{}, hosts)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		addrs, err := egress.Vet(ctx, nil, h.Name)
		switch {
		case errors.Is(err, egress.ErrPrivateAddress):
			return nil, fmt.Errorf("hosted upstream refused at config load: %w", err)
		case err != nil:
			slog.Warn("hosted upstream host did not resolve at boot; every call re-vets it",
				"host", h.Name, "err", err)
		default:
			slog.Info("hosted upstream vetted", "host", h.Name, "addresses", addrs,
				"trust", trustOf(h))
		}
	}
	return client, nil
}

func trustOf(h egress.Host) string {
	if h.CAFile == "" {
		return "system roots"
	}
	return "ca_file " + h.CAFile
}

// wireInternet injects the one hardened client into BOTH seams. Kept as
// a function so a test can assert the two handlers share the very same
// client — the property D25 asks for.
func wireInternet(pd proxy.Deps, gd gateway.Deps, client *http.Client) (proxy.Deps, gateway.Deps) {
	pd.InternetClient = client
	gd.InternetClient = client
	return pd, gd
}
