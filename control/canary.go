package control

import (
	"fmt"
	"sort"
)

// Canary declares gradual traffic shifting for a rollout.
//
// Without it, a rollout replaces allocations one at a time and every ready
// replica takes an equal share of traffic the moment it becomes ready. That is
// safe for a change that works and abrupt for one that does not: the first new
// replica immediately serves 1/N of production. A canary makes the share
// explicit and advances it only on measured evidence.
//
// The weights are computed by the kernel from observed readiness, never proposed
// by an agent. An agent that could name its own traffic share could send all
// traffic to a version nothing has verified.
type Canary struct {
	// Steps are the percentages of traffic the new version takes, in order. The
	// last step must be 100, because a rollout that never reaches full traffic
	// never finishes.
	Steps []int `json:"steps"`
	// HoldFor is how long a step must be observed healthy before the next one.
	// Zero means advance as soon as the step's replicas are ready, which is
	// appropriate only when readiness is a strong signal on its own.
	HoldFor Duration `json:"hold_for,omitempty"`
}

// Validate checks a canary is usable before anything is authorized against it.
func (c *Canary) Validate() error {
	if c == nil {
		return nil
	}
	if len(c.Steps) == 0 {
		return fmt.Errorf("canary needs at least one step")
	}
	previous := 0
	for _, step := range c.Steps {
		if step < 1 || step > 100 {
			return fmt.Errorf("canary step %d must be between 1 and 100", step)
		}
		// Strictly increasing: a step that repeats or goes backwards would either
		// stall the rollout or shift traffic away from a version already carrying
		// it, neither of which is a rollout.
		if step <= previous {
			return fmt.Errorf("canary steps must strictly increase, got %d after %d", step, previous)
		}
		previous = step
	}
	if c.Steps[len(c.Steps)-1] != 100 {
		return fmt.Errorf("the last canary step must be 100, got %d", previous)
	}
	if c.HoldFor < 0 {
		return fmt.Errorf("canary hold cannot be negative")
	}
	return nil
}

// WeightedEndpoint is one endpoint and the share of traffic it should receive.
type WeightedEndpoint struct {
	Endpoint
	// Weight is a relative share, not a percentage. A gateway divides each
	// endpoint's weight by the total, so the numbers stay integers and no
	// rounding error accumulates across a snapshot.
	Weight int `json:"weight"`
	// Version is the image this endpoint runs, so an operator reading a snapshot
	// can see which side of a canary an endpoint is on.
	Version string `json:"version,omitempty"`
}

// CanaryState reports where a canary rollout currently stands.
type CanaryState struct {
	// Target is the image the goal asks for.
	Target string `json:"target"`
	// Step is the traffic percentage currently authorized for the target.
	Step int `json:"step"`
	// TargetReady and PreviousReady count ready allocations on each side.
	TargetReady   int `json:"target_ready"`
	PreviousReady int `json:"previous_ready"`
	// Complete is true when every ready allocation runs the target image.
	Complete bool `json:"complete"`
	// Advanced is true when this evaluation moved to a later step, which is what
	// makes the progression recordable as an event.
	Advanced bool `json:"advanced"`
}

// EvaluateCanary computes the authorized traffic share for a canary rollout.
//
// The step is derived from what is actually ready rather than from a counter:
// a canary that advanced on a timer would keep shifting traffic toward a version
// whose replicas had since failed their probes. Deriving it means a regression in
// readiness pulls traffic back automatically.
func EvaluateCanary(goal Goal, world World) CanaryState {
	canary := goal.Canary
	state := CanaryState{Target: goal.Workload.Image}
	if canary == nil {
		return state
	}
	now := world.Now()
	for _, allocation := range world.Allocations {
		if allocation.Workload != goal.Workload.Name {
			continue
		}
		if !allocation.ReadyAt(now) {
			continue
		}
		if allocation.Image == goal.Workload.Image {
			state.TargetReady++
		} else {
			state.PreviousReady++
		}
	}

	// Nothing else is serving, so the target carries everything. This covers the
	// first deploy of a workload, where withholding traffic would mean the
	// service never comes up at all.
	if state.PreviousReady == 0 {
		state.Step = 100
		state.Complete = state.TargetReady > 0
		return state
	}
	if state.TargetReady == 0 {
		// The new version has nothing ready, so it gets no traffic. This is the
		// case that makes a failed canary safe: a version that never becomes
		// ready never receives a request.
		state.Step = 0
		return state
	}

	// The authorized share is the largest step the ready proportion supports.
	// Weighting beyond that would send more traffic to the new version than its
	// replica count can carry.
	total := state.TargetReady + state.PreviousReady
	proportion := state.TargetReady * 100 / total
	state.Step = canary.Steps[0]
	for _, step := range canary.Steps {
		if step <= proportion {
			state.Step = step
		}
	}
	// The first step is always available once one replica is ready, so a canary
	// with a 10% first step still starts on a 3-replica workload where one ready
	// replica is 33%.
	if state.Step < canary.Steps[0] {
		state.Step = canary.Steps[0]
	}
	return state
}

// WeightEndpoints distributes traffic across endpoints according to a canary.
//
// Endpoints on the target version share the authorized step between them, and
// the rest share the remainder. Both sides are given whole-number weights that
// sum to 100, so a gateway needs no floating point and an operator reading the
// snapshot sees percentages.
func WeightEndpoints(goal Goal, world World, endpoints []Endpoint) []WeightedEndpoint {
	weighted := make([]WeightedEndpoint, 0, len(endpoints))
	if len(endpoints) == 0 {
		return weighted
	}

	versions := make(map[string]string, len(endpoints))
	for _, allocation := range world.Allocations {
		versions[allocation.ID] = allocation.Image
	}

	// Without a canary every endpoint is equal, which is the existing behaviour.
	if goal.Canary == nil {
		for _, endpoint := range endpoints {
			weighted = append(weighted, WeightedEndpoint{
				Endpoint: endpoint, Weight: 1, Version: versions[endpoint.Allocation],
			})
		}
		return weighted
	}

	state := EvaluateCanary(goal, world)
	var target, previous []Endpoint
	for _, endpoint := range endpoints {
		if versions[endpoint.Allocation] == goal.Workload.Image {
			target = append(target, endpoint)
		} else {
			previous = append(previous, endpoint)
		}
	}

	// One side being empty means there is nothing to split: whoever is serving
	// takes all of it, and a zero-weight snapshot would black-hole the route.
	switch {
	case len(target) == 0:
		return equalWeights(previous, versions)
	case len(previous) == 0:
		return equalWeights(target, versions)
	}

	weighted = append(weighted, shareAcross(target, state.Step, versions)...)
	weighted = append(weighted, shareAcross(previous, 100-state.Step, versions)...)
	sort.Slice(weighted, func(i, j int) bool {
		return weighted[i].Allocation < weighted[j].Allocation
	})
	return weighted
}

// shareAcross splits a percentage across endpoints, giving the remainder to the
// first so the total is exact rather than lost to integer division.
func shareAcross(endpoints []Endpoint, share int, versions map[string]string) []WeightedEndpoint {
	weighted := make([]WeightedEndpoint, 0, len(endpoints))
	if len(endpoints) == 0 || share <= 0 {
		return weighted
	}
	each := share / len(endpoints)
	remainder := share % len(endpoints)
	for index, endpoint := range endpoints {
		weight := each
		if index < remainder {
			weight++
		}
		if weight <= 0 {
			// A share too small to divide still needs somewhere to go, or the
			// endpoint would silently receive nothing.
			weight = 1
		}
		weighted = append(weighted, WeightedEndpoint{
			Endpoint: endpoint, Weight: weight, Version: versions[endpoint.Allocation],
		})
	}
	return weighted
}

func equalWeights(endpoints []Endpoint, versions map[string]string) []WeightedEndpoint {
	weighted := make([]WeightedEndpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		weighted = append(weighted, WeightedEndpoint{
			Endpoint: endpoint, Weight: 1, Version: versions[endpoint.Allocation],
		})
	}
	sort.Slice(weighted, func(i, j int) bool {
		return weighted[i].Allocation < weighted[j].Allocation
	})
	return weighted
}
