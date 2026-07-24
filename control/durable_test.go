package control

import (
	"reflect"
	"strings"
	"testing"
)

type recordedEvidence struct {
	items []Evidence
	err   error
}

func (r *recordedEvidence) ReplayEvidence() ([]Evidence, error) {
	if r.err != nil {
		return nil, r.err
	}
	return append([]Evidence(nil), r.items...), nil
}

func baseWorld() World {
	world := World{Nodes: map[string]*Node{
		"base": {
			ID: "base", Healthy: true, Labels: map[string]string{"pool": "base"},
			Capacity: Resources{CPUMillis: 4000, MemoryMB: 8192},
		},
	}}
	world.normalize()
	return world
}

func lifecycleEvidence() []Evidence {
	return []Evidence{
		{Kind: EvidenceImagePresent, Target: testImage, Observed: map[string]string{"node": "base", "image": testImage}},
		createdEvidence(),
		{Kind: EvidenceAllocationRunning, Target: "app-0", Observed: map[string]string{"node": "base"}},
		{Kind: EvidenceAllocationReady, Target: "app-0", Observed: map[string]string{"ready": "true"}},
		{Kind: EvidenceRouteReachable, Target: "app.example.com", Observed: map[string]string{
			"workload": "app", "port": "443", "exposure": "public",
		}},
	}
}

// The whole point of a durable projection: a server that restarts must rebuild
// exactly the state it had, from recorded evidence alone.
func TestDurableProjectionSurvivesRestart(t *testing.T) {
	source := &recordedEvidence{items: lifecycleEvidence()}

	live, err := NewDurableProjector(baseWorld(), &recordedEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range source.items {
		if err := live.Project(item); err != nil {
			t.Fatal(err)
		}
	}

	// A fresh process replays the same log from the same base.
	restarted, err := NewDurableProjector(baseWorld(), source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live.World(), restarted.World()) {
		t.Fatalf("restart produced a different world:\nlive:      %+v\nrestarted: %+v", live.World(), restarted.World())
	}
	rebuilt := restarted.World()
	if rebuilt.Allocations["app-0"] == nil || !rebuilt.Allocations["app-0"].Ready {
		t.Fatalf("replay lost allocation readiness: %+v", rebuilt.Allocations)
	}
	if rebuilt.Nodes["base"].Used != (Resources{CPUMillis: 100, MemoryMB: 128}) {
		t.Fatalf("replay lost capacity accounting: %+v", rebuilt.Nodes["base"].Used)
	}
}

// Rebuild must be a pure function of the base and the log, so calling it twice
// cannot drift and cannot accumulate state.
func TestRebuildIsRepeatable(t *testing.T) {
	source := &recordedEvidence{items: lifecycleEvidence()}
	projector, err := NewDurableProjector(baseWorld(), source)
	if err != nil {
		t.Fatal(err)
	}
	first := projector.World()
	if err := projector.Rebuild(source); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, projector.World()) {
		t.Fatalf("rebuild drifted:\nfirst:  %+v\nsecond: %+v", first, projector.World())
	}
}

// A log the projection cannot interpret must fail loudly. Silently skipping
// evidence would produce a world that disagrees with its own durable history.
func TestReplayRefusesUninterpretableEvidence(t *testing.T) {
	source := &recordedEvidence{items: []Evidence{{Kind: "invented.kind", Target: "app-0"}}}
	if _, err := NewDurableProjector(baseWorld(), source); err == nil ||
		!strings.Contains(err.Error(), "unknown evidence kind") {
		t.Fatalf("expected replay to reject unknown evidence, got %v", err)
	}
}

// Deletion evidence must replay to released capacity, not a leak.
func TestReplayReleasesCapacityForDeletedAllocation(t *testing.T) {
	items := append(lifecycleEvidence(),
		Evidence{Kind: EvidenceAllocationStopped, Target: "app-0", Observed: map[string]string{"exit_code": "0"}},
		Evidence{Kind: EvidenceAllocationDeleted, Target: "app-0", Observed: map[string]string{"deleted": "true"}},
	)
	projector, err := NewDurableProjector(baseWorld(), &recordedEvidence{items: items})
	if err != nil {
		t.Fatal(err)
	}
	world := projector.World()
	if world.Nodes["base"].Used != (Resources{}) {
		t.Fatalf("replayed deletion leaked capacity: %+v", world.Nodes["base"].Used)
	}
	if len(world.Allocations) != 0 {
		t.Fatalf("replayed deletion left allocations: %+v", world.Allocations)
	}
}
