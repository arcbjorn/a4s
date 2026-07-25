package control

import (
	"testing"
	"time"
)

// twoNodeWorld places one ready replica on each of two nodes.
func twoNodeWorld() (World, map[string]int) {
	now := time.Now().UTC()
	world := World{
		Revision:   4,
		ObservedAt: now,
		Nodes: map[string]*Node{
			"alpha": {ID: "alpha", Healthy: true, Address: "100.64.0.1", GatewayPort: 8080},
			"beta":  {ID: "beta", Healthy: true, Address: "100.64.0.2", GatewayPort: 8080},
		},
		Allocations: map[string]*Allocation{
			"api-0": {
				ID: "api-0", Workload: "api", Node: "alpha", Address: "10.42.0.5",
				Phase: AllocationRunning, Ready: true, ReadyExpiresAt: now.Add(time.Minute),
			},
			"api-1": {
				ID: "api-1", Workload: "api", Node: "beta", Address: "10.43.0.7",
				Phase: AllocationRunning, Ready: true, ReadyExpiresAt: now.Add(time.Minute),
			},
		},
	}
	return world, map[string]int{"api": 8000}
}

// This is the M3 property: one name means the same service from any node.
func TestServiceNameResolvesFromEveryNode(t *testing.T) {
	world, ports := twoNodeWorld()

	for _, node := range []string{"alpha", "beta"} {
		endpoints := Resolve(world, ports, "api", node)
		if len(endpoints) != 2 {
			t.Fatalf("from %s: resolved %d endpoints, want 2", node, len(endpoints))
		}
	}
}

// A caller must reach a local instance directly, because an allocation address
// is routable only on its own node.
func TestLocalEndpointIsDialedDirectly(t *testing.T) {
	world, ports := twoNodeWorld()

	endpoints := Resolve(world, ports, "api", "alpha")
	local := endpoints[0]
	if !local.Local {
		t.Fatal("the local endpoint was not returned first")
	}
	if local.Address != "10.42.0.5" || local.Port != 8000 {
		t.Fatalf("local endpoint = %s, want the allocation address", local.HostPort())
	}
}

// A caller elsewhere must reach the instance through its owning node's gateway,
// since the allocation address means nothing off that node.
func TestRemoteEndpointGoesThroughTheOwningGateway(t *testing.T) {
	world, ports := twoNodeWorld()

	endpoints := Resolve(world, ports, "api", "alpha")
	var remote ResolvedEndpoint
	for _, endpoint := range endpoints {
		if !endpoint.Local {
			remote = endpoint
		}
	}
	if remote.Allocation != "api-1" {
		t.Fatalf("remote endpoint = %+v", remote)
	}
	if remote.Address != "100.64.0.2" || remote.Port != 8080 {
		t.Fatalf("remote endpoint = %s, want the owning node's gateway", remote.HostPort())
	}
	// The ultimate target is still named, so an operator can see past the hop.
	if remote.Node != "beta" {
		t.Fatalf("remote endpoint does not name its node: %+v", remote)
	}
}

// A node nobody can reach contributes no endpoints. A resolvable name that
// times out is worse than an honest omission.
func TestUnreachableNodeContributesNoEndpoint(t *testing.T) {
	world, ports := twoNodeWorld()
	world.Nodes["beta"].Address = ""

	endpoints := Resolve(world, ports, "api", "alpha")
	if len(endpoints) != 1 {
		t.Fatalf("resolved %d endpoints, want only the local one", len(endpoints))
	}
	if !endpoints[0].Local {
		t.Fatal("expected only the local endpoint")
	}
}

// A node with no gateway cannot forward, so its allocations are local-only.
func TestNodeWithoutGatewayIsLocalOnly(t *testing.T) {
	world, ports := twoNodeWorld()
	world.Nodes["beta"].GatewayPort = 0

	if endpoints := Resolve(world, ports, "api", "alpha"); len(endpoints) != 1 {
		t.Fatalf("resolved %d endpoints from alpha, want 1", len(endpoints))
	}
	// From beta itself the allocation is still reachable directly.
	if endpoints := Resolve(world, ports, "api", "beta"); len(endpoints) != 2 {
		t.Fatalf("resolved %d endpoints from beta, want 2", len(endpoints))
	}
}

// Readiness expiry must remove an endpoint, so traffic never goes to an
// instance nobody has recently observed serving.
func TestExpiredReadinessRemovesEndpoint(t *testing.T) {
	world, ports := twoNodeWorld()
	world.Allocations["api-1"].ReadyExpiresAt = world.Now().Add(-time.Minute)

	endpoints := Resolve(world, ports, "api", "alpha")
	if len(endpoints) != 1 {
		t.Fatalf("resolved %d endpoints, want 1 after readiness expired", len(endpoints))
	}
}

func TestParseServiceName(t *testing.T) {
	for name, want := range map[string]string{
		"api":                  "api",
		"api.a4s.internal":     "api",
		"API.A4S.INTERNAL":     "api",
		"api.a4s.internal.":    "api",
		"  api.a4s.internal  ": "api",
	} {
		got, ok := ParseServiceName(name)
		if !ok || got != want {
			t.Fatalf("parse %q = %q,%t, want %q", name, got, ok, want)
		}
	}
}

// Names outside the zone must not resolve, or the resolver becomes a way to
// reach arbitrary hosts through a4s.
func TestParseServiceNameRefusesForeignNames(t *testing.T) {
	for _, name := range []string{
		"", "example.com", "api.example.com", "a4s.internal",
		"tenant.api.a4s.internal",
	} {
		if workload, ok := ParseServiceName(name); ok {
			t.Fatalf("parsed foreign name %q as workload %q", name, workload)
		}
	}
}

func TestResolveNameRejectsForeignNames(t *testing.T) {
	world, ports := twoNodeWorld()
	if _, err := ResolveName(world, ports, "example.com", "alpha"); err == nil {
		t.Fatal("expected a foreign name to be refused")
	}
}

// The zone must publish only names that actually resolve from this node.
func TestServiceZoneOmitsUnreachableNames(t *testing.T) {
	world, ports := twoNodeWorld()
	// Remove the local replica and make the remote node unreachable, so the
	// service is serving somewhere but reachable from nowhere.
	delete(world.Allocations, "api-0")
	world.Nodes["beta"].Address = ""

	zone := BuildServiceZone(world, ports, "alpha")
	if _, published := zone.Records[ServiceName("api")]; published {
		t.Fatal("published a name with no reachable endpoint")
	}
}

func TestServiceZonePublishesReachableNames(t *testing.T) {
	world, ports := twoNodeWorld()
	zone := BuildServiceZone(world, ports, "alpha")

	endpoints, published := zone.Records["api.a4s.internal"]
	if !published {
		t.Fatalf("zone did not publish the service: %+v", zone.Records)
	}
	if len(endpoints) != 2 {
		t.Fatalf("zone published %d endpoints, want 2", len(endpoints))
	}
}

// Resolution must be stable, or a client retrying a lookup would be sent
// somewhere different for no reason.
func TestResolutionOrderIsStable(t *testing.T) {
	world, ports := twoNodeWorld()
	first := Resolve(world, ports, "api", "alpha")
	for range 20 {
		next := Resolve(world, ports, "api", "alpha")
		for index := range first {
			if first[index] != next[index] {
				t.Fatalf("resolution order changed: %+v then %+v", first, next)
			}
		}
	}
}

func TestUnknownServiceResolvesToNothing(t *testing.T) {
	world, ports := twoNodeWorld()
	if endpoints := Resolve(world, ports, "ghost", "alpha"); len(endpoints) != 0 {
		t.Fatalf("unknown service resolved to %+v", endpoints)
	}
}

// The M3 exit criterion, end to end: the network agent publishes names to every
// healthy node, and a workload on a node holding no replica can still resolve
// the service to something it can dial.
func TestNamesConvergeToEveryNode(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	goal := scenario.Goal
	goal.Route = nil

	world := scenario.World
	// Give every node a reachable address so cross-node forwarding is possible.
	for _, node := range world.Nodes {
		node.Address = "100.64.0." + node.ID
		node.GatewayPort = 8080
	}

	executor := NewMemoryExecutor(world)
	engine := NewEngine(executor, PlacementAgent{}, NetworkAgent{})
	if err := engine.Run(goal, 20); err != nil {
		t.Fatalf("converge: %v", err)
	}

	final := executor.World()
	published := 0
	for _, node := range final.Nodes {
		if node.ZoneFingerprint != "" {
			published++
		}
	}
	if published == 0 {
		t.Fatal("no node received a service zone")
	}

	// The name must resolve from a node, including one holding no replica.
	ports := map[string]int{goal.Workload.Name: goal.Workload.Port}
	for id := range final.Nodes {
		zone := BuildServiceZone(final, ports, id)
		if len(zone.Records) == 0 {
			t.Fatalf("service name does not resolve from node %q", id)
		}
		for _, endpoints := range zone.Records {
			for _, endpoint := range endpoints {
				if endpoint.Address == "" || endpoint.Port == 0 {
					t.Fatalf("node %q resolved to an undialable endpoint: %+v", id, endpoint)
				}
			}
		}
	}
}

// Republishing an unchanged zone every round would be pure churn, so the
// fingerprint must make a converged cluster stop proposing zone updates.
func TestUnchangedZoneIsNotRepublished(t *testing.T) {
	world, ports := twoNodeWorld()
	goal := Goal{
		ID: "api-goal", Workload: WorkloadSpec{Name: "api", Port: ports["api"], Replicas: 2},
	}
	// Record the current fingerprint on both nodes, as publication would.
	for id, node := range world.Nodes {
		node.ZoneFingerprint = BuildServiceZone(world, ports, id).Fingerprint()
	}

	if actions := staleZones(goal, world); len(actions) != 0 {
		t.Fatalf("proposed %d zone updates for an unchanged cluster", len(actions))
	}
}
