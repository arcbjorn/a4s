package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

func operatorServer(t *testing.T) (*Server, string, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "events.log")
	server, err := Open(Config{
		EventLog: path, Base: baseWorld(),
		OperatorKeys: map[string]ed25519.PublicKey{"operator-arc": public},
	}, control.PlacementAgent{}, control.NetworkAgent{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server, path, private
}

func signedGrant(t *testing.T, key ed25519.PrivateKey, scope string) control.SignedApproval {
	t.Helper()
	now := time.Now().UTC()
	signed, err := control.SignApproval(control.ApprovalGrant{
		ID: "web-" + scope, GoalID: "web-public", Scope: scope, IssuedBy: "arc",
		IssuedAt: now, ExpiresAt: now.Add(time.Hour), Reason: "reviewed",
	}, "operator-arc", key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// This is the only path by which authority enters the system from outside.
func TestApproveRecordsAVerifiedGrant(t *testing.T) {
	server, _, key := operatorServer(t)
	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}

	grant, err := server.Approve(signedGrant(t, key, "public-route"))
	if err != nil {
		t.Fatalf("approve failed: %v", err)
	}
	if grant.Scope != "public-route" {
		t.Fatalf("unexpected grant: %+v", grant)
	}
	if !server.World().Approvals[grant.ID].Valid(time.Now()) {
		t.Fatal("expected the approval to authorize in the world")
	}
}

// A projection updated without a durable record would vanish on restart,
// silently withdrawing an authorization the operator was told had been granted.
func TestApprovalSurvivesRestart(t *testing.T) {
	server, path, key := operatorServer(t)
	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	grant, err := server.Approve(signedGrant(t, key, "public-route"))
	if err != nil {
		t.Fatal(err)
	}
	server.Close()

	reopened := openServer(t, path)
	defer reopened.Close()
	approval, ok := reopened.World().Approvals[grant.ID]
	if !ok {
		t.Fatal("the approval did not survive restart")
	}
	if !approval.Valid(time.Now()) {
		t.Fatal("the restored approval no longer authorizes")
	}
	if approval.IssuedBy != "arc" {
		t.Fatalf("expected the issuer to survive, got %q", approval.IssuedBy)
	}
}

// An agent holds no operator key, so a forged grant is its only route to
// authorizing itself.
func TestApproveRefusesUnknownKey(t *testing.T) {
	server, _, _ := operatorServer(t)
	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	_, attacker, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := server.Approve(signedGrant(t, attacker, "public-route")); err == nil {
		t.Fatal("a grant signed by an unknown key was accepted")
	}
	// The fixture's base world seeds one approval, so a refused grant is
	// detectable as the absence of a second.
	if _, leaked := server.World().Approvals["web-public-route"]; leaked {
		t.Fatal("a refused grant reached the world")
	}
	if len(server.Events()) != 0 {
		t.Fatal("a refused grant reached the durable log")
	}
}

// Approving a goal that was never submitted would let an operator pre-authorize
// something whose contents they have not seen.
func TestApproveRefusesUnknownGoal(t *testing.T) {
	server, _, key := operatorServer(t)

	_, err := server.Approve(signedGrant(t, key, "public-route"))
	if err == nil || !strings.Contains(err.Error(), "has not been submitted") {
		t.Fatalf("expected an unsubmitted goal to be refused, got %v", err)
	}
}

// Withdrawing another operator's approval is as consequential as issuing one.
func TestRevokeIsAuthenticatedAndDurable(t *testing.T) {
	server, path, key := operatorServer(t)
	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	grant, err := server.Approve(signedGrant(t, key, "public-route"))
	if err != nil {
		t.Fatal(err)
	}

	_, attacker, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Revoke(signedGrant(t, attacker, "public-route")); err == nil {
		t.Fatal("an unauthenticated revoke was accepted")
	}

	if err := server.Revoke(signedGrant(t, key, "public-route")); err != nil {
		t.Fatalf("authenticated revoke failed: %v", err)
	}
	if server.World().Approvals[grant.ID].Valid(time.Now()) {
		t.Fatal("a revoked grant still authorizes")
	}
	server.Close()

	// The withdrawal must survive restart too, or a revoked grant would come
	// back to life on the next start.
	reopened := openServer(t, path)
	defer reopened.Close()
	if reopened.World().Approvals[grant.ID].Valid(time.Now()) {
		t.Fatal("a revoked grant was restored as live")
	}
}

// An operator asking "is this approved" is often really asking "was it, and
// what happened".
func TestApprovalsListsLiveGrantsFirst(t *testing.T) {
	server, _, key := operatorServer(t)
	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Approve(signedGrant(t, key, "public-route")); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Approve(signedGrant(t, key, "destroy-stateful")); err != nil {
		t.Fatal(err)
	}
	if err := server.Revoke(signedGrant(t, key, "destroy-stateful")); err != nil {
		t.Fatal(err)
	}

	approvals := server.Approvals()
	var live, withdrawn int
	for _, approval := range approvals {
		if approval.Valid(time.Now()) {
			live++
			continue
		}
		if approval.ID == "web-destroy-stateful" && !approval.Granted {
			withdrawn++
		}
	}
	if withdrawn != 1 {
		t.Fatalf("expected the revoked grant to be listed as withdrawn: %+v", approvals)
	}
	if !approvals[0].Valid(time.Now()) {
		t.Fatal("expected a live grant to sort first")
	}
	if live == 0 {
		t.Fatal("expected the surviving grant to still authorize")
	}
}

// The unfiltered log stops being usable on the first busy day.
func TestQueryNarrowsHistory(t *testing.T) {
	server, _, key := operatorServer(t)
	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Approve(signedGrant(t, key, "public-route")); err != nil {
		t.Fatal(err)
	}

	all := server.Query(HistoryQuery{})
	if len(all) != len(server.Events()) {
		t.Fatalf("expected an unfiltered query to return everything, got %d of %d",
			len(all), len(server.Events()))
	}

	byKind := server.Query(HistoryQuery{Kind: control.EvidenceApprovalGranted})
	if len(byKind) != 1 {
		t.Fatalf("expected one approval event, got %d", len(byKind))
	}

	byGoal := server.Query(HistoryQuery{GoalID: "web-public"})
	if len(byGoal) == 0 {
		t.Fatal("expected the goal filter to match")
	}
	if none := server.Query(HistoryQuery{GoalID: "other"}); len(none) != 0 {
		t.Fatalf("expected no matches for an unknown goal, got %d", len(none))
	}

	// The limit keeps the most recent entries, which is what an operator
	// scanning for what just happened actually wants.
	limited := server.Query(HistoryQuery{Limit: 1})
	if len(limited) != 1 {
		t.Fatalf("expected the limit to apply, got %d", len(limited))
	}
	if limited[0].Sequence != all[len(all)-1].Sequence {
		t.Fatal("expected the limit to keep the most recent event")
	}

	// The window bounds both ends. An operator narrowing to "before this
	// happened" needs the upper bound as much as the lower one.
	latest := all[len(all)-1].At
	if before := server.Query(HistoryQuery{Until: latest.Add(-time.Nanosecond)}); len(before) >= len(all) {
		t.Fatalf("expected the upper bound to exclude the newest event, got %d of %d",
			len(before), len(all))
	}
	if window := server.Query(HistoryQuery{Since: latest, Until: latest}); len(window) == 0 {
		t.Fatal("expected an inclusive window to match the event on its bound")
	}
}

// A shutdown that races a query must refuse it rather than dereference the log
// the server has already closed.
func TestQueryAfterCloseReturnsNothing(t *testing.T) {
	server, _, _ := operatorServer(t)
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if events := server.Query(HistoryQuery{}); events != nil {
		t.Fatalf("expected a closed server to answer nothing, got %d events", len(events))
	}
}

// A server that has not been told who its operators are must not authorize
// public exposure or data destruction.
func TestServerWithoutOperatorKeysApprovesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	server := openServer(t, path)
	defer server.Close()
	if err := server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Approve(signedGrant(t, key, "public-route")); err == nil {
		t.Fatal("a server with no operator keys accepted an approval")
	}
}
