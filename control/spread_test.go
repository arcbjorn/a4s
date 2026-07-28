package control

import (
	"strings"
	"testing"
)

// spreadWorld builds a cluster whose nodes sit in named failure domains.
func spreadWorld(domains map[string]string) World {
	world := World{Nodes: map[string]*Node{}, Allocations: map[string]*Allocation{}}
	for id, domain := range domains {
		world.Nodes[id] = &Node{
			ID: id, Domain: domain, Healthy: true,
			Capacity: Resources{CPUMillis: 4000, MemoryMB: 8192},
			Images:   map[string]bool{spreadImage: true},
		}
	}
	return world
}

const spreadImage = "registry.example/web@sha256:" +
	"3333333333333333333333333333333333333333333333333333333333333333"

func spreadGoal(replicas, maxPerDomain int) Goal {
	goal := Goal{
		APIVersion: APIVersion, ID: "web-public", Objective: "serve web",
		Workload: WorkloadSpec{
			Name: "web", Image: spreadImage, Replicas: replicas, Port: 8080,
			Resources: Resources{CPUMillis: 100, MemoryMB: 128},
		},
	}
	if maxPerDomain > 0 {
		goal.Workload.Placement = &Placement{MaxPerDomain: maxPerDomain}
	}
	return goal
}

// A node that declares no domain is its own, so spreading works on a cluster
// nobody has labelled yet.
func TestUndeclaredDomainIsTheNodeItself(t *testing.T) {
	node := &Node{ID: "node-a"}
	if got := node.FailureDomain(); got != "node-a" {
		t.Fatalf("FailureDomain = %q, want the node id", got)
	}
	labelled := &Node{ID: "node-a", Domain: "rack-1"}
	if got := labelled.FailureDomain(); got != "rack-1" {
		t.Fatalf("FailureDomain = %q, want rack-1", got)
	}
}

// The kernel refuses a placement that would overfill a domain, whoever proposed
// it. This is the denial that makes the constraint real rather than advisory.
func TestKernelRefusesOverfillingAFailureDomain(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-1"})
	goal := spreadGoal(2, 1)
	// One replica already occupies rack-1 on node-a.
	world.Allocations["web-0"] = &Allocation{
		ID: "web-0", Workload: "web", Replica: 0, Node: "node-a",
		Image: spreadImage, Phase: AllocationRunning,
		Resources: goal.Workload.Resources,
	}

	// Placing the second replica on node-b lands in the same domain.
	err := validateAction(goal, world, Action{
		ID: "create-web-1", Kind: ActionCreateAllocation, Target: "web-1",
		Workload: "web", Node: "node-b", Image: spreadImage, Replica: 1,
		Resources: goal.Workload.Resources,
	})
	if err == nil || !strings.Contains(err.Error(), "failure domain") {
		t.Fatalf("expected a failure-domain denial, got %v", err)
	}
}

// A stopped replica no longer occupies its domain, or a rollout could never
// replace the allocation it just retired.
func TestStoppedReplicaReleasesItsDomain(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-2"})
	goal := spreadGoal(2, 1)
	world.Allocations["web-0"] = &Allocation{
		ID: "web-0", Workload: "web", Replica: 0, Node: "node-a",
		Image: spreadImage, Phase: AllocationStopped,
		Resources: goal.Workload.Resources,
	}
	if held := DomainOccupancy(world, "web")["rack-1"]; held != 0 {
		t.Fatalf("a stopped replica still occupies its domain: %d", held)
	}
	if err := validateAction(goal, world, Action{
		ID: "create-web-1", Kind: ActionCreateAllocation, Target: "web-1",
		Workload: "web", Node: "node-a", Image: spreadImage, Replica: 1,
		Resources: goal.Workload.Resources,
	}); err != nil {
		t.Fatalf("replacing into a vacated domain was refused: %v", err)
	}
}

// The placement agent must choose nodes that satisfy the constraint, or it would
// propose work the kernel refuses and the goal would block on its own agent.
func TestPlacementAgentSpreadsAcrossDomains(t *testing.T) {
	world := spreadWorld(map[string]string{
		"node-a": "rack-1", "node-b": "rack-1", "node-c": "rack-2",
	})
	proposal, err := (PlacementAgent{}).Propose(spreadGoal(2, 1), world)
	if err != nil {
		t.Fatal(err)
	}

	domains := map[string]int{}
	for _, action := range proposal.Actions {
		if action.Kind != ActionCreateAllocation {
			continue
		}
		domains[world.Nodes[action.Node].FailureDomain()]++
	}
	if len(domains) != 2 {
		t.Fatalf("expected replicas in two domains, got %v", domains)
	}
	for domain, count := range domains {
		if count > 1 {
			t.Fatalf("domain %q received %d replicas, want at most 1", domain, count)
		}
	}
}

// Every proposed replica must survive the kernel, which is the property that
// keeps the agent and the policy from disagreeing.
func TestPlacementAgentProposalSatisfiesTheKernel(t *testing.T) {
	world := spreadWorld(map[string]string{
		"node-a": "rack-1", "node-b": "rack-2", "node-c": "rack-3",
	})
	goal := spreadGoal(3, 1)
	proposal, err := (PlacementAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatal(err)
	}
	simulated := cloneWorld(world)
	for _, action := range proposal.Actions {
		if err := validateAction(goal, simulated, action); err != nil {
			t.Fatalf("kernel refused the agent's own action %q: %v", action.ID, err)
		}
		if err := simulateAction(&simulated, action); err != nil {
			t.Fatalf("simulate %q: %v", action.ID, err)
		}
	}
}

// When every domain is full the agent must say so in those terms. "No capacity"
// would send an operator to look at memory when the problem is topology.
func TestPlacementAgentReportsACrowdedTopology(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-1"})
	goal := spreadGoal(2, 1)
	world.Allocations["web-0"] = &Allocation{
		ID: "web-0", Workload: "web", Replica: 0, Node: "node-a",
		Image: spreadImage, Phase: AllocationRunning,
		Resources: goal.Workload.Resources,
	}
	_, err := (PlacementAgent{}).Propose(goal, world)
	if err == nil || !strings.Contains(err.Error(), "failure domain") {
		t.Fatalf("expected a topology failure, got %v", err)
	}
}

// A goal wanting more replicas than the topology can hold is refused at
// admission, where the mistake is, rather than blocking mid-reconciliation.
func TestUnsatisfiableSpreadIsRefusedAtAdmission(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-2"})
	scenario := Scenario{Goal: spreadGoal(3, 1), World: world}
	err := scenario.NormalizeAndValidate()
	if err == nil || !strings.Contains(err.Error(), "failure domains") {
		t.Fatalf("expected an admission denial, got %v", err)
	}

	// Two replicas across two domains is satisfiable and must be accepted.
	ok := Scenario{Goal: spreadGoal(2, 1), World: spreadWorld(
		map[string]string{"node-a": "rack-1", "node-b": "rack-2"})}
	if err := ok.NormalizeAndValidate(); err != nil {
		t.Fatalf("a satisfiable spread was refused: %v", err)
	}
}

// An unhealthy node's domain is not capacity a goal can be admitted against.
func TestFailureDomainsCountsHealthyNodesOnly(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-2"})
	world.Nodes["node-b"].Healthy = false
	if domains := FailureDomains(world); len(domains) != 1 || domains[0] != "rack-1" {
		t.Fatalf("FailureDomains = %v, want [rack-1]", domains)
	}
}

func TestPlacementValidation(t *testing.T) {
	if err := (&Placement{MaxPerDomain: -1}).Validate(); err == nil {
		t.Fatal("a negative ceiling was accepted")
	}
	if err := (&Placement{MaxPerDomain: 2}).Validate(); err != nil {
		t.Fatalf("a usable placement was refused: %v", err)
	}
	var absent *Placement
	if err := absent.Validate(); err != nil {
		t.Fatalf("an absent placement was refused: %v", err)
	}
}
