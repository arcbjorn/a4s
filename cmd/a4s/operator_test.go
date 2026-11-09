package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
)

func rollbackWorld() control.World {
	return control.World{
		Allocations: map[string]*control.Allocation{
			"web-0": {ID: "web-0", Workload: "web", Image: "img@sha256:bad", Phase: control.AllocationStopped, ExitCode: 1},
			"web-1": {ID: "web-1", Workload: "web", Image: "img@sha256:worse", Phase: control.AllocationStopped, Restarts: 3},
			"api-0": {ID: "api-0", Workload: "api", Image: "img@sha256:api", Phase: control.AllocationStopped, ExitCode: 1},
		},
		KnownGood: map[string]string{"web": "img@sha256:good"},
	}
}

func TestRollbackVersions(t *testing.T) {
	tests := []struct {
		name               string
		workload, from, to string
		world              control.World
		wantFailed, wantTo string
		wantErr            string
	}{
		{name: "derived from the world", workload: "web", world: rollbackWorld(),
			wantFailed: "img@sha256:bad", wantTo: "img@sha256:good"},
		{name: "explicit flags win", workload: "web", from: "img@sha256:x", to: "img@sha256:y",
			world: rollbackWorld(), wantFailed: "img@sha256:x", wantTo: "img@sha256:y"},
		{name: "workload required", world: rollbackWorld(), wantErr: "workload is required"},
		{name: "nothing failing", workload: "db", world: rollbackWorld(), wantErr: "no failing allocation"},
		{name: "no known good", workload: "api", world: rollbackWorld(), wantErr: "no known-good image"},
		{name: "same version", workload: "web", to: "img@sha256:bad", world: rollbackWorld(),
			wantErr: "version that failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failed, target, err := rollbackVersions(test.world, test.workload, test.from, test.to)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("want error containing %q, got %v", test.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if failed != test.wantFailed || target != test.wantTo {
				t.Fatalf("got %s -> %s, want %s -> %s", failed, target, test.wantFailed, test.wantTo)
			}
		})
	}
}

// With two failing images on record, the derived one must not depend on map
// iteration order.
func TestRollbackVersionsIsDeterministic(t *testing.T) {
	for i := 0; i < 200; i++ {
		failed, _, err := rollbackVersions(rollbackWorld(), "web", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if failed != "img@sha256:bad" {
			t.Fatalf("iteration %d derived %q", i, failed)
		}
	}
}

func TestApproveScopesListsEveryScope(t *testing.T) {
	stdout := mustRun(t, "approve", "--scopes")
	for scope := range control.ApprovalScopes {
		if !strings.Contains(stdout, scope) {
			t.Fatalf("scope %q missing from:\n%s", scope, stdout)
		}
	}
}

func TestApproveRefusesUngatedScope(t *testing.T) {
	_, _, err := runCLI(t, "approve", "--event-log", filepath.Join(t.TempDir(), "e.db"),
		"--goal", "g", "--scope", "root-access", "--operator", "arc", "--key", "k", "--key-id", "op")
	if err == nil || !strings.Contains(err.Error(), "not one the kernel gates on") {
		t.Fatalf("want an ungated-scope refusal, got %v", err)
	}
}

// The direct-log approval path records a verified grant, refuses to record the
// same grant twice, and revokes what it granted.
func TestApproveAndRevokeThroughTheLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), "events.db")
	key := generatedKey(t, "operator")
	approve := func(extra ...string) (string, error) {
		args := append([]string{"approve", "--event-log", log, "--goal", "homepage-public",
			"--scope", "public-route", "--operator", "arc", "--key", key, "--key-id", "op-1"}, extra...)
		stdout, _, err := runCLI(t, args...)
		return stdout, err
	}
	if stdout, err := approve("--reason", "launch"); err != nil || !strings.Contains(stdout, "approved homepage-public-public-route") {
		t.Fatalf("approve: %q %v", stdout, err)
	}
	if stdout, err := approve("--revoke"); err != nil || !strings.Contains(stdout, "revoked") {
		t.Fatalf("revoke: %q %v", stdout, err)
	}

	store, err := eventlog.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projector, err := control.NewDurableProjector(control.World{}, store)
	if err != nil {
		t.Fatal(err)
	}
	approval := projector.World().Approvals["homepage-public-public-route"]
	if approval == nil || approval.Granted || approval.RevokedAt.IsZero() {
		t.Fatalf("the approval should be recorded as revoked: %+v", approval)
	}
}

func TestApproveRevokeOfUnknownApprovalFails(t *testing.T) {
	log := filepath.Join(t.TempDir(), "events.db")
	key := generatedKey(t, "operator")
	_, _, err := runCLI(t, "approve", "--event-log", log, "--goal", "g", "--scope", "public-route",
		"--operator", "arc", "--key", key, "--key-id", "op-1", "--revoke")
	if err == nil || !strings.Contains(err.Error(), "never granted") {
		t.Fatalf("want a never-granted error, got %v", err)
	}
}
