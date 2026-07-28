package control

import (
	"testing"
	"time"
)

const (
	oldImage = "registry.example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	newImage = "registry.example/web@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// canaryWorld builds a world where some replicas run the new image and the rest
// run the old one, all measured ready.
func canaryWorld(newReady, oldReady int) World {
	now := time.Unix(1700000000, 0).UTC()
	world := World{
		ObservedAt:  now,
		Nodes:       map[string]*Node{"base": {ID: "base", Healthy: true}},
		Allocations: map[string]*Allocation{},
		Routes: map[string]*Route{
			"web.example.com": {
				Host: "web.example.com", Workload: "web", Port: 443, Exposure: "public",
			},
		},
	}
	index := 0
	add := func(image string) {
		id := "web-" + string(rune('a'+index))
		world.Allocations[id] = &Allocation{
			ID: id, Workload: "web", Node: "base", Image: image,
			Phase: AllocationRunning, Ready: true,
			ReadyExpiresAt: now.Add(time.Minute),
			Address:        "10.42.0." + string(rune('2'+index)),
		}
		index++
	}
	for i := 0; i < newReady; i++ {
		add(newImage)
	}
	for i := 0; i < oldReady; i++ {
		add(oldImage)
	}
	return world
}

func canaryGoal(steps ...int) Goal {
	return Goal{
		APIVersion: APIVersion, ID: "web-public", Objective: "serve web",
		Workload: WorkloadSpec{
			Name: "web", Image: newImage, Replicas: 4, Port: 8080,
			Resources: Resources{CPUMillis: 100, MemoryMB: 128},
		},
		Route:  &RouteSpec{Host: "web.example.com", Port: 443, Exposure: "public"},
		Canary: &Canary{Steps: steps},
	}
}

// A new version with nothing ready receives no traffic. This is what makes a
// failed canary safe rather than merely gradual.
func TestCanaryGivesNoTrafficToAnUnreadyVersion(t *testing.T) {
	state := EvaluateCanary(canaryGoal(10, 50, 100), canaryWorld(0, 4))
	if state.Step != 0 {
		t.Fatalf("an unready version was authorized %d%% of traffic", state.Step)
	}
	if state.Complete {
		t.Fatal("a rollout with nothing ready reported complete")
	}
}

// The first deploy has nothing to split against, so withholding traffic would
// mean the service never serves at all.
func TestCanaryGivesAllTrafficWhenNothingElseServes(t *testing.T) {
	state := EvaluateCanary(canaryGoal(10, 50, 100), canaryWorld(2, 0))
	if state.Step != 100 {
		t.Fatalf("a first deploy was throttled to %d%%", state.Step)
	}
	if !state.Complete {
		t.Fatal("a fully migrated rollout did not report complete")
	}
}

// The authorized step follows the ready proportion, so it cannot exceed what the
// replica count can carry.
func TestCanaryStepFollowsReadyProportion(t *testing.T) {
	steps := []int{10, 50, 100}
	// One of four ready is 25%: the 10% step is supported, 50% is not.
	if got := EvaluateCanary(canaryGoal(steps...), canaryWorld(1, 3)).Step; got != 10 {
		t.Fatalf("one of four ready authorized %d%%, want 10%%", got)
	}
	// Two of four is 50%.
	if got := EvaluateCanary(canaryGoal(steps...), canaryWorld(2, 2)).Step; got != 50 {
		t.Fatalf("two of four ready authorized %d%%, want 50%%", got)
	}
	// Three of four is 75%: still the 50% step, since 100% is not supported.
	if got := EvaluateCanary(canaryGoal(steps...), canaryWorld(3, 1)).Step; got != 50 {
		t.Fatalf("three of four ready authorized %d%%, want 50%%", got)
	}
}

// A regression in readiness pulls traffic back, because the step is derived
// rather than latched.
func TestCanaryRetreatsWhenReadinessRegresses(t *testing.T) {
	goal := canaryGoal(10, 50, 100)
	advanced := EvaluateCanary(goal, canaryWorld(2, 2))
	if advanced.Step != 50 {
		t.Fatalf("expected 50%%, got %d%%", advanced.Step)
	}
	// The new replicas stop being measured ready.
	regressed := EvaluateCanary(goal, canaryWorld(0, 2))
	if regressed.Step != 0 {
		t.Fatalf("traffic stayed at %d%% after the new version failed", regressed.Step)
	}
}

// readyFor backdates the target side's readiness clock, which is what a hold is
// measured against.
func readyFor(world World, image string, age time.Duration) World {
	for _, allocation := range world.Allocations {
		if allocation.Image == image {
			allocation.ReadySince = world.Now().Add(-age)
		}
	}
	return world
}

// A declared hold must actually hold. Advancing on readiness alone would let a
// version that has been healthy for one second take the share meant for one
// that has been healthy for the whole interval.
func TestCanaryHoldsEachStepForItsDuration(t *testing.T) {
	goal := canaryGoal(10, 50, 100)
	goal.Canary.HoldFor = Duration(2 * time.Minute)

	// Two of four ready supports 50% by proportion, but nothing has been ready
	// long enough to leave the first step.
	fresh := readyFor(canaryWorld(2, 2), newImage, 10*time.Second)
	if got := EvaluateCanary(goal, fresh).Step; got != 10 {
		t.Fatalf("a canary with no elapsed hold authorized %d%%, want 10%%", got)
	}

	// One hold elapsed unlocks the next step, and the proportion still allows it.
	held := readyFor(canaryWorld(2, 2), newImage, 2*time.Minute+time.Second)
	state := EvaluateCanary(goal, held)
	if state.Step != 50 {
		t.Fatalf("an elapsed hold authorized %d%%, want 50%%", state.Step)
	}
	if state.HeldFor.Duration() < 2*time.Minute {
		t.Fatalf("HeldFor = %s, want at least 2m", state.HeldFor.Duration())
	}

	// The proportion still caps the ladder: a long hold cannot buy a share the
	// replica count cannot carry.
	long := readyFor(canaryWorld(1, 3), newImage, time.Hour)
	if got := EvaluateCanary(goal, long).Step; got != 10 {
		t.Fatalf("a long hold on one of four authorized %d%%, want 10%%", got)
	}
}

// The hold is measured from the least-established replica, so scaling the target
// side up starts the new step's hold rather than inheriting the previous one's.
func TestCanaryHoldFollowsTheNewestReplica(t *testing.T) {
	goal := canaryGoal(10, 50, 100)
	goal.Canary.HoldFor = Duration(2 * time.Minute)

	world := readyFor(canaryWorld(2, 2), newImage, time.Hour)
	// One target replica was replaced moments ago.
	for _, allocation := range world.Allocations {
		if allocation.Image == newImage {
			allocation.ReadySince = world.Now().Add(-time.Second)
			break
		}
	}
	if got := EvaluateCanary(goal, world).Step; got != 10 {
		t.Fatalf("a freshly added replica did not restart the hold: got %d%%", got)
	}
}

// A canary that declares no hold advances on readiness alone, which is the
// behaviour every existing rollout depends on.
func TestCanaryWithoutHoldAdvancesOnReadiness(t *testing.T) {
	goal := canaryGoal(10, 50, 100)
	if got := EvaluateCanary(goal, canaryWorld(2, 2)).Step; got != 50 {
		t.Fatalf("a canary with no hold authorized %d%%, want 50%%", got)
	}
}

// Weights split traffic between versions and sum to 100, so a gateway needs no
// floating point.
func TestWeightEndpointsSplitsByVersion(t *testing.T) {
	goal := canaryGoal(50, 100)
	world := canaryWorld(2, 2)
	endpoints := BuildDirectory(world, map[string]int{"web": 8080})["web"].Endpoints
	if len(endpoints) != 4 {
		t.Fatalf("expected four endpoints, got %d", len(endpoints))
	}

	weighted := WeightEndpoints(goal, world, endpoints)
	total := 0
	newTotal, oldTotal := 0, 0
	for _, endpoint := range weighted {
		total += endpoint.Weight
		if endpoint.Version == newImage {
			newTotal += endpoint.Weight
		} else {
			oldTotal += endpoint.Weight
		}
	}
	if total != 100 {
		t.Fatalf("weights sum to %d, want 100", total)
	}
	if newTotal != 50 || oldTotal != 50 {
		t.Fatalf("split is new=%d old=%d, want 50/50", newTotal, oldTotal)
	}
}

// An uneven share still sums exactly, with the remainder given out rather than
// lost to integer division.
func TestWeightEndpointsDistributesRemainder(t *testing.T) {
	goal := canaryGoal(10, 100)
	world := canaryWorld(1, 3)
	endpoints := BuildDirectory(world, map[string]int{"web": 8080})["web"].Endpoints

	weighted := WeightEndpoints(goal, world, endpoints)
	total := 0
	for _, endpoint := range weighted {
		total += endpoint.Weight
		if endpoint.Weight <= 0 {
			t.Fatalf("endpoint %s got no weight", endpoint.Allocation)
		}
	}
	if total != 100 {
		t.Fatalf("weights sum to %d, want 100", total)
	}
}

// Without a canary every endpoint is equal, which is the pre-existing behaviour.
func TestWeightEndpointsIsEqualWithoutACanary(t *testing.T) {
	goal := canaryGoal(50, 100)
	goal.Canary = nil
	world := canaryWorld(2, 2)
	endpoints := BuildDirectory(world, map[string]int{"web": 8080})["web"].Endpoints

	for _, endpoint := range WeightEndpoints(goal, world, endpoints) {
		if endpoint.Weight != 1 {
			t.Fatalf("endpoint %s was weighted %d without a canary",
				endpoint.Allocation, endpoint.Weight)
		}
	}
}

// A snapshot carries weights only while traffic is actually split, so an
// unchanged route does not look new to the gateway.
func TestSnapshotOmitsEqualWeights(t *testing.T) {
	goal := canaryGoal(50, 100)
	ports := map[string]int{"web": 8080}

	// Fully migrated: every endpoint runs the target, so there is nothing to split.
	migrated := BuildWeightedRouteSnapshots(canaryWorld(4, 0), ports, []Goal{goal})
	if len(migrated) != 1 {
		t.Fatalf("expected one snapshot, got %d", len(migrated))
	}
	if migrated[0].Weighted != nil {
		t.Fatalf("a fully migrated route carried weights: %+v", migrated[0].Weighted)
	}

	// Mid-canary: weights are present.
	splitting := BuildWeightedRouteSnapshots(canaryWorld(1, 3), ports, []Goal{goal})
	if splitting[0].Weighted == nil {
		t.Fatal("a splitting route carried no weights")
	}
}

// The unweighted builder keeps working for callers that have no goals to hand.
func TestBuildRouteSnapshotsStaysUnweighted(t *testing.T) {
	snapshots := BuildRouteSnapshots(canaryWorld(2, 2), map[string]int{"web": 8080})
	if len(snapshots) != 1 || snapshots[0].Weighted != nil {
		t.Fatalf("the unweighted builder produced weights: %+v", snapshots)
	}
	if len(snapshots[0].Endpoints) != 4 {
		t.Fatalf("endpoints were lost: %+v", snapshots[0].Endpoints)
	}
}

func TestCanaryValidation(t *testing.T) {
	if err := (&Canary{Steps: []int{10, 50, 100}}).Validate(); err != nil {
		t.Fatalf("a valid canary was refused: %v", err)
	}
	for name, canary := range map[string]*Canary{
		"no steps":         {Steps: nil},
		"not increasing":   {Steps: []int{50, 50, 100}},
		"decreasing":       {Steps: []int{50, 10, 100}},
		"never reaches100": {Steps: []int{10, 50}},
		"out of range":     {Steps: []int{0, 100}},
		"above 100":        {Steps: []int{10, 101}},
		"negative hold":    {Steps: []int{100}, HoldFor: Duration(-1)},
	} {
		if err := canary.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	var absent *Canary
	if err := absent.Validate(); err != nil {
		t.Fatalf("an absent canary was refused: %v", err)
	}
}

// A canary must apply to something that can actually receive split traffic.
func TestScenarioRejectsUnusableCanary(t *testing.T) {
	world := canaryWorld(0, 0)
	world.Nodes["base"].Capacity = Resources{CPUMillis: 4000, MemoryMB: 8192}

	// No route to split.
	noRoute := canaryGoal(100)
	noRoute.Route = nil
	if err := (&Scenario{Goal: noRoute, World: world}).NormalizeAndValidate(); err == nil {
		t.Fatal("a canary without a route was accepted")
	}

	// One replica cannot serve two versions at once.
	single := canaryGoal(50, 100)
	single.Workload.Replicas = 1
	if err := (&Scenario{Goal: single, World: world}).NormalizeAndValidate(); err == nil {
		t.Fatal("a canary on a single replica was accepted")
	}

	// A scheduled job has no steady traffic.
	scheduled := canaryGoal(100)
	scheduled.Workload.Schedule = &Schedule{Cron: "0 3 * * *"}
	if err := (&Scenario{Goal: scheduled, World: world}).NormalizeAndValidate(); err == nil {
		t.Fatal("a canary on a scheduled workload was accepted")
	}

	// The valid case still passes, so these rejections are specific.
	valid := canaryGoal(10, 50, 100)
	if err := (&Scenario{Goal: valid, World: world}).NormalizeAndValidate(); err != nil {
		t.Fatalf("a valid canary goal was refused: %v", err)
	}
}
