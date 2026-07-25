package control

import (
	"strings"
	"testing"
	"time"
)

func policyWorld() (World, map[string]int) {
	now := time.Now().UTC()
	world := World{
		Revision:   3,
		ObservedAt: now,
		Nodes: map[string]*Node{
			"alpha": {ID: "alpha", Healthy: true},
		},
		Allocations: map[string]*Allocation{
			"api-0": {
				ID: "api-0", Workload: "api", Node: "alpha", Address: "10.42.0.5",
				Phase: AllocationRunning, Ready: true, ReadyExpiresAt: now.Add(time.Minute),
			},
			"web-0": {
				ID: "web-0", Workload: "web", Node: "alpha", Address: "10.42.0.9",
				Phase: AllocationRunning, Ready: true, ReadyExpiresAt: now.Add(time.Minute),
			},
		},
	}
	return world, map[string]int{"api": 8000, "web": 80}
}

// The base ruleset must deny by default, or a policy that fails to compile a
// rule would leave the workload open rather than closed.
func TestCompiledPolicyDeniesByDefault(t *testing.T) {
	world, ports := policyWorld()
	compiled, err := CompilePolicies(world, ports, nil, "alpha")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	script := compiled.Script()
	for _, want := range []string{
		"add table inet a4s",
		"flush table inet a4s",
		"policy drop",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("ruleset missing %q:\n%s", want, script)
		}
	}
	// Established traffic must still be allowed or nothing works at all.
	if !strings.Contains(script, "ct state established,related accept") {
		t.Fatalf("ruleset drops reply traffic:\n%s", script)
	}
}

// A rule naming a workload must expand to the addresses observed serving it,
// so the rule survives replacement without being rewritten.
func TestPolicyExpandsWorkloadNameToEndpoints(t *testing.T) {
	world, ports := policyWorld()
	compiled, err := CompilePolicies(world, ports, []NetworkPolicy{{
		Workload: "api",
		Ingress:  []IngressRule{{FromWorkload: "web", Port: 8000}},
	}}, "alpha")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	script := compiled.Script()
	if !strings.Contains(script, "ip saddr 10.42.0.9/32 ip daddr 10.42.0.5/32 tcp dport 8000 accept") {
		t.Fatalf("ruleset did not expand the workload name:\n%s", script)
	}
}

// This is the property that makes the compiler safe: a rule naming a workload
// with nothing serving compiles to no permission, not to a wildcard.
func TestPolicyFailsClosedForAbsentWorkload(t *testing.T) {
	world, ports := policyWorld()
	compiled, err := CompilePolicies(world, ports, []NetworkPolicy{{
		Workload: "api",
		Ingress:  []IngressRule{{FromWorkload: "ghost", Port: 8000}},
	}}, "alpha")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	script := compiled.Script()
	if strings.Contains(script, "accept") && strings.Contains(script, "ghost") {
		t.Fatalf("a rule for an absent workload produced a permission:\n%s", script)
	}
	// Only the two established-state rules should accept anything.
	if got := strings.Count(script, "accept"); got != 2 {
		t.Fatalf("ruleset holds %d accepts, want only the established-state pair:\n%s",
			got, script)
	}
}

func TestPolicyCompilesEgressToCIDR(t *testing.T) {
	world, ports := policyWorld()
	compiled, err := CompilePolicies(world, ports, []NetworkPolicy{{
		Workload: "api",
		Egress:   []EgressRule{{ToCIDR: "10.0.0.0/8", Port: 5432}},
	}}, "alpha")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(compiled.Script(),
		"ip saddr 10.42.0.5/32 ip daddr 10.0.0.0/8 tcp dport 5432 accept") {
		t.Fatalf("egress rule not compiled:\n%s", compiled.Script())
	}
}

// A workload with no allocation here produces no local rules, so a node is not
// asked to enforce policy for something it does not run.
func TestPolicyForRemoteWorkloadCompilesNoLocalRules(t *testing.T) {
	world, ports := policyWorld()
	world.Allocations["api-0"].Node = "beta"

	compiled, err := CompilePolicies(world, ports, []NetworkPolicy{{
		Workload: "api",
		Ingress:  []IngressRule{{FromWorkload: "web", Port: 8000}},
	}}, "alpha")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if strings.Contains(compiled.Script(), "10.42.0.5") {
		t.Fatalf("compiled a rule for an allocation on another node:\n%s", compiled.Script())
	}
}

// A malformed CIDR must be refused at compile time. Passing it through would
// make nft reject the whole ruleset, taking every other policy down with it.
func TestPolicyRefusesMalformedCIDR(t *testing.T) {
	world, ports := policyWorld()
	_, err := CompilePolicies(world, ports, []NetworkPolicy{{
		Workload: "api",
		Ingress:  []IngressRule{{FromCIDR: "not-a-cidr", Port: 80}},
	}}, "alpha")
	if err == nil {
		t.Fatal("expected a malformed CIDR to be refused")
	}
}

func TestPolicyValidationRefusesAmbiguousRules(t *testing.T) {
	for _, policy := range []NetworkPolicy{
		{Workload: "api", Ingress: []IngressRule{{}}},
		{Workload: "api", Ingress: []IngressRule{{FromWorkload: "web", FromCIDR: "10.0.0.0/8"}}},
		{Workload: "api", Egress: []EgressRule{{}}},
		{Workload: "api", Egress: []EgressRule{{ToWorkload: "db", ToCIDR: "10.0.0.0/8"}}},
		{Workload: "api", Ingress: []IngressRule{{FromWorkload: "web", Port: 70000}}},
		{Workload: ""},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("accepted an invalid policy: %+v", policy)
		}
	}
}

// Compilation must be deterministic, or every reconciliation would look like a
// change and reinstall the ruleset.
func TestCompilationIsDeterministic(t *testing.T) {
	world, ports := policyWorld()
	policies := []NetworkPolicy{{
		Workload: "api",
		Ingress:  []IngressRule{{FromWorkload: "web", Port: 8000}},
		Egress:   []EgressRule{{ToCIDR: "10.0.0.0/8", Port: 5432}},
	}}

	first, err := CompilePolicies(world, ports, policies, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		next, err := CompilePolicies(world, ports, policies, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		if next.Fingerprint() != first.Fingerprint() {
			t.Fatalf("compilation is not deterministic:\n%s\nvs\n%s",
				first.Script(), next.Script())
		}
	}
}

// A changed policy must produce a different fingerprint, or a node would never
// be told to reinstall.
func TestFingerprintChangesWithPolicy(t *testing.T) {
	world, ports := policyWorld()
	first, _ := CompilePolicies(world, ports, []NetworkPolicy{{
		Workload: "api", Ingress: []IngressRule{{FromWorkload: "web", Port: 8000}},
	}}, "alpha")
	second, _ := CompilePolicies(world, ports, []NetworkPolicy{{
		Workload: "api", Ingress: []IngressRule{{FromWorkload: "web", Port: 9000}},
	}}, "alpha")

	if first.Fingerprint() == second.Fingerprint() {
		t.Fatal("a changed policy produced the same fingerprint")
	}
}

// The kernel must refuse a ruleset compiled for another node, since allocation
// addresses are node-local.
func TestApplyPolicyRefusesForeignRuleset(t *testing.T) {
	world, _ := policyWorld()
	err := validateApplyPolicy(Goal{ID: "g"}, world, Action{
		ID: "apply", Kind: ActionApplyPolicy, Node: "alpha",
		Policy: &CompiledPolicy{Node: "beta", Table: PolicyTable, Rules: []string{"x"}},
	})
	if err == nil {
		t.Fatal("expected a ruleset compiled for another node to be refused")
	}
}

// a4s owns exactly one table. A ruleset naming another would edit firewall
// state outside the boundary a4s is allowed to manage.
func TestApplyPolicyRefusesForeignTable(t *testing.T) {
	world, _ := policyWorld()
	err := validateApplyPolicy(Goal{ID: "g"}, world, Action{
		ID: "apply", Kind: ActionApplyPolicy, Node: "alpha",
		Policy: &CompiledPolicy{Node: "alpha", Table: "filter", Rules: []string{"x"}},
	})
	if err == nil {
		t.Fatal("expected a ruleset targeting another table to be refused")
	}
}

func TestApplyPolicyRequiresACompiledRuleset(t *testing.T) {
	world, _ := policyWorld()
	if err := validateApplyPolicy(Goal{ID: "g"}, world, Action{
		ID: "apply", Kind: ActionApplyPolicy, Node: "alpha",
	}); err == nil {
		t.Fatal("expected an action with no ruleset to be refused")
	}
}

// Only the network agent may install firewall rules.
func TestOnlyNetworkAgentMayApplyPolicy(t *testing.T) {
	policy := DefaultPolicy()
	if !policy.Grants["network-agent"][ActionApplyPolicy] {
		t.Fatal("network agent cannot apply policy")
	}
	for _, agent := range []string{"placement-agent", "rollout-agent", "storage-agent"} {
		if policy.Grants[agent][ActionApplyPolicy] {
			t.Fatalf("%s must not be granted firewall control", agent)
		}
	}
}
