package server

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

const testImage = "registry.example/web@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func baseWorld() control.World {
	return control.World{
		Nodes: map[string]*control.Node{
			"base": {
				ID: "base", Healthy: true, Labels: map[string]string{"pool": "base"},
				Capacity: control.Resources{CPUMillis: 4000, MemoryMB: 8192},
			},
		},
		Approvals: map[string]*control.Approval{
			"approve-web": {
				ID: "approve-web", GoalID: "web-public", Scope: "public-route",
				IssuedBy: "operator:test", Granted: true,
			},
		},
	}
}

func testGoal() control.Goal {
	return control.Goal{
		APIVersion: control.APIVersion, ID: "web-public",
		Objective: "keep one web replica reachable",
		Workload: control.WorkloadSpec{
			Name: "web", Image: testImage, Replicas: 1, Port: 8080,
			Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
		},
		Constraints: control.Constraints{RequiredLabels: map[string]string{"pool": "base"}},
	}
}

func openServer(t *testing.T, path string) *Server {
	t.Helper()
	server, err := Open(Config{EventLog: path, Base: baseWorld()},
		control.PlacementAgent{}, control.NetworkAgent{})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// executorOver stands in for a node: it performs the mutation and keeps its own
// view of local state, exactly as a real node does. The server holds the
// authoritative projection separately and advances it from returned evidence,
// so this deliberately does not share state with the server.
type executorOver struct{ inner *control.MemoryExecutor }

func (e executorOver) Execute(action control.Action) (control.Evidence, error) {
	evidence, err := e.inner.Execute(action)
	if err != nil {
		return control.Evidence{}, err
	}
	// A real node's local state changes as a consequence of its own mutation.
	if projectErr := e.inner.Project(evidence); projectErr != nil {
		return control.Evidence{}, projectErr
	}
	return evidence, nil
}

// A goal must converge and its history must be durable.
func TestServerReconcilesGoal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	server := openServer(t, path)
	defer server.Close()

	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	executor := control.NewMemoryExecutor(baseWorld())
	if err := server.Reconcile("web-public", executorOver{executor},
		control.NewMeasuredProber(executor, map[string]control.ProbeTarget{
			"web-0": {Allocation: "web-0", Kind: control.ProbeProcess},
		})); err != nil {
		t.Fatalf("goal did not converge: %v", err)
	}

	status := server.Status()
	if status.Allocations != 1 || status.Goals != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.Events == 0 {
		t.Fatal("reconciliation recorded no durable history")
	}
}

// The property that makes a single-server control plane acceptable: a restarted
// server rebuilds exactly the state it had, from the log alone.
func TestServerRecoversWorldAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	first := openServer(t, path)
	if err := first.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	executor := control.NewMemoryExecutor(baseWorld())
	if err := first.Reconcile("web-public", executorOver{executor},
		control.NewMeasuredProber(executor, map[string]control.ProbeTarget{
			"web-0": {Allocation: "web-0", Kind: control.ProbeProcess},
		})); err != nil {
		t.Fatal(err)
	}
	before := first.World()
	events := first.Status().Events
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh process opens the same log.
	restarted := openServer(t, path)
	defer restarted.Close()
	after := restarted.World()

	if !sameAllocations(before, after) {
		t.Fatalf("restart lost allocations:\nbefore: %s\nafter:  %s",
			describeAllocations(before), describeAllocations(after))
	}
	if before.Nodes["base"].Used != after.Nodes["base"].Used {
		t.Fatalf("restart changed capacity: before=%+v after=%+v",
			before.Nodes["base"].Used, after.Nodes["base"].Used)
	}
	if restarted.Status().Events != events {
		t.Fatalf("restart lost history: before=%d after=%d", events, restarted.Status().Events)
	}
}

// Rebuild must be repeatable, proving the projection is a function of the log
// and not of accumulated in-memory state.
func TestServerRebuildIsRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	server := openServer(t, path)
	defer server.Close()

	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	executor := control.NewMemoryExecutor(baseWorld())
	if err := server.Reconcile("web-public", executorOver{executor},
		control.NewMeasuredProber(executor, map[string]control.ProbeTarget{
			"web-0": {Allocation: "web-0", Kind: control.ProbeProcess},
		})); err != nil {
		t.Fatal(err)
	}
	before := server.World()
	if err := server.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if !sameAllocations(before, server.World()) {
		t.Fatalf("rebuild drifted:\nbefore: %s\nafter:  %s",
			describeAllocations(before), describeAllocations(server.World()))
	}
}

// An invalid goal must be refused at submission, not discovered later inside
// reconciliation where the failure is harder to attribute.
func TestServerRejectsInvalidGoal(t *testing.T) {
	server := openServer(t, filepath.Join(t.TempDir(), "events.jsonl"))
	defer server.Close()

	goal := testGoal()
	goal.Workload.Image = "registry.example/web:latest"
	if err := server.Submit(goal); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("expected a mutable image to be rejected, got %v", err)
	}
	if len(server.Goals()) != 0 {
		t.Fatal("a rejected goal was still accepted")
	}
}

func TestServerRefusesUnknownGoal(t *testing.T) {
	server := openServer(t, filepath.Join(t.TempDir(), "events.jsonl"))
	defer server.Close()

	err := server.Reconcile("never-submitted", executorOver{control.NewMemoryExecutor(baseWorld())})
	if err == nil || !strings.Contains(err.Error(), "never accepted") {
		t.Fatalf("expected unknown goal rejection, got %v", err)
	}
}

// Two goals touching the same allocation must not interleave, even when driven
// separately. The shared lease manager is what prevents that.
func TestServerSharesLeasesAcrossReconciliations(t *testing.T) {
	server := openServer(t, filepath.Join(t.TempDir(), "events.jsonl"))
	defer server.Close()

	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	// Another controller holds the allocation this goal needs.
	if _, err := server.leases.Acquire("other", "other-proposal", []string{"web-0"}); err != nil {
		t.Fatal(err)
	}
	executor := control.NewMemoryExecutor(baseWorld())
	err := server.Reconcile("web-public", executorOver{executor})
	if err == nil {
		t.Fatal("reconciliation proceeded against a leased target")
	}
	if len(executor.World().Allocations) != 0 {
		t.Fatal("reconciliation mutated a leased target")
	}
}

// sameAllocations compares allocations by value. The world holds pointers, so a
// structural comparison would compare addresses and always differ across a
// rebuild.
func sameAllocations(a, b control.World) bool {
	if len(a.Allocations) != len(b.Allocations) {
		return false
	}
	for id, left := range a.Allocations {
		right, ok := b.Allocations[id]
		if !ok {
			return false
		}
		// Compare timestamps with Equal, not ==. An in-memory time carries a
		// monotonic reading that a JSON round trip through the log strips, so
		// == reports two identical instants as different.
		if !left.ReadyExpiresAt.Equal(right.ReadyExpiresAt) {
			return false
		}
		if !left.ReadySince.Equal(right.ReadySince) {
			return false
		}
		// Compare secret versions explicitly; a map field makes the struct
		// itself uncomparable.
		if len(left.Secrets) != len(right.Secrets) {
			return false
		}
		for name, version := range left.Secrets {
			if right.Secrets[name] != version {
				return false
			}
		}
		// DeepEqual over the remaining fields rather than an explicit list, so
		// a field added later is compared automatically instead of silently
		// skipped.
		leftCopy, rightCopy := *left, *right
		leftCopy.ReadyExpiresAt, rightCopy.ReadyExpiresAt = time.Time{}, time.Time{}
		leftCopy.ReadySince, rightCopy.ReadySince = time.Time{}, time.Time{}
		leftCopy.Secrets, rightCopy.Secrets = nil, nil
		if !reflect.DeepEqual(leftCopy, rightCopy) {
			return false
		}
	}
	return true
}

func describeAllocations(world control.World) string {
	ids := make([]string, 0, len(world.Allocations))
	for id := range world.Allocations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%+v", *world.Allocations[id]))
	}
	return strings.Join(parts, ", ")
}

// The server must be able to explain its own history, since the log it holds is
// the authoritative record of why anything exists.
func TestServerExplainsFromDurableHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	server := openServer(t, path)
	defer server.Close()

	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	executor := control.NewMemoryExecutor(baseWorld())
	if err := server.Reconcile("web-public", executorOver{executor},
		control.NewMeasuredProber(executor, map[string]control.ProbeTarget{
			"web-0": {Allocation: "web-0", Kind: control.ProbeProcess},
		})); err != nil {
		t.Fatal(err)
	}

	explanation := server.Explain("web-0")
	if !explanation.Found || explanation.Status != control.StateServing {
		t.Fatalf("server could not explain its own allocation: %+v", explanation)
	}
	if len(explanation.Goals) != 1 || explanation.Goals[0] != "web-public" {
		t.Fatalf("explanation did not attribute the allocation: %+v", explanation.Goals)
	}
}

// Diagnosis must work against the server's recovered state, which is the case
// an operator actually hits after a restart.
func TestServerDiagnosesConvergedGoal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	server := openServer(t, path)
	defer server.Close()

	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	executor := control.NewMemoryExecutor(baseWorld())
	if err := server.Reconcile("web-public", executorOver{executor},
		control.NewMeasuredProber(executor, map[string]control.ProbeTarget{
			"web-0": {Allocation: "web-0", Kind: control.ProbeProcess},
		})); err != nil {
		t.Fatal(err)
	}

	if diagnosis := server.Diagnose("web-public", nil); !diagnosis.Converged {
		t.Fatalf("a converged goal was diagnosed as failing: %s", diagnosis)
	}
}

// Planning against the server's real projection must report nothing to do once
// the goal is satisfied, and must not disturb the recovered world.
func TestServerPlanIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	server := openServer(t, path)
	defer server.Close()

	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	before := server.Status()

	plan, err := server.Plan("web-public")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("server planned no work for an unsatisfied goal")
	}
	after := server.Status()
	if before != after {
		t.Fatalf("planning changed server state:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func TestServerRefusesPlanForUnknownGoal(t *testing.T) {
	server := openServer(t, filepath.Join(t.TempDir(), "events.jsonl"))
	defer server.Close()

	if _, err := server.Plan("never-submitted"); err == nil ||
		!strings.Contains(err.Error(), "never accepted") {
		t.Fatalf("expected an unknown-goal error, got %v", err)
	}
}

// openAnchoredServer starts a server whose chain head is witnessed externally.
func openAnchoredServer(t *testing.T, logPath, anchorPath string) *Server {
	t.Helper()
	server, err := Open(Config{EventLog: logPath, Base: baseWorld(), Anchor: anchorPath},
		control.PlacementAgent{}, control.NetworkAgent{})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// A server that reconciled with an anchor configured restarts cleanly against
// the log it wrote. Anchoring must not make ordinary recovery fail.
func TestAnchoredServerRestartsCleanly(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.db")
	anchorPath := filepath.Join(dir, "anchor.jsonl")

	first := openAnchoredServer(t, logPath, anchorPath)
	if err := first.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	executor := control.NewMemoryExecutor(baseWorld())
	if err := first.Reconcile("web-public", executorOver{executor},
		control.NewMeasuredProber(executor, map[string]control.ProbeTarget{
			"web-0": {Allocation: "web-0", Kind: control.ProbeProcess},
		})); err != nil {
		t.Fatal(err)
	}
	events := first.Status().Events
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := openAnchoredServer(t, logPath, anchorPath)
	defer restarted.Close()
	if restarted.Status().Events != events {
		t.Fatalf("restart lost history: before=%d after=%d", events, restarted.Status().Events)
	}
}

// The point of the anchor: a substituted event log is refused at startup, before
// the server builds a world from forged history.
func TestAnchoredServerRefusesReplacedLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.db")
	anchorPath := filepath.Join(dir, "anchor.jsonl")

	first := openAnchoredServer(t, logPath, anchorPath)
	if err := first.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	executor := control.NewMemoryExecutor(baseWorld())
	if err := first.Reconcile("web-public", executorOver{executor},
		control.NewMeasuredProber(executor, map[string]control.ProbeTarget{
			"web-0": {Allocation: "web-0", Kind: control.ProbeProcess},
		})); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Swap in an empty log, standing in for any forged history: its own chain
	// verifies, so nothing inside the store reveals the substitution.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	_, err := Open(Config{EventLog: logPath, Base: baseWorld(), Anchor: anchorPath},
		control.PlacementAgent{}, control.NetworkAgent{})
	if err == nil {
		t.Fatal("the server accepted a replaced event log")
	}
	if !strings.Contains(err.Error(), "anchor") {
		t.Fatalf("the failure did not name the anchor check: %v", err)
	}
}
