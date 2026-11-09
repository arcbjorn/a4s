package control

import (
	"strings"
	"testing"
)

// An approval for one goal must not block admission of another. The server
// validates every submission against the live world, which holds every
// approval ever granted, so refusing foreign approvals would wedge admission
// after the first grant.
func TestAdmissionIgnoresApprovalsForOtherGoals(t *testing.T) {
	scenario := validScenario()
	scenario.World.Approvals["web-public-route"] = &Approval{
		ID: "web-public-route", GoalID: "web", Scope: "public-route",
		IssuedBy: "operator:alice", Granted: true,
	}
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatalf("an approval for another goal blocked admission: %v", err)
	}
}

// A foreign approval is admitted but grants nothing: authorization still
// matches on goal id.
func TestForeignApprovalGrantsNothing(t *testing.T) {
	scenario := validScenario()
	scenario.World.Approvals = map[string]*Approval{
		"web-public-route": {ID: "web-public-route", GoalID: "web", Scope: "public-route",
			IssuedBy: "operator:alice", Granted: true},
	}
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	if hasApproval(scenario.World, scenario.Goal.ID, "public-route") {
		t.Fatal("an approval for another goal authorized this one")
	}
}

func TestAdmissionRejectsMalformedApprovals(t *testing.T) {
	tests := map[string]*Approval{
		"nil":          nil,
		"id mismatch":  {ID: "other", GoalID: "app-public", Scope: "public-route", IssuedBy: "op"},
		"no goal":      {ID: "a", Scope: "public-route", IssuedBy: "op"},
		"no scope":     {ID: "a", GoalID: "app-public", IssuedBy: "op"},
		"no issued by": {ID: "a", GoalID: "app-public", Scope: "public-route"},
	}
	for name, approval := range tests {
		t.Run(name, func(t *testing.T) {
			scenario := validScenario()
			scenario.World.Approvals = map[string]*Approval{"a": approval}
			err := scenario.NormalizeAndValidate()
			if err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("want a malformed-approval error, got %v", err)
			}
		})
	}
}

// A null node entry reaches validation from scenario JSON or a server's base
// world. It must be refused by name, not dereferenced.
func TestAdmissionRefusesNullNodeWithoutPanic(t *testing.T) {
	scenario := validScenario()
	scenario.World.Nodes["ghost"] = nil
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("a null node panicked validation: %v", recovered)
		}
	}()
	err := scenario.NormalizeAndValidate()
	if err == nil || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("want the null node refused by name, got %v", err)
	}
}

// An uncompilable policy must be refused at admission. Accepted, it would be
// skipped every round and the workload would run with no firewall at all.
func TestAdmissionRefusesInvalidNetworkPolicy(t *testing.T) {
	for name, policy := range map[string]*NetworkPolicy{
		"malformed cidr":   {Egress: []EgressRule{{ToCIDR: "10.0.0.0/33"}}},
		"no source":        {Ingress: []IngressRule{{Port: 8080}}},
		"other workload":   {Workload: "billing", Ingress: []IngressRule{{FromCIDR: "10.0.0.0/8"}}},
		"ambiguous source": {Ingress: []IngressRule{{FromWorkload: "web", FromCIDR: "10.0.0.0/8"}}},
	} {
		t.Run(name, func(t *testing.T) {
			scenario := validScenario()
			scenario.Goal.Workload.Policy = policy
			if err := scenario.NormalizeAndValidate(); err == nil {
				t.Fatal("an uncompilable network policy was admitted")
			}
		})
	}
}

func TestAdmissionFillsPolicyWorkload(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Policy = &NetworkPolicy{Ingress: []IngressRule{{FromCIDR: "10.0.0.0/8", Port: 8080}}}
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	if got := scenario.Goal.Workload.Policy.Workload; got != "app" {
		t.Fatalf("policy workload = %q, want the goal's workload", got)
	}
}
