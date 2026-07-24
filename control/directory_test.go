package control

import (
	"testing"
	"time"
)

func servingWorld(t *testing.T, replicas int) World {
	t.Helper()
	world := World{Nodes: map[string]*Node{
		"base": {
			ID: "base", Healthy: true, Labels: map[string]string{"pool": "base"},
			Capacity: Resources{CPUMillis: 8000, MemoryMB: 16384},
		},
	}}
	world.normalize()
	world.ObservedAt = time.Unix(9000, 0).UTC()
	for replica := 0; replica < replicas; replica++ {
		id := allocationID("app", replica)
		world.Allocations[id] = &Allocation{
			ID: id, Workload: "app", Replica: replica, Node: "base",
			Image: testImage, Phase: AllocationRunning, Ready: true,
			Address:        "10.42.0." + string(rune('2'+replica)),
			ReadyExpiresAt: world.ObservedAt.Add(30 * time.Second),
		}
	}
	world.Routes["app.example.com"] = &Route{
		Host: "app.example.com", Workload: "app", Port: 443, Exposure: "public",
	}
	return world
}

func appPorts() map[string]int { return map[string]int{"app": 8080} }

// The directory must list every replica that was actually observed serving, so
// traffic spreads across them rather than one arbitrary instance.
func TestDirectoryListsServingReplicas(t *testing.T) {
	world := servingWorld(t, 3)
	services := BuildDirectory(world, appPorts())

	endpoints := services["app"].Endpoints
	if len(endpoints) != 3 {
		t.Fatalf("expected three endpoints, got %d: %+v", len(endpoints), endpoints)
	}
	seen := make(map[string]bool)
	for _, endpoint := range endpoints {
		if endpoint.Port != 8080 {
			t.Fatalf("endpoint used the wrong port: %+v", endpoint)
		}
		if seen[endpoint.HostPort()] {
			t.Fatalf("duplicate endpoint %s", endpoint.HostPort())
		}
		seen[endpoint.HostPort()] = true
	}
}

// An allocation whose readiness has expired must leave the directory. Routing to
// an instance nobody has recently observed serving is how a rollout becomes an
// outage.
func TestExpiredReadinessLeavesDirectory(t *testing.T) {
	world := servingWorld(t, 2)
	fresh := BuildDirectory(world, appPorts())
	if len(fresh["app"].Endpoints) != 2 {
		t.Fatalf("expected two fresh endpoints: %+v", fresh["app"].Endpoints)
	}

	// Time passes beyond the readiness expiry with no new measurement.
	world.ObservedAt = world.ObservedAt.Add(time.Minute)
	stale := BuildDirectory(world, appPorts())
	if len(stale["app"].Endpoints) != 0 {
		t.Fatalf("expired readiness still served traffic: %+v", stale["app"].Endpoints)
	}
}

// An allocation with no address cannot be dialed, so it must not appear even
// when it is otherwise ready.
func TestAllocationWithoutAddressIsNotServed(t *testing.T) {
	world := servingWorld(t, 2)
	world.Allocations["app-1"].Address = ""

	endpoints := BuildDirectory(world, appPorts())["app"].Endpoints
	if len(endpoints) != 1 || endpoints[0].Allocation != "app-0" {
		t.Fatalf("an addressless allocation was published: %+v", endpoints)
	}
}

// A stopped allocation must leave the directory immediately, not linger until
// its readiness expires.
func TestStoppedAllocationLeavesDirectory(t *testing.T) {
	world := servingWorld(t, 2)
	world.Allocations["app-0"].Phase = AllocationStopped
	world.Allocations["app-0"].Ready = false

	endpoints := BuildDirectory(world, appPorts())["app"].Endpoints
	if len(endpoints) != 1 || endpoints[0].Allocation != "app-1" {
		t.Fatalf("a stopped allocation was still served: %+v", endpoints)
	}
}

// The directory is derived from evidence. There is no path by which an agent
// or a caller can insert an endpoint the world does not support.
func TestDirectoryIgnoresUnknownWorkloadPorts(t *testing.T) {
	world := servingWorld(t, 2)
	// No declared port for this workload means nothing to dial.
	if services := BuildDirectory(world, map[string]int{}); len(services) != 0 {
		t.Fatalf("endpoints appeared without a declared port: %+v", services)
	}
}

// Route snapshots must resolve to the endpoints currently serving the route's
// workload.
func TestRouteSnapshotResolvesEndpoints(t *testing.T) {
	world := servingWorld(t, 2)
	snapshots := BuildRouteSnapshots(world, appPorts())

	if len(snapshots) != 1 {
		t.Fatalf("expected one route snapshot, got %d", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.Host != "app.example.com" || snapshot.Port != 443 {
		t.Fatalf("snapshot lost the route definition: %+v", snapshot)
	}
	if len(snapshot.Endpoints) != 2 {
		t.Fatalf("snapshot did not resolve endpoints: %+v", snapshot.Endpoints)
	}
}

// A route whose workload has no healthy instance must still appear, carrying an
// empty endpoint list. Dropping it would let the hostname fall through to an
// unrelated site instead of failing honestly.
func TestRouteWithNoHealthyEndpointIsRetained(t *testing.T) {
	world := servingWorld(t, 1)
	world.Allocations["app-0"].Ready = false

	snapshots := BuildRouteSnapshots(world, appPorts())
	if len(snapshots) != 1 {
		t.Fatalf("a route with no endpoints was dropped: %+v", snapshots)
	}
	if len(snapshots[0].Endpoints) != 0 {
		t.Fatalf("expected no endpoints: %+v", snapshots[0].Endpoints)
	}
}

// Snapshots must be deterministic, or an unchanged world would produce a config
// the gateway treats as new and reloads for no reason.
func TestRouteSnapshotsAreDeterministic(t *testing.T) {
	world := servingWorld(t, 3)
	first := BuildRouteSnapshots(world, appPorts())
	second := BuildRouteSnapshots(world, appPorts())

	if len(first) != len(second) {
		t.Fatalf("snapshot length changed: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if len(first[i].Endpoints) != len(second[i].Endpoints) {
			t.Fatalf("endpoint count changed for %s", first[i].Host)
		}
		for j := range first[i].Endpoints {
			if first[i].Endpoints[j] != second[i].Endpoints[j] {
				t.Fatalf("endpoint order is unstable: %+v vs %+v",
					first[i].Endpoints[j], second[i].Endpoints[j])
			}
		}
	}
}

// An operator needs to know which routes have stopped resolving.
func TestStaleRoutesAreReported(t *testing.T) {
	world := servingWorld(t, 2)
	if stale := StaleAfter(world, appPorts(), world.ObservedAt); len(stale) != 0 {
		t.Fatalf("a healthy route was reported stale: %+v", stale)
	}
	later := world.ObservedAt.Add(time.Hour)
	stale := StaleAfter(world, appPorts(), later)
	if len(stale) != 1 || stale[0] != "app.example.com" {
		t.Fatalf("expired route was not reported: %+v", stale)
	}
}

// Workload ports come from accepted goals, which is what makes an endpoint
// dialable. The route port is what the gateway listens on and is separate.
func TestWorkloadPortsComeFromGoals(t *testing.T) {
	goal := validScenario().Goal
	ports := WorkloadPorts([]Goal{goal})
	if ports["app"] != 8080 {
		t.Fatalf("workload port was not taken from the goal: %+v", ports)
	}
	if ports["app"] == goal.Route.Port {
		t.Fatal("workload port was confused with the route's listen port")
	}
}
