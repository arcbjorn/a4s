package control

import (
	"errors"
	"fmt"
	"strings"
)

// Plan is what reconciliation would do to the current world, computed without
// touching anything.
//
// This is exact rather than approximate because the kernel simulates every
// authorized plan against a cloned world before its first mutation anyway.
// Dry-run reuses that same path, so a plan cannot disagree with what execution
// would do for reasons of drift between two code paths.
type Plan struct {
	GoalID string `json:"goal_id"`
	// Achieved reports that the current world already satisfies the goal, so
	// reconciliation would do nothing.
	Achieved bool          `json:"achieved"`
	Steps    []PlannedStep `json:"steps"`
	// Blocked records why an agent could not produce a usable plan. A blocked
	// dry run is the useful case: it surfaces the obstacle before any mutation.
	Blocked []Obstacle `json:"blocked,omitempty"`
	// Consequences describes the world the plan would produce.
	Consequences Consequences `json:"consequences"`
}

// PlannedStep is one authorized action the plan would take.
type PlannedStep struct {
	AgentID    string     `json:"agent_id"`
	ProposalID string     `json:"proposal_id"`
	Reasoning  string     `json:"reasoning"`
	ActionID   string     `json:"action_id"`
	Kind       ActionKind `json:"kind"`
	Target     string     `json:"target"`
	Node       string     `json:"node,omitempty"`
	Image      string     `json:"image,omitempty"`
	// Assumed marks a step that only runs if an earlier step's outcome is
	// confirmed by evidence the dry run cannot produce. Publishing a route, for
	// example, requires a probe to measure the workload as ready. Simulation
	// assumes that optimistically so the kernel can authorize a whole plan;
	// reporting the assumption keeps the plan from over-promising.
	Assumed bool `json:"assumed,omitempty"`
}

// Obstacle is a reason reconciliation could not proceed.
type Obstacle struct {
	AgentID string `json:"agent_id"`
	// Stage distinguishes an agent declining to plan from the kernel refusing
	// a plan it was given. They mean different things to an operator.
	Stage  string `json:"stage"`
	Reason string `json:"reason"`
	// RollbackTarget is set when the obstacle is a failed rollout, naming the
	// image last observed serving.
	RollbackTarget string `json:"rollback_target,omitempty"`
}

// Consequences is the difference between the current world and the world the
// plan would produce.
type Consequences struct {
	StartRevision   uint64            `json:"start_revision"`
	CreatedAllocs   []string          `json:"created_allocations,omitempty"`
	RemovedAllocs   []string          `json:"removed_allocations,omitempty"`
	PublishedRoutes []string          `json:"published_routes,omitempty"`
	NodeUsage       map[string]string `json:"node_usage,omitempty"`
}

// DryRun computes what reconciliation would do without mutating anything.
//
// It runs the real agents and the real kernel against a clone of the observed
// world. No executor is involved, so no host is touched and no evidence is
// produced: the simulated world advances only through the kernel's own
// simulation of an authorized plan.
func DryRun(kernel Kernel, world World, goal Goal, agents ...Agent) Plan {
	plan := Plan{GoalID: goal.ID}
	plan.Consequences.StartRevision = world.Revision

	// The plan is computed against a clone. Even a bug in an agent or in
	// simulation cannot reach the caller's world.
	sim := cloneWorld(world)
	if goalAchieved(goal, sim) {
		plan.Achieved = true
		plan.Consequences = diffWorlds(world, sim)
		return plan
	}

	before := cloneWorld(sim)
	// Once simulation has assumed an outcome that only evidence can confirm,
	// every later step is contingent on that assumption holding.
	assumed := false
	for _, agent := range agents {
		descriptor := agent.Descriptor()
		proposal, err := agent.Propose(goal, sim)
		if err != nil {
			obstacle := Obstacle{AgentID: descriptor.ID, Stage: "propose", Reason: err.Error()}
			var rollback *RollbackRequired
			if errors.As(err, &rollback) {
				obstacle.RollbackTarget = rollback.KnownGood
			}
			plan.Blocked = append(plan.Blocked, obstacle)
			continue
		}
		if len(proposal.Actions) == 0 {
			continue
		}
		if err := kernel.Authorize(descriptor, goal, sim, proposal); err != nil {
			plan.Blocked = append(plan.Blocked, Obstacle{
				AgentID: descriptor.ID, Stage: "authorize", Reason: err.Error(),
			})
			continue
		}
		for _, action := range proposal.Actions {
			plan.Steps = append(plan.Steps, PlannedStep{
				AgentID: descriptor.ID, ProposalID: proposal.ID,
				Reasoning: proposal.Reasoning, ActionID: action.ID,
				Kind: action.Kind, Target: action.Target,
				Node: action.Node, Image: action.Image, Assumed: assumed,
			})
			if action.Kind == ActionStartAllocation {
				// Readiness is measured by an independent probe, never by
				// simulation. Anything planned after this point is contingent.
				assumed = true
			}
			// Advance the simulated world so a later agent plans against the
			// state its predecessor would have produced, exactly as it would
			// during real reconciliation.
			if err := simulateAction(&sim, action); err != nil {
				plan.Blocked = append(plan.Blocked, Obstacle{
					AgentID: descriptor.ID, Stage: "simulate", Reason: err.Error(),
				})
				break
			}
		}
		sim.Revision++
	}

	plan.Consequences = diffWorlds(before, sim)
	return plan
}

// diffWorlds reports what changed between two worlds.
func diffWorlds(before, after World) Consequences {
	consequences := Consequences{
		StartRevision: before.Revision,
		NodeUsage:     make(map[string]string),
	}
	for id := range after.Allocations {
		if _, existed := before.Allocations[id]; !existed {
			consequences.CreatedAllocs = append(consequences.CreatedAllocs, id)
		}
	}
	for id := range before.Allocations {
		if _, remains := after.Allocations[id]; !remains {
			consequences.RemovedAllocs = append(consequences.RemovedAllocs, id)
		}
	}
	for host := range after.Routes {
		if _, existed := before.Routes[host]; !existed {
			consequences.PublishedRoutes = append(consequences.PublishedRoutes, host)
		}
	}
	for id, node := range after.Nodes {
		previous, existed := before.Nodes[id]
		if !existed || previous.Used == node.Used {
			continue
		}
		consequences.NodeUsage[id] = fmt.Sprintf("%dm/%dMB -> %dm/%dMB",
			previous.Used.CPUMillis, previous.Used.MemoryMB,
			node.Used.CPUMillis, node.Used.MemoryMB)
	}
	sortStrings(consequences.CreatedAllocs)
	sortStrings(consequences.RemovedAllocs)
	sortStrings(consequences.PublishedRoutes)
	if len(consequences.NodeUsage) == 0 {
		consequences.NodeUsage = nil
	}
	return consequences
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// String renders the plan for an operator.
func (p Plan) String() string {
	var out strings.Builder
	if p.Achieved {
		fmt.Fprintf(&out, "goal %s is already satisfied; no actions would run\n", p.GoalID)
		return out.String()
	}
	if len(p.Steps) == 0 && len(p.Blocked) == 0 {
		fmt.Fprintf(&out, "goal %s has no available actions and no reported obstacle\n", p.GoalID)
		return out.String()
	}

	if len(p.Steps) > 0 {
		fmt.Fprintf(&out, "goal %s would run %d action(s):\n\n", p.GoalID, len(p.Steps))
		lastProposal := ""
		for _, step := range p.Steps {
			if step.ProposalID != lastProposal {
				fmt.Fprintf(&out, "  %s: %s\n", step.AgentID, step.Reasoning)
				lastProposal = step.ProposalID
			}
			target := step.Target
			if step.Node != "" {
				target += " on " + step.Node
			}
			if step.Assumed {
				target += "  (only if readiness is confirmed)"
			}
			fmt.Fprintf(&out, "    %-20s %s\n", step.Kind, target)
		}
	}

	if len(p.Blocked) > 0 {
		out.WriteString("\nblocked:\n")
		for _, obstacle := range p.Blocked {
			fmt.Fprintf(&out, "  %s (%s): %s\n", obstacle.AgentID, obstacle.Stage, obstacle.Reason)
			if obstacle.RollbackTarget != "" {
				fmt.Fprintf(&out, "    last known-good image: %s\n", obstacle.RollbackTarget)
			}
		}
	}

	consequences := p.Consequences
	if len(consequences.CreatedAllocs) > 0 || len(consequences.RemovedAllocs) > 0 ||
		len(consequences.PublishedRoutes) > 0 || len(consequences.NodeUsage) > 0 {
		out.WriteString("\nresulting world:\n")
		if len(consequences.CreatedAllocs) > 0 {
			fmt.Fprintf(&out, "  created allocations: %s\n", strings.Join(consequences.CreatedAllocs, ", "))
		}
		if len(consequences.RemovedAllocs) > 0 {
			fmt.Fprintf(&out, "  removed allocations: %s\n", strings.Join(consequences.RemovedAllocs, ", "))
		}
		if len(consequences.PublishedRoutes) > 0 {
			fmt.Fprintf(&out, "  published routes: %s\n", strings.Join(consequences.PublishedRoutes, ", "))
		}
		for node, usage := range consequences.NodeUsage {
			fmt.Fprintf(&out, "  node %s: %s\n", node, usage)
		}
	}
	return out.String()
}
