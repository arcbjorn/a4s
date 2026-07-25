package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// DefaultGatewayTimeout bounds a config push so an unresponsive gateway cannot
// stall the control loop.
const DefaultGatewayTimeout = 10 * time.Second

// CaddyConfig describes how the node talks to its local gateway.
type CaddyConfig struct {
	// AdminAddress is Caddy's admin API, conventionally localhost-only.
	AdminAddress string
	// ConfigPath is where the last applied snapshot is persisted, so a gateway
	// restart can be reconciled against what was authorized.
	ConfigPath string
	// ACMEEmail is the contact address for certificate issuance.
	ACMEEmail string
	// TLSInternal issues self-signed certificates instead of using ACME, for a
	// tailnet-only route or a test environment with no public DNS.
	TLSInternal bool
	Timeout     time.Duration
	Client      *http.Client
}

// CaddyGateway applies whole route snapshots to a local Caddy instance.
//
// The gateway is not a control plane. It receives a complete configuration and
// replaces its own with it; it never merges, never decides, and never learns
// endpoints on its own. That keeps routing exactly as authorized: a config the
// kernel did not approve cannot appear through incremental drift.
//
// Caddy is used because it obtains and renews certificates natively, which
// collapses ingress and cert-manager into one component the node already needs.
type CaddyGateway struct {
	config CaddyConfig
	mu     sync.Mutex
}

func NewCaddyGateway(config CaddyConfig) (*CaddyGateway, error) {
	if config.AdminAddress == "" {
		config.AdminAddress = "http://127.0.0.1:2019"
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultGatewayTimeout
	}
	if config.Client == nil {
		config.Client = &http.Client{Timeout: config.Timeout}
	}
	if config.ConfigPath != "" {
		if !filepath.IsAbs(config.ConfigPath) {
			return nil, fmt.Errorf("gateway config path must be absolute")
		}
		if err := os.MkdirAll(filepath.Dir(config.ConfigPath), 0o750); err != nil {
			return nil, fmt.Errorf("create gateway config directory: %w", err)
		}
	}
	return &CaddyGateway{config: config}, nil
}

// Apply replaces the gateway's configuration with one built from the snapshot.
func (g *CaddyGateway) Apply(ctx context.Context, snapshots []control.RouteSnapshot) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	document, err := g.render(snapshots)
	if err != nil {
		return err
	}
	// Persist before pushing. If the push fails the operator can still see what
	// was meant to be applied, and a restarted node can reconcile against it.
	if g.config.ConfigPath != "" {
		if err := writeAtomic(g.config.ConfigPath, document); err != nil {
			return err
		}
	}
	return g.push(ctx, document)
}

// render builds a complete Caddy configuration from the snapshot.
func (g *CaddyGateway) render(snapshots []control.RouteSnapshot) ([]byte, error) {
	routes := make([]map[string]any, 0, len(snapshots))
	var acmeHosts []string

	for _, snapshot := range snapshots {
		if snapshot.Host == "" {
			return nil, fmt.Errorf("route snapshot has no host")
		}
		upstreams := make([]map[string]any, 0, len(snapshot.Endpoints))
		for _, endpoint := range snapshot.Endpoints {
			upstreams = append(upstreams, map[string]any{"dial": endpoint.HostPort()})
		}

		handler := map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": upstreams,
		}
		// A canary in progress carries per-endpoint weights. Caddy's weighted
		// round-robin takes the weights positionally, so the upstream list is
		// rebuilt from the weighted set rather than reordered: a weight applied to
		// the wrong upstream would send traffic to the version it was meant to
		// hold back.
		if weights := weightedUpstreams(snapshot); len(weights) > 0 {
			upstreams = upstreams[:0]
			numbers := make([]any, 0, len(snapshot.Weighted))
			for _, endpoint := range snapshot.Weighted {
				upstreams = append(upstreams, map[string]any{"dial": endpoint.HostPort()})
				numbers = append(numbers, endpoint.Weight)
			}
			handler["upstreams"] = upstreams
			handler["load_balancing"] = map[string]any{
				"selection_policy": map[string]any{
					"policy":  "weighted_round_robin",
					"weights": numbers,
				},
			}
		}
		if len(upstreams) == 0 {
			// A route with no serving endpoint answers 503 rather than being
			// dropped. Dropping it would make the hostname fall through to an
			// unrelated site, which is a worse failure than an honest error.
			handler = map[string]any{
				"handler":     "static_response",
				"status_code": 503,
				"body":        "no healthy endpoint for " + snapshot.Workload,
			}
		}
		routes = append(routes, map[string]any{
			"match":  []map[string]any{{"host": []string{snapshot.Host}}},
			"handle": []map[string]any{handler},
		})
		if snapshot.Exposure == "public" && !g.config.TLSInternal {
			acmeHosts = append(acmeHosts, snapshot.Host)
		}
	}

	server := map[string]any{
		"listen": []string{":443"},
		"routes": routes,
	}
	apps := map[string]any{
		"http": map[string]any{
			"servers": map[string]any{"a4s": server},
		},
	}
	if g.config.TLSInternal {
		// Internal issuance keeps a tailnet-only route working without public
		// DNS, which ACME would require.
		apps["tls"] = map[string]any{
			"automation": map[string]any{
				"policies": []map[string]any{{
					"issuers": []map[string]any{{"module": "internal"}},
				}},
			},
		}
	} else if len(acmeHosts) > 0 {
		issuer := map[string]any{"module": "acme"}
		if g.config.ACMEEmail != "" {
			issuer["email"] = g.config.ACMEEmail
		}
		apps["tls"] = map[string]any{
			"automation": map[string]any{
				"policies": []map[string]any{{
					"subjects": acmeHosts,
					"issuers":  []map[string]any{issuer},
				}},
			},
		}
	}

	return json.MarshalIndent(map[string]any{"apps": apps}, "", "  ")
}

// push replaces the running configuration through Caddy's admin API.
func (g *CaddyGateway) push(ctx context.Context, document []byte) error {
	ctx, cancel := context.WithTimeout(ctx, g.config.Timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.config.AdminAddress+"/load", bytes.NewReader(document))
	if err != nil {
		return fmt.Errorf("build gateway request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := g.config.Client.Do(request)
	if err != nil {
		return fmt.Errorf("push gateway config: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body := make([]byte, 512)
		n, _ := response.Body.Read(body)
		return fmt.Errorf("gateway refused config: %s: %s", response.Status, string(body[:n]))
	}
	return nil
}

// writeAtomic replaces a file in one step, so a crash mid-write cannot leave a
// partial configuration behind.
func writeAtomic(path string, content []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return fmt.Errorf("write gateway config: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace gateway config: %w", err)
	}
	return nil
}

// weightedUpstreams reports the weights to apply, or nothing when the snapshot
// carries no canary split.
//
// A zero-weight upstream would be configured but never selected, which reads as a
// healthy endpoint receiving no traffic for no visible reason. Anything at or
// below zero is treated as no split at all rather than silently black-holed.
func weightedUpstreams(snapshot control.RouteSnapshot) []control.WeightedEndpoint {
	if len(snapshot.Weighted) < 2 {
		return nil
	}
	for _, endpoint := range snapshot.Weighted {
		if endpoint.Weight <= 0 {
			return nil
		}
	}
	return snapshot.Weighted
}
