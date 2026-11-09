package control

import (
	"strings"
	"testing"
)

func apiPolicyGoal() Goal {
	return Goal{ID: "api", Workload: WorkloadSpec{Name: "api", Port: 8000,
		Policy: &NetworkPolicy{Ingress: []IngressRule{{FromWorkload: "api", Port: 8000}}}}}
}

// The network agent's own proposal must be authorized: the kernel recompiles
// the same intent and gets the same bytes.
func TestApplyPolicyAcceptsTheKernelsCompilation(t *testing.T) {
	world, _ := policyWorld()
	actions := stalePolicies(apiPolicyGoal(), world)
	if len(actions) != 1 {
		t.Fatalf("expected one policy action, got %d", len(actions))
	}
	if err := validateApplyPolicy(apiPolicyGoal(), world, actions[0]); err != nil {
		t.Fatalf("the agent's compiled ruleset was refused: %v", err)
	}
}

// The ruleset is piped to nft as root. Any statement the kernel did not
// compile itself must be refused, however well-formed the envelope.
func TestApplyPolicyRefusesRulesTheKernelDidNotCompile(t *testing.T) {
	world, _ := policyWorld()
	compiled := stalePolicies(apiPolicyGoal(), world)[0].Policy

	injected := *compiled
	injected.Rules = append(append([]string(nil), compiled.Rules...), "flush ruleset")
	widened := *compiled
	widened.Rules = append(append([]string(nil), compiled.Rules...),
		"add rule inet a4s input accept")

	for name, policy := range map[string]CompiledPolicy{
		"arbitrary script":   {Node: "alpha", Table: PolicyTable, Rules: []string{"flush ruleset"}},
		"appended statement": injected,
		"widened rule":       widened,
	} {
		t.Run(name, func(t *testing.T) {
			policy := policy
			err := validateApplyPolicy(apiPolicyGoal(), world, Action{
				ID: "apply", Kind: ActionApplyPolicy, Node: "alpha", Policy: &policy,
			})
			if err == nil || !strings.Contains(err.Error(), "differs") {
				t.Fatalf("want the uncompiled ruleset refused, got %v", err)
			}
		})
	}
}

func TestApplyPolicyRefusedForGoalWithoutPolicy(t *testing.T) {
	world, _ := policyWorld()
	compiled := stalePolicies(apiPolicyGoal(), world)[0].Policy
	goal := apiPolicyGoal()
	goal.Workload.Policy = nil
	err := validateApplyPolicy(goal, world, Action{
		ID: "apply", Kind: ActionApplyPolicy, Node: "alpha", Policy: compiled,
	})
	if err == nil || !strings.Contains(err.Error(), "declares no network policy") {
		t.Fatalf("a goal that declares no policy must not install one, got %v", err)
	}
}

// The whole path through Authorize, as the network agent would reach it.
func TestAuthorizeRefusesArbitraryNftScript(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{ID: "p", AgentID: "network-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: scenario.World.Revision,
		Actions: []Action{{ID: "fw", Kind: ActionApplyPolicy, Target: "base", Node: "base",
			Policy: &CompiledPolicy{Node: "base", Table: PolicyTable, Rules: []string{"flush ruleset"}}}}}
	kernel := Kernel{Policy: DefaultPolicy()}
	if err := kernel.Authorize(AgentDescriptor{ID: "network-agent"}, scenario.Goal,
		scenario.World, proposal); err == nil {
		t.Fatal("an arbitrary nft script was authorized")
	}
}

func zoneGoal() Goal {
	return Goal{ID: "api", Workload: WorkloadSpec{Name: "api", Port: 8000}}
}

func TestPublishZoneAcceptsTheKernelsResolution(t *testing.T) {
	world, _ := policyWorld()
	actions := staleZones(zoneGoal(), world)
	if len(actions) != 1 {
		t.Fatalf("expected one zone action, got %d", len(actions))
	}
	if err := validatePublishZone(zoneGoal(), world, actions[0]); err != nil {
		t.Fatalf("the agent's zone was refused: %v", err)
	}
}

// A zone taken on trust could point any service name at any address.
func TestPublishZoneRefusesRecordsTheKernelDidNotResolve(t *testing.T) {
	world, _ := policyWorld()
	legitimate := *staleZones(zoneGoal(), world)[0].Zone

	hijacked := ServiceZone{Node: "alpha", Records: map[string][]ResolvedEndpoint{}}
	for name, endpoints := range legitimate.Records {
		hijacked.Records[name] = endpoints
	}
	hijacked.Records[ServiceName("billing")] = []ResolvedEndpoint{{Address: "203.0.113.66", Port: 443}}

	redirected := ServiceZone{Node: "alpha", Records: map[string][]ResolvedEndpoint{}}
	for name := range legitimate.Records {
		redirected.Records[name] = []ResolvedEndpoint{{Address: "203.0.113.66", Port: 8000}}
	}

	for name, zone := range map[string]ServiceZone{"extra name": hijacked, "redirected name": redirected} {
		t.Run(name, func(t *testing.T) {
			zone := zone
			err := validatePublishZone(zoneGoal(), world, Action{
				ID: "zone", Kind: ActionPublishZone, Node: "alpha", Zone: &zone,
			})
			if err == nil || !strings.Contains(err.Error(), "differs") {
				t.Fatalf("want the forged zone refused, got %v", err)
			}
		})
	}
}
