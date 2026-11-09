package control

import (
	"strings"
	"testing"
	"time"
)

func scopeWorld() World {
	world := World{Nodes: map[string]*Node{"base": {ID: "base", Healthy: true,
		Labels: map[string]string{"pool": "base"}, Capacity: Resources{CPUMillis: 8000, MemoryMB: 16384}}}}
	world.normalize()
	world.ObservedAt = time.Unix(1_700_000_000, 0).UTC()
	world.Allocations["b-0"] = &Allocation{ID: "b-0", Workload: "b", Node: "base", Image: testImage,
		Resources: Resources{CPUMillis: 100, MemoryMB: 128}, Phase: AllocationCreated}
	return world
}

func authorizeAs(t *testing.T, agent string, goal Goal, world World, actions ...Action) error {
	t.Helper()
	proposal := Proposal{ID: "p", AgentID: agent, GoalID: goal.ID,
		BasedOnRevision: world.Revision, Actions: actions}
	return (Kernel{Policy: DefaultPolicy()}).Authorize(AgentDescriptor{ID: agent}, goal, world, proposal)
}

// Goal a's restore-volume approval must not overwrite goal b's data.
func TestRestoreCannotUseAnotherGoalsApproval(t *testing.T) {
	world := scopeWorld()
	world.Volumes["b-data"] = &Volume{Name: "b-data", Node: "base",
		Snapshots: map[string]string{"s1": "sum1"}, SnapshotOrder: []string{"s1"}, LastSnapshot: "s1"}
	world.Approvals["a-restore"] = &Approval{ID: "a-restore", GoalID: "a", Scope: "restore-volume",
		IssuedBy: "op", Granted: true}
	goal := Goal{ID: "a", Workload: WorkloadSpec{Name: "a",
		Volumes: []VolumeRef{{Name: "a-data", MountPath: "/data"}}}}
	ref := VolumeRef{Name: "b-data", MountPath: "/data", Checksum: "sum1"}
	err := authorizeAs(t, "storage-agent", goal, world, Action{ID: "restore",
		Kind: ActionRestoreSnapshot, Target: "b-data", Node: "base", Volume: &ref, Snapshot: "s1"})
	if err == nil || !strings.Contains(err.Error(), "not declared by goal") {
		t.Fatalf("goal a's approval restored goal b's volume: %v", err)
	}
}

// Secrets, tools, and volumes declared by one goal must not be installed into
// another workload's allocation.
func TestActionsCannotReachAnotherWorkloadsAllocation(t *testing.T) {
	secret := SecretRef{Name: "db-pass", Version: "v1", MountPath: "/run/secrets/db"}
	tests := map[string]func() (string, Goal, World, Action){
		"mount_secret": func() (string, Goal, World, Action) {
			goal := validScenario().Goal
			goal.Workload.Secrets = []SecretRef{secret}
			ref := secret
			return "placement-agent", goal, scopeWorld(), Action{ID: "m", Kind: ActionMountSecret,
				Target: "b-0", Node: "base", Secret: &ref}
		},
		"grant_tools": func() (string, Goal, World, Action) {
			goal := agentGoal()
			mutating := ToolGrant{Name: "repo.write", Scope: "github.com/x/y", Mutating: true}
			goal.Workload.Runtime.Tools = []ToolGrant{mutating}
			world := scopeWorld()
			world.Approvals["mut"] = &Approval{ID: "mut", GoalID: goal.ID, Scope: "agent-mutating-tools",
				IssuedBy: "op", Granted: true}
			return "placement-agent", goal, world, Action{ID: "g", Kind: ActionGrantTools,
				Target: "b-0", Node: "base", Tools: []ToolGrant{mutating}}
		},
		"attach_volume": func() (string, Goal, World, Action) {
			world := scopeWorld()
			world.Volumes["app-data"] = &Volume{Name: "app-data", Node: "base"}
			goal := validScenario().Goal
			goal.Workload.Volumes = []VolumeRef{{Name: "app-data", MountPath: "/data"}}
			ref := goal.Workload.Volumes[0]
			return "placement-agent", goal, world, Action{ID: "a", Kind: ActionAttachVolume,
				Target: "b-0", Node: "base", Volume: &ref}
		},
		"workload field lies": func() (string, Goal, World, Action) {
			goal := validScenario().Goal
			return "placement-agent", goal, scopeWorld(), Action{ID: "s", Kind: ActionStartAllocation,
				Target: "app-0", Workload: "b", Node: "base"}
		},
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			agent, goal, world, action := build()
			err := authorizeAs(t, agent, goal, world, action)
			if err == nil || !(strings.Contains(err.Error(), "belongs to workload") ||
				strings.Contains(err.Error(), "names workload")) {
				t.Fatalf("an action under goal %q reached workload b: %v", goal.ID, err)
			}
		})
	}
}

// Stopping another workload's replicas would bypass its availability floor,
// which is only computed for the goal being reconciled.
func TestStopCannotReachAnotherWorkload(t *testing.T) {
	world := scopeWorld()
	for _, id := range []string{"app-0", "app-1", "app-2"} {
		world.Allocations[id] = &Allocation{ID: id, Workload: "app", Node: "base", Image: testImage,
			Phase: AllocationRunning, Ready: true}
	}
	for _, id := range []string{"b-0", "b-1"} {
		world.Allocations[id] = &Allocation{ID: id, Workload: "b", Node: "base", Image: testImage,
			Phase: AllocationRunning, Ready: true}
	}
	goal := validScenario().Goal
	goal.Workload.Replicas = 3
	err := authorizeAs(t, "rollout-agent", goal, world,
		Action{ID: "s0", Kind: ActionStopAllocation, Target: "b-0", Node: "base"},
		Action{ID: "s1", Kind: ActionStopAllocation, Target: "b-1", Node: "base"})
	if err == nil {
		t.Fatal("every replica of workload b was stopped under goal app")
	}
}

// A create target is derived from the replica index. Free-form targets would
// add replicas beyond the goal or rename one out of its backoff.
func TestCreateTargetIsBoundToReplica(t *testing.T) {
	world := scopeWorld()
	world.Nodes["base"].Images[testImage] = true
	world.Backoff["app-1"] = &Backoff{Failures: 5, Until: world.ObservedAt.Add(time.Hour)}
	goal := validScenario().Goal
	goal.Workload.Replicas = 2
	create := func(target string, replica int) Action {
		return Action{ID: "c-" + target, Kind: ActionCreateAllocation, Target: target,
			Workload: "app", Node: "base", Image: testImage, Replica: replica,
			Resources: goal.Workload.Resources}
	}
	for name, action := range map[string]Action{
		"alias of replica 0":     create("app-0-extra", 0),
		"arbitrary name":         create("anything", 0),
		"rename out of backoff":  create("app-1x", 1),
		"replica index mismatch": create("app-1", 0),
	} {
		t.Run(name, func(t *testing.T) {
			if err := authorizeAs(t, "placement-agent", goal, world, action); err == nil {
				t.Fatalf("create %q as replica %d was authorized", action.Target, action.Replica)
			}
		})
	}
	if err := authorizeAs(t, "placement-agent", goal, world, create("app-0", 0)); err != nil {
		t.Fatalf("the canonical create was refused: %v", err)
	}
}

// A host already serving another workload is not this goal's to take.
func TestPublishRouteRefusesAnotherWorkloadsHost(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Route.Exposure = "tailnet"
	world := scenario.World
	world.normalize()
	world.ObservedAt = time.Unix(1_700_000_000, 0).UTC()
	world.Routes["app.example.com"] = &Route{Host: "app.example.com", Workload: "billing",
		Port: 443, Exposure: "public"}
	world.Allocations["app-0"] = &Allocation{ID: "app-0", Workload: "app", Node: "base", Image: testImage,
		Resources: scenario.Goal.Workload.Resources, Phase: AllocationRunning, Ready: true}
	publish := Action{ID: "publish", Kind: ActionPublishRoute, Target: "app.example.com",
		Workload: "app", Port: 443, Exposure: "tailnet"}

	err := validatePublishRoute(scenario.Goal, world, publish)
	if err == nil || !strings.Contains(err.Error(), "already routed") {
		t.Fatalf("a tailnet goal captured billing's public host: %v", err)
	}
	world.Routes["app.example.com"].Workload = "app"
	if err := validatePublishRoute(scenario.Goal, world, publish); err != nil {
		t.Fatalf("republishing the goal's own route was refused: %v", err)
	}
}
