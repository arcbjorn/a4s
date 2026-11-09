package control

import (
	"strings"
	"testing"
	"time"
)

// Every action the default policy can authorize must have an admission rule,
// or authorizing it would be pointless: its evidence could never be accepted.
// This is what catches a new action kind added without one.
func TestEveryGrantedActionHasAnEvidenceRule(t *testing.T) {
	for agent, grants := range DefaultPolicy().Grants {
		for kind := range grants {
			if _, ok := actionEvidence[kind]; !ok {
				t.Errorf("%s may propose %s, but no evidence for it is admissible", agent, kind)
			}
		}
	}
}

// No executed action may report operator authority, whatever it is.
func TestNoActionMayReportAuthority(t *testing.T) {
	authority := []string{
		EvidenceApprovalGranted, EvidenceApprovalRevoked, EvidenceDiagnosisRecorded,
		EvidenceNodeReachable, EvidenceNodeUnreachable,
	}
	for kind, rule := range actionEvidence {
		action := Action{Kind: kind, Target: "t", Node: "n", Image: "i", Volume: &VolumeRef{Name: "v"}}
		for _, forged := range authority {
			evidence := Evidence{Kind: forged, Target: rule.target(action)}
			if err := AdmitActionEvidence(action, evidence); err == nil {
				t.Errorf("%s admitted %q evidence", kind, forged)
			}
		}
	}
}

func TestAdmitActionEvidence(t *testing.T) {
	volume := &VolumeRef{Name: "app-data", MountPath: "/data"}
	tests := []struct {
		name     string
		action   Action
		evidence Evidence
		allowed  bool
	}{
		{"pull reports its image", Action{Kind: ActionPullImage, Image: testImage},
			Evidence{Kind: EvidenceImagePresent, Target: testImage}, true},
		{"pull about another image", Action{Kind: ActionPullImage, Image: testImage},
			Evidence{Kind: EvidenceImagePresent, Target: "other@sha256:x"}, false},
		{"create reports its allocation", Action{Kind: ActionCreateAllocation, Target: "app-0"},
			Evidence{Kind: EvidenceAllocationCreated, Target: "app-0"}, true},
		{"create about another allocation", Action{Kind: ActionCreateAllocation, Target: "app-0"},
			Evidence{Kind: EvidenceAllocationCreated, Target: "billing-0"}, false},
		{"create answered as running", Action{Kind: ActionCreateAllocation, Target: "app-0"},
			Evidence{Kind: EvidenceAllocationRunning, Target: "app-0"}, false},
		{"snapshot reports its volume", Action{Kind: ActionSnapshotVolume, Target: "app-0", Volume: volume},
			Evidence{Kind: EvidenceVolumeSnapshotted, Target: "app-data"}, true},
		{"snapshot about another volume", Action{Kind: ActionSnapshotVolume, Target: "app-0", Volume: volume},
			Evidence{Kind: EvidenceVolumeSnapshotted, Target: "billing-data"}, false},
		{"volume action without a volume", Action{Kind: ActionSnapshotVolume, Target: "app-0"},
			Evidence{Kind: EvidenceVolumeSnapshotted, Target: "app-data"}, false},
		{"drain still finishing", Action{Kind: ActionDrainAllocation, Target: "agent-0"},
			Evidence{Kind: EvidenceAgentDraining, Target: "agent-0"}, true},
		{"policy reports its node", Action{Kind: ActionApplyPolicy, Node: "base"},
			Evidence{Kind: EvidencePolicyApplied, Target: "base"}, true},
		{"policy about another node", Action{Kind: ActionApplyPolicy, Node: "base"},
			Evidence{Kind: EvidencePolicyApplied, Target: "other"}, false},
		{"pull answered with a cordon", Action{Kind: ActionPullImage, Image: testImage},
			Evidence{Kind: EvidenceNodeCordoned, Target: testImage}, false},
		{"unknown action kind", Action{Kind: "exec", Target: "x"},
			Evidence{Kind: EvidenceAllocationRunning, Target: "x"}, false},
		{"read-only probe is not executed", Action{Kind: ActionProbeReadiness, Target: "x"},
			Evidence{Kind: EvidenceAllocationReady, Target: "x"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := AdmitActionEvidence(test.action, test.evidence)
			if test.allowed && err != nil {
				t.Fatalf("legitimate evidence refused: %v", err)
			}
			if !test.allowed && err == nil {
				t.Fatal("evidence admitted that the action could not have produced")
			}
		})
	}
}

func TestAdmitProbeEvidence(t *testing.T) {
	ready := Check{Kind: CheckAllocationReady, Target: "app-0", Want: "true"}
	if err := AdmitProbeEvidence(ready, Evidence{Kind: EvidenceAllocationReady, Target: "app-0"}); err != nil {
		t.Fatalf("a readiness measurement was refused: %v", err)
	}
	refused := map[string]Evidence{
		"another target":   {Kind: EvidenceAllocationReady, Target: "billing-0"},
		"an approval":      {Kind: EvidenceApprovalGranted, Target: "app-0"},
		"the wrong signal": {Kind: EvidenceAgentReady, Target: "app-0"},
	}
	for name, evidence := range refused {
		if err := AdmitProbeEvidence(ready, evidence); err == nil {
			t.Errorf("a readiness check admitted %s", name)
		}
	}
	if err := AdmitProbeEvidence(Check{Kind: "bogus", Target: "app-0"},
		Evidence{Kind: EvidenceAllocationReady, Target: "app-0"}); err == nil {
		t.Error("an unknown check kind admitted evidence")
	}
}

// mintingExecutor is a compromised node: it performs every action faithfully
// except that it answers a pull with an operator approval.
type mintingExecutor struct{ *MemoryExecutor }

func (e mintingExecutor) Execute(action Action) (Evidence, error) {
	if action.Kind == ActionPullImage {
		return Evidence{Kind: EvidenceApprovalGranted, Target: "minted",
			ExpiresAt: time.Unix(1_700_000_000, 0).Add(time.Hour),
			Observed: map[string]string{
				"goal": "app-public", "scope": "public-route", "issued_by": "node-base",
			}}, nil
	}
	return e.MemoryExecutor.Execute(action)
}

func TestEngineRefusesEvidenceThatMintsAuthority(t *testing.T) {
	scenario := validScenario()
	scenario.World.Approvals = nil
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	memory := NewMemoryExecutor(scenario.World)
	engine := NewEngine(memory, PlacementAgent{}, NetworkAgent{})
	engine.Executor = mintingExecutor{memory}
	engine.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

	err := engine.Run(scenario.Goal, 6)
	if err == nil || !strings.Contains(err.Error(), "may not be answered") {
		t.Fatalf("want the forged evidence refused, got %v", err)
	}
	world := engine.World.World()
	if _, minted := world.Approvals["minted"]; minted {
		t.Fatal("executor evidence created an operator approval")
	}
	if len(world.Routes) != 0 {
		t.Fatal("a route was published on the strength of a forged approval")
	}
	if !containsEvent(engine.Events, EventGoalBlocked) {
		t.Fatal("refusing forged evidence must be recorded, not silent")
	}
}

// approvingProber answers every check with an approval.
type approvingProber struct{}

func (approvingProber) Probe(_ World, check Check) (Evidence, bool) {
	return Evidence{Kind: EvidenceApprovalGranted, Target: check.Target,
		Observed: map[string]string{"goal": "app-public", "scope": "public-route", "issued_by": "prober"}}, true
}

func TestEngineRefusesProbeEvidenceThatMintsAuthority(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(NewMemoryExecutor(scenario.World), PlacementAgent{}, NetworkAgent{})
	engine.WithProbers(approvingProber{})
	engine.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

	if err := engine.Run(scenario.Goal, 6); err == nil {
		t.Fatal("a prober reporting approvals must stop reconciliation")
	}
	for id, approval := range engine.World.World().Approvals {
		if approval.IssuedBy == "prober" {
			t.Fatalf("probe evidence created approval %q", id)
		}
	}
}
