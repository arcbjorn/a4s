package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

func attachAction(allocation string) control.Action {
	return control.Action{
		ID: "attach-" + allocation, Kind: control.ActionAttachNetwork,
		Target: allocation, Workload: "web", Node: "base", Port: 8080,
	}
}

// Every allocation gets its own address, which is what lets replicas share a
// node without contending for a host port.
func TestAttachAssignsDistinctAddresses(t *testing.T) {
	backend, err := NewMemoryNetwork("10.42.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	network := NewNetwork(backend)
	ctx := context.Background()

	seen := make(map[string]string)
	for _, allocation := range []string{"web-0", "web-1", "web-2"} {
		evidence, err := network.Execute(ctx, attachAction(allocation))
		if err != nil {
			t.Fatal(err)
		}
		address := evidence.Observed["address"]
		if address == "" {
			t.Fatalf("%s received no address", allocation)
		}
		if previous, clash := seen[address]; clash {
			t.Fatalf("%s and %s share address %s", previous, allocation, address)
		}
		seen[address] = allocation
		if evidence.Observed["namespace"] == "" {
			t.Fatalf("%s received no namespace: %+v", allocation, evidence.Observed)
		}
	}
}

// A replayed attach must return the existing attachment rather than creating a
// second namespace, which would strand the first.
func TestAttachIsIdempotent(t *testing.T) {
	backend, err := NewMemoryNetwork("10.42.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	network := NewNetwork(backend)
	ctx := context.Background()

	first, err := network.Execute(ctx, attachAction("web-0"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := network.Execute(ctx, attachAction("web-0"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed["address"] != second.Observed["address"] {
		t.Fatalf("replayed attach changed the address: %s -> %s",
			first.Observed["address"], second.Observed["address"])
	}
	if second.Observed["already_attached"] != "true" {
		t.Fatalf("replayed attach was not reported as a repeat: %+v", second.Observed)
	}
}

// Detach must release the address so it can be reused, or a cluster that churns
// allocations would exhaust its subnet.
func TestDetachReleasesAddressForReuse(t *testing.T) {
	backend, err := NewMemoryNetwork("10.42.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	network := NewNetwork(backend)
	ctx := context.Background()

	first, err := network.Execute(ctx, attachAction("web-0"))
	if err != nil {
		t.Fatal(err)
	}
	released := first.Observed["address"]

	if _, err := network.Detach(ctx, "web-0"); err != nil {
		t.Fatal(err)
	}
	// A replayed detach must not fail.
	if _, err := network.Detach(ctx, "web-0"); err != nil {
		t.Fatalf("replayed detach failed: %v", err)
	}

	reused, err := network.Execute(ctx, attachAction("web-9"))
	if err != nil {
		t.Fatal(err)
	}
	if reused.Observed["address"] != released {
		t.Logf("address %s was not immediately reused (got %s); acceptable but noted",
			released, reused.Observed["address"])
	}
	if _, err := network.Attachment(ctx, "web-0"); err == nil {
		t.Fatal("a detached allocation still reports an attachment")
	}
}

// A backend that returns no address must be refused rather than producing
// evidence that would record an empty address in the world.
func TestAttachRejectsMissingAddress(t *testing.T) {
	network := NewNetwork(&brokenNetwork{})
	_, err := network.Execute(context.Background(), attachAction("web-0"))
	if err == nil || !strings.Contains(err.Error(), "no address") {
		t.Fatalf("expected a missing-address rejection, got %v", err)
	}
}

// A malformed address must be refused, since the probe path would otherwise
// try to dial it.
func TestAttachRejectsInvalidAddress(t *testing.T) {
	network := NewNetwork(&brokenNetwork{address: "not-an-ip"})
	_, err := network.Execute(context.Background(), attachAction("web-0"))
	if err == nil || !strings.Contains(err.Error(), "invalid address") {
		t.Fatalf("expected an invalid-address rejection, got %v", err)
	}
}

// A failed attach must not leak the address it reserved.
func TestFailedAttachReleasesAddress(t *testing.T) {
	backend, err := NewMemoryNetwork("10.42.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	backend.FailAttach = errors.New("plugin unavailable")
	network := NewNetwork(backend)

	if _, err := network.Execute(context.Background(), attachAction("web-0")); err == nil {
		t.Fatal("expected the attach to fail")
	}
	if address, assigned := backend.ipam.Address("web-0"); assigned {
		t.Fatalf("failed attach leaked address %s", address)
	}
}

// Deleting an allocation must tear down its network, or a node would accumulate
// namespaces and exhaust its address pool.
func TestDeleteDetachesNetwork(t *testing.T) {
	backend, err := NewMemoryNetwork("10.42.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	network := NewNetwork(backend)
	containers := NewContainerRuntime(&supervisedBackend{states: map[string]BackendState{}})
	runtime := &CompositeRuntime{Containers: containers, Networks: network}
	ctx := context.Background()

	if _, err := network.Execute(ctx, attachAction("web-0")); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, control.Action{
		Kind: control.ActionDeleteAllocation, Target: "web-0", Workload: "web",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := network.Attachment(ctx, "web-0"); err == nil {
		t.Fatal("delete left the network attached")
	}
	if _, assigned := backend.ipam.Address("web-0"); assigned {
		t.Fatal("delete left the address assigned")
	}
}

// IPAM must not hand out the subnet's network or broadcast address, and must
// report exhaustion rather than wrapping.
func TestIPAMRespectsSubnetBounds(t *testing.T) {
	// A /30 has exactly two usable hosts, one of which is the bridge.
	ipam, err := NewBridgeIPAM("10.42.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := ipam.Assign("web-0")
	if err != nil {
		t.Fatal(err)
	}
	if first != "10.42.0.2" {
		t.Fatalf("first assignment should skip the bridge, got %s", first)
	}
	if _, _, err := ipam.Assign("web-1"); err == nil {
		t.Fatal("IPAM handed out an address beyond the subnet")
	}
}

func TestIPAMAssignmentIsStable(t *testing.T) {
	ipam, err := NewBridgeIPAM("10.42.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	first, reused, err := ipam.Assign("web-0")
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("a first assignment was reported as reused")
	}
	second, reused, err := ipam.Assign("web-0")
	if err != nil {
		t.Fatal(err)
	}
	if !reused || first != second {
		t.Fatalf("repeated assignment changed: %s -> %s (reused=%t)", first, second, reused)
	}
}

func TestIPAMRejectsInvalidSubnet(t *testing.T) {
	if _, err := NewBridgeIPAM("not-a-cidr"); err == nil {
		t.Fatal("accepted an invalid subnet")
	}
	if _, err := NewBridgeIPAM("fd00::/64"); err == nil {
		t.Fatal("accepted an IPv6 subnet without support for it")
	}
}

// The probe must refuse to guess an address. Probing loopback could report a
// dead workload healthy because an unrelated process holds the port.
func TestProbeRefusesToGuessAddress(t *testing.T) {
	observer := NewRuntimeObserver(NewContainerRuntime(
		&fakeBackend{state: BackendState{Exists: true, Running: true}}))
	_, _, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeTCP, Port: 8080,
	})
	if err == nil || !strings.Contains(err.Error(), "no known address") {
		t.Fatalf("probe guessed an address instead of refusing: %v", err)
	}
}

// Given the allocation's address, the probe must dial that address rather than
// loopback. A short timeout keeps the test fast: the dial is expected to fail,
// and what is asserted is the address it targeted.
func TestProbeUsesAllocationAddress(t *testing.T) {
	observer := NewRuntimeObserver(NewContainerRuntime(
		&fakeBackend{state: BackendState{Exists: true, Running: true}}))
	observer.Timeout = 50 * time.Millisecond
	_, observed, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeTCP, Port: 8080, Address: "10.42.0.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed["address"] != "10.42.0.5:8080" {
		t.Fatalf("probe did not target the allocation address: %+v", observed)
	}
}

// brokenNetwork returns malformed attachments for failure-path tests.
type brokenNetwork struct{ address string }

func (b *brokenNetwork) Attach(context.Context, NetworkRequest) (NetworkAttachment, error) {
	return NetworkAttachment{Address: b.address}, nil
}
func (b *brokenNetwork) Detach(context.Context, string) (bool, error) { return false, nil }
func (b *brokenNetwork) Check(context.Context, string) (NetworkAttachment, error) {
	return NetworkAttachment{}, errors.New("no attachment")
}
func (b *brokenNetwork) Close() error { return nil }
