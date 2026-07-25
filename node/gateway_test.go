package node

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// fakeCaddy stands in for Caddy's admin API and records what was pushed.
type fakeCaddy struct {
	mu       sync.Mutex
	server   *httptest.Server
	payloads [][]byte
	status   int
}

func newFakeCaddy(t *testing.T) *fakeCaddy {
	t.Helper()
	caddy := &fakeCaddy{status: http.StatusOK}
	caddy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/load" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		caddy.mu.Lock()
		caddy.payloads = append(caddy.payloads, body)
		status := caddy.status
		caddy.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(caddy.server.Close)
	return caddy
}

func (c *fakeCaddy) last(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.payloads) == 0 {
		t.Fatal("gateway received no configuration")
	}
	var document map[string]any
	if err := json.Unmarshal(c.payloads[len(c.payloads)-1], &document); err != nil {
		t.Fatalf("gateway received invalid JSON: %v", err)
	}
	return document
}

func gatewayFor(t *testing.T, caddy *fakeCaddy, configure func(*CaddyConfig)) *CaddyGateway {
	t.Helper()
	config := CaddyConfig{
		AdminAddress: caddy.server.URL,
		ConfigPath:   filepath.Join(t.TempDir(), "gateway.json"),
	}
	if configure != nil {
		configure(&config)
	}
	gateway, err := NewCaddyGateway(config)
	if err != nil {
		t.Fatal(err)
	}
	return gateway
}

func servingSnapshot() []control.RouteSnapshot {
	return []control.RouteSnapshot{{
		Host: "app.example.com", Workload: "app", Port: 443, Exposure: "public",
		Endpoints: []control.Endpoint{
			{Allocation: "app-0", Node: "base", Address: "10.42.0.2", Port: 8080},
			{Allocation: "app-1", Node: "base", Address: "10.42.0.3", Port: 8080},
		},
	}}
}

// The gateway must receive every serving endpoint as an upstream, so traffic
// spreads across replicas rather than one instance.
func TestGatewayProxiesToEveryEndpoint(t *testing.T) {
	caddy := newFakeCaddy(t)
	gateway := gatewayFor(t, caddy, nil)

	if err := gateway.Apply(context.Background(), servingSnapshot()); err != nil {
		t.Fatal(err)
	}
	document := caddy.last(t)
	rendered, _ := json.Marshal(document)
	body := string(rendered)

	for _, upstream := range []string{"10.42.0.2:8080", "10.42.0.3:8080"} {
		if !strings.Contains(body, upstream) {
			t.Fatalf("gateway config omitted upstream %s: %s", upstream, body)
		}
	}
	if !strings.Contains(body, "reverse_proxy") {
		t.Fatalf("gateway did not configure proxying: %s", body)
	}
}

// A route with no healthy endpoint must answer honestly rather than being
// dropped, which would let the hostname fall through to an unrelated site.
func TestGatewayServesErrorWhenNoEndpointIsHealthy(t *testing.T) {
	caddy := newFakeCaddy(t)
	gateway := gatewayFor(t, caddy, nil)

	snapshot := servingSnapshot()
	snapshot[0].Endpoints = nil
	if err := gateway.Apply(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(caddy.last(t))
	if !strings.Contains(string(body), "503") {
		t.Fatalf("an endpointless route did not produce an error response: %s", body)
	}
	if strings.Contains(string(body), "reverse_proxy") {
		t.Fatalf("an endpointless route was still proxied: %s", body)
	}
	// The hostname must still be matched, or it would fall through.
	if !strings.Contains(string(body), "app.example.com") {
		t.Fatalf("the route host was dropped entirely: %s", body)
	}
}

// A public route must get ACME certificate automation, which is what replaces
// cert-manager.
func TestGatewayRequestsCertificatesForPublicRoutes(t *testing.T) {
	caddy := newFakeCaddy(t)
	gateway := gatewayFor(t, caddy, func(config *CaddyConfig) {
		config.ACMEEmail = "ops@example.com"
	})

	if err := gateway.Apply(context.Background(), servingSnapshot()); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(caddy.last(t))
	for _, want := range []string{"acme", "ops@example.com", "app.example.com"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("certificate automation missing %q: %s", want, body)
		}
	}
}

// A tailnet route has no public DNS, so ACME cannot work. Internal issuance
// must be used instead of failing or silently serving no TLS.
func TestGatewayUsesInternalIssuerWhenConfigured(t *testing.T) {
	caddy := newFakeCaddy(t)
	gateway := gatewayFor(t, caddy, func(config *CaddyConfig) {
		config.TLSInternal = true
	})

	snapshot := servingSnapshot()
	snapshot[0].Exposure = "tailnet"
	if err := gateway.Apply(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(caddy.last(t))
	if !strings.Contains(string(body), "internal") {
		t.Fatalf("internal issuance was not configured: %s", body)
	}
	if strings.Contains(string(body), "acme") {
		t.Fatalf("ACME was requested for a tailnet route: %s", body)
	}
}

// A tailnet route must not request a public certificate, which ACME could not
// issue anyway.
func TestGatewaySkipsACMEForTailnetRoutes(t *testing.T) {
	caddy := newFakeCaddy(t)
	gateway := gatewayFor(t, caddy, nil)

	snapshot := servingSnapshot()
	snapshot[0].Exposure = "tailnet"
	if err := gateway.Apply(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(caddy.last(t))
	if strings.Contains(string(body), "acme") {
		t.Fatalf("a tailnet route requested a public certificate: %s", body)
	}
}

// The applied snapshot must be persisted, so an operator can see what was meant
// to be running and a restarted node can reconcile against it.
func TestGatewayPersistsAppliedConfig(t *testing.T) {
	caddy := newFakeCaddy(t)
	path := filepath.Join(t.TempDir(), "nested", "gateway.json")
	gateway, err := NewCaddyGateway(CaddyConfig{
		AdminAddress: caddy.server.URL, ConfigPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Apply(context.Background(), servingSnapshot()); err != nil {
		t.Fatal(err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("gateway config was not persisted: %v", err)
	}
	if !strings.Contains(string(saved), "app.example.com") {
		t.Fatalf("persisted config is missing the route: %s", saved)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("gateway config has permissive mode %v", mode)
	}
}

// A gateway that refuses a configuration must surface the failure, not report
// success while routing stays on the old config.
func TestGatewayReportsRefusedConfig(t *testing.T) {
	caddy := newFakeCaddy(t)
	caddy.status = http.StatusBadRequest
	gateway := gatewayFor(t, caddy, nil)

	err := gateway.Apply(context.Background(), servingSnapshot())
	if err == nil || !strings.Contains(err.Error(), "refused config") {
		t.Fatalf("expected the gateway refusal to surface, got %v", err)
	}
}

// An unreachable gateway must fail loudly rather than silently leaving routing
// on a stale configuration.
func TestGatewayReportsUnreachableAdmin(t *testing.T) {
	gateway, err := NewCaddyGateway(CaddyConfig{
		// A port nothing is listening on.
		AdminAddress: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Apply(context.Background(), servingSnapshot()); err == nil {
		t.Fatal("an unreachable gateway reported success")
	}
}

// The router must resolve endpoints for the routes it publishes, or the gateway
// receives a route with nothing behind it.
func TestRouterResolvesEndpointsIntoSnapshot(t *testing.T) {
	gateway := &recordingGateway{}
	router := NewRouter(gateway)
	router.Endpoints = func(workload string) []control.Endpoint {
		if workload != "app" {
			return nil
		}
		return []control.Endpoint{{Allocation: "app-0", Address: "10.42.0.2", Port: 8080}}
	}

	if _, err := router.Execute(context.Background(), control.Action{
		Kind: control.ActionPublishRoute, Target: "app.example.com",
		Workload: "app", Port: 443, Exposure: "public",
	}); err != nil {
		t.Fatal(err)
	}
	if len(gateway.snapshots) != 1 {
		t.Fatalf("expected one snapshot, got %d", len(gateway.snapshots))
	}
	snapshot := gateway.snapshots[0][0]
	if len(snapshot.Endpoints) != 1 || snapshot.Endpoints[0].Address != "10.42.0.2" {
		t.Fatalf("router did not resolve endpoints: %+v", snapshot)
	}
}

// canarySnapshot splits traffic 10/90 between two versions.
func canarySnapshot() []control.RouteSnapshot {
	snapshot := servingSnapshot()
	snapshot[0].Weighted = []control.WeightedEndpoint{
		{
			Endpoint: control.Endpoint{
				Allocation: "app-0", Node: "base", Address: "10.42.0.2", Port: 8080,
			},
			Weight: 10, Version: "registry.example/app@sha256:new",
		},
		{
			Endpoint: control.Endpoint{
				Allocation: "app-1", Node: "base", Address: "10.42.0.3", Port: 8080,
			},
			Weight: 90, Version: "registry.example/app@sha256:old",
		},
	}
	return snapshot
}

// A canary's weights must reach the gateway, or the control plane computes a
// traffic split that nothing enforces.
func TestGatewayAppliesCanaryWeights(t *testing.T) {
	caddy := newFakeCaddy(t)
	gateway := gatewayFor(t, caddy, nil)

	if err := gateway.Apply(context.Background(), canarySnapshot()); err != nil {
		t.Fatal(err)
	}
	document := caddy.last(t)
	rendered, _ := json.Marshal(document)
	body := string(rendered)

	if !strings.Contains(body, "weighted_round_robin") {
		t.Fatalf("gateway did not configure weighted balancing: %s", body)
	}
	// Both upstreams are still present, so no endpoint is dropped by weighting.
	for _, upstream := range []string{"10.42.0.2:8080", "10.42.0.3:8080"} {
		if !strings.Contains(body, upstream) {
			t.Fatalf("weighting dropped upstream %s: %s", upstream, body)
		}
	}
	// The weights must appear in the same order as the upstreams, or traffic goes
	// to the version the canary meant to hold back.
	weights := weightOrder(t, document)
	if len(weights) != 2 || weights[0] != 10 || weights[1] != 90 {
		t.Fatalf("weights are %v, want [10 90]", weights)
	}
	dials := dialOrder(t, document)
	if len(dials) != 2 || dials[0] != "10.42.0.2:8080" || dials[1] != "10.42.0.3:8080" {
		t.Fatalf("upstream order is %v, which does not match the weights", dials)
	}
}

// An ordinary route carries no weights, so a gateway sees the same config it
// always did and a canary is genuinely opt-in.
func TestGatewayOmitsWeightsWithoutACanary(t *testing.T) {
	caddy := newFakeCaddy(t)
	gateway := gatewayFor(t, caddy, nil)

	if err := gateway.Apply(context.Background(), servingSnapshot()); err != nil {
		t.Fatal(err)
	}
	rendered, _ := json.Marshal(caddy.last(t))
	if strings.Contains(string(rendered), "weighted_round_robin") {
		t.Fatalf("an unweighted route configured weighted balancing: %s", rendered)
	}
}

// handlerOf digs the reverse_proxy handler out of the rendered config.
func handlerOf(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	apps, _ := document["apps"].(map[string]any)
	httpApp, _ := apps["http"].(map[string]any)
	servers, _ := httpApp["servers"].(map[string]any)
	a4s, _ := servers["a4s"].(map[string]any)
	routes, _ := a4s["routes"].([]any)
	if len(routes) == 0 {
		t.Fatalf("no routes in config: %+v", document)
	}
	route, _ := routes[0].(map[string]any)
	handlers, _ := route["handle"].([]any)
	if len(handlers) == 0 {
		t.Fatalf("no handlers in route: %+v", route)
	}
	handler, _ := handlers[0].(map[string]any)
	return handler
}

func weightOrder(t *testing.T, document map[string]any) []int {
	t.Helper()
	balancing, _ := handlerOf(t, document)["load_balancing"].(map[string]any)
	policy, _ := balancing["selection_policy"].(map[string]any)
	raw, _ := policy["weights"].([]any)
	weights := make([]int, 0, len(raw))
	for _, value := range raw {
		number, ok := value.(float64)
		if !ok {
			t.Fatalf("weight %v is not a number", value)
		}
		weights = append(weights, int(number))
	}
	return weights
}

func dialOrder(t *testing.T, document map[string]any) []string {
	t.Helper()
	raw, _ := handlerOf(t, document)["upstreams"].([]any)
	dials := make([]string, 0, len(raw))
	for _, value := range raw {
		upstream, _ := value.(map[string]any)
		dial, _ := upstream["dial"].(string)
		dials = append(dials, dial)
	}
	return dials
}
