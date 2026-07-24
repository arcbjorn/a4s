package control

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// The kernel is the trusted core: it is the only component that turns untrusted
// input into authorization. These targets exist because architecture.md commits
// to keeping it small enough to fuzz thoroughly, and because every one of these
// entry points parses something a hostile or malfunctioning party supplied.
//
// Each target asserts an invariant rather than merely "does not panic". A
// decoder that survives arbitrary input by accepting it would pass a
// crash-only fuzz test while being exactly the bug worth finding.

// FuzzScenarioValidation checks that no input makes validation both succeed and
// leave a scenario the kernel would refuse to reason about.
//
// Validation is the front door: a scenario that validates is treated as
// well-formed everywhere downstream, so an accepted-but-malformed scenario
// would put the kernel in a state its later invariants assume cannot happen.
func FuzzScenarioValidation(f *testing.F) {
	valid, err := json.Marshal(validScenario())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"goal":{"api_version":"a4s.io/v1alpha1"}}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		var scenario Scenario
		if err := json.Unmarshal(raw, &scenario); err != nil {
			return
		}
		if err := scenario.NormalizeAndValidate(); err != nil {
			return
		}
		// Everything below is a property the rest of the kernel relies on.
		goal := scenario.Goal
		if goal.APIVersion != APIVersion {
			t.Fatalf("validated a scenario with api version %q", goal.APIVersion)
		}
		if goal.Workload.Replicas < 1 {
			t.Fatalf("validated %d replicas", goal.Workload.Replicas)
		}
		if goal.Workload.Privileged {
			t.Fatal("validated a privileged workload")
		}
		if goal.Workload.Resources.CPUMillis < 1 || goal.Workload.Resources.MemoryMB < 1 {
			t.Fatalf("validated non-positive resources: %+v", goal.Workload.Resources)
		}
		if !digestPattern.MatchString(goal.Workload.Image) {
			t.Fatalf("validated unpinned image %q", goal.Workload.Image)
		}
		if len(scenario.World.Nodes) == 0 {
			t.Fatal("validated a world with no nodes")
		}
		// Normalization must leave every map non-nil, since the rest of the
		// kernel indexes them without checking.
		if scenario.World.Allocations == nil || scenario.World.Volumes == nil ||
			scenario.World.Routes == nil || scenario.World.Queues == nil ||
			scenario.World.Approvals == nil {
			t.Fatal("normalization left a nil map")
		}
		// An agent workload's ceilings must be positive, or a validated goal
		// could reserve an unbounded budget.
		if runtime := goal.Workload.Runtime; runtime != nil {
			if runtime.Budget.Tokens < 1 || runtime.Budget.CostMillis < 1 ||
				runtime.Budget.WallSeconds < 1 || runtime.Budget.ToolCalls < 1 {
				t.Fatalf("validated an agent with a zero ceiling: %+v", runtime.Budget)
			}
			for _, grant := range runtime.Tools {
				if grant.Scope == "" {
					t.Fatalf("validated an unscoped tool grant %q", grant.Name)
				}
			}
		}
	})
}

// FuzzModelDiagnosisDecode checks that untrusted model output can never produce
// a diagnosis naming something the world does not contain.
//
// This is the property that keeps a hallucinated allocation from reaching an
// operator as observed fact. It matters more than crash-resistance: a decoder
// that accepted invented targets would be quietly wrong rather than loud.
func FuzzModelDiagnosisDecode(f *testing.F) {
	f.Add([]byte(`{"converged":false,"findings":[{"cause":"c","detail":"d","targets":["base"]}]}`))
	f.Add([]byte("```json\n{\"converged\":true,\"findings\":[]}\n```"))
	f.Add([]byte(`{"findings":[{"cause":"c","targets":["ghost"]}]}`))
	f.Add([]byte(`not json at all`))

	world := World{
		Nodes:       map[string]*Node{"base": {ID: "base", Healthy: true}},
		Allocations: map[string]*Allocation{"web-0": {ID: "web-0"}},
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		diagnosis, err := DecodeModelDiagnosis("web-public", raw, world)
		if err != nil {
			return
		}
		// The goal id comes from the caller, never from the model, so a model
		// cannot attribute its explanation to a different goal.
		if diagnosis.GoalID != "web-public" {
			t.Fatalf("decoder took the goal id from model output: %q", diagnosis.GoalID)
		}
		if len(diagnosis.Findings) > MaxModelFindings {
			t.Fatalf("decoded %d findings, above the limit", len(diagnosis.Findings))
		}
		for _, finding := range diagnosis.Findings {
			if finding.Cause == "" {
				t.Fatal("decoded a finding with no cause")
			}
			for _, target := range finding.Targets {
				if !knownTarget(target, world) {
					t.Fatalf("decoded an invented target %q", target)
				}
			}
		}
	})
}

// FuzzApprovalVerification checks that no byte sequence produces authority
// without a valid operator signature.
//
// Everything the kernel gates on — public exposure, destroying durable data,
// granting an agent mutating tools — is authorized by a grant that passed
// verification. A single accepting input here is a complete authorization
// bypass, which makes this the highest-value target in the package.
func FuzzApprovalVerification(f *testing.F) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	genuine, err := SignApproval(ApprovalGrant{
		ID: "web-public-route", GoalID: "web-public", Scope: "public-route",
		IssuedBy: "arc", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}, "operator-arc", private)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(genuine.GrantBytes, genuine.KeyID, genuine.Signature)
	f.Add([]byte(`{}`), "operator-arc", "")
	f.Add(genuine.GrantBytes, "unknown-key", genuine.Signature)

	keys := map[string]ed25519.PublicKey{"operator-arc": public}

	f.Fuzz(func(t *testing.T, grantBytes []byte, keyID, signature string) {
		grant, err := VerifyApproval(
			SignedApproval{GrantBytes: grantBytes, KeyID: keyID, Signature: signature},
			keys, now)
		if err != nil {
			return
		}
		// Anything that verifies must carry a real operator signature over
		// exactly these bytes.
		decoded, decodeErr := base64.StdEncoding.DecodeString(signature)
		if decodeErr != nil || !ed25519.Verify(public, grantBytes, decoded) {
			t.Fatal("verification accepted a grant without a valid signature")
		}
		if keyID != "operator-arc" || grant.KeyID != keyID {
			t.Fatalf("verification accepted key id %q", keyID)
		}
		if _, gated := ApprovalScopes[grant.Scope]; !gated {
			t.Fatalf("verification accepted ungated scope %q", grant.Scope)
		}
		if !grant.ExpiresAt.After(now) {
			t.Fatal("verification accepted an expired grant")
		}
		if grant.ExpiresAt.Sub(grant.IssuedAt) > MaxApprovalLifetime {
			t.Fatal("verification accepted an unbounded lifetime")
		}
	})
}

// FuzzProjection checks that applying arbitrary evidence to a world never
// produces a state the kernel's own invariants forbid.
//
// Evidence arrives from nodes, and a node is a weaker trust boundary than the
// server. The projection is where that input becomes the world every
// authorization decision reads.
func FuzzProjection(f *testing.F) {
	f.Add("allocation.created", "web-0", "base", "web")
	f.Add("approval.granted", "a1", "base", "web")
	f.Add("volume.attached", "data", "base", "web")
	f.Add("", "", "", "")

	f.Fuzz(func(t *testing.T, kind, target, node, workload string) {
		world := World{
			ObservedAt: time.Unix(1_700_000_000, 0).UTC(),
			Nodes: map[string]*Node{"base": {
				ID: "base", Healthy: true,
				Capacity: Resources{CPUMillis: 1000, MemoryMB: 1024},
				Images:   map[string]bool{}, Providers: map[string]ProviderReach{},
			}},
			Allocations: map[string]*Allocation{},
			Volumes:     map[string]*Volume{},
			Routes:      map[string]*Route{},
			Queues:      map[string]*Queue{},
			Approvals:   map[string]*Approval{},
		}
		next, err := Project(world, Evidence{
			Kind: kind, Target: target,
			Observed: map[string]string{
				"node": node, "workload": workload, "scope": "public-route",
				"goal": "web-public", "cpu_millis": "10", "memory_mb": "10",
			},
		})
		if err != nil {
			return
		}
		// Capacity accounting must never go negative, or a node would appear to
		// have more room than it has and the scheduler would overcommit it.
		for id, projected := range next.Nodes {
			if projected.Used.CPUMillis < 0 || projected.Used.MemoryMB < 0 {
				t.Fatalf("node %q has negative usage: %+v", id, projected.Used)
			}
			if projected.BudgetUsed.Tokens < 0 || projected.BudgetUsed.CostMillis < 0 {
				t.Fatalf("node %q has negative budget usage: %+v", id, projected.BudgetUsed)
			}
		}
		// An approval can only enter the world through a gated scope, whatever
		// evidence claimed.
		for id, approval := range next.Approvals {
			if _, gated := ApprovalScopes[approval.Scope]; !gated {
				t.Fatalf("projection admitted approval %q with ungated scope %q",
					id, approval.Scope)
			}
		}
		// A volume must never have two owners, which is the failure the whole
		// storage subsystem exists to prevent.
		owners := map[string]string{}
		for name, volume := range next.Volumes {
			if volume.Owner == "" {
				continue
			}
			if previous, taken := owners[volume.Owner]; taken {
				t.Fatalf("allocation %q owns both %q and %q",
					volume.Owner, previous, name)
			}
			owners[volume.Owner] = name
		}
	})
}
