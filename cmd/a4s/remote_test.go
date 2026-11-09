package main

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
	"github.com/arcbjorn/a4s/server"
)

// remoteFixture is a real control plane behind a real operator API, with an
// operator keypair on disk the way the CLI expects to find it.
type remoteFixture struct {
	url      string
	keyPath  string
	instance *server.Server
}

func newRemoteFixture(t *testing.T) remoteFixture {
	t.Helper()
	keyPath := generatedKey(t, "operator")
	public, err := loadPublicKey(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	scenario, err := loadScenario(example("web-service.json"))
	if err != nil {
		t.Fatal(err)
	}
	eventLog := filepath.Join(t.TempDir(), "events.db")
	instance, err := server.Open(server.Config{
		EventLog: eventLog, Base: scenario.World,
		OperatorKeys: map[string]ed25519.PublicKey{"op-1": public},
	}, control.PlacementAgent{}, control.NetworkAgent{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close() })
	api := httptest.NewServer(server.NewAPI(instance, server.APIConfig{}).Handler())
	t.Cleanup(api.Close)
	return remoteFixture{url: api.URL, keyPath: keyPath, instance: instance}
}

// args returns the connection flags every remote command takes.
func (f remoteFixture) args(command ...string) []string {
	return append(command, "--server", f.url, "--key-id", "op-1", "--operator-key", f.keyPath)
}

func TestSubmitAndStatus(t *testing.T) {
	fixture := newRemoteFixture(t)
	stdout := mustRun(t, fixture.args("submit", "--file", example("web-service.json"))...)
	if strings.TrimSpace(stdout) != "submitted goal homepage-public" {
		t.Fatalf("unexpected submit output %q", stdout)
	}
	if goals := fixture.instance.Goals(); len(goals) != 1 || goals[0].ID != "homepage-public" {
		t.Fatalf("the server did not accept the goal: %+v", goals)
	}

	text := mustRun(t, fixture.args("status")...)
	if !strings.Contains(text, "1 goals") || !strings.Contains(text, "2 nodes") {
		t.Fatalf("unexpected status output %q", text)
	}
	if strings.Contains(text, "holding back") {
		t.Fatalf("a healthy cluster should not report safeguards: %q", text)
	}

	var reported server.Status
	if err := json.Unmarshal([]byte(mustRun(t, fixture.args("status", "--json")...)), &reported); err != nil {
		t.Fatal(err)
	}
	if reported.Goals != 1 || reported.Nodes != 2 {
		t.Fatalf("unexpected status %+v", reported)
	}
}

func TestSubmitAcceptsABareGoalDocument(t *testing.T) {
	fixture := newRemoteFixture(t)
	scenario, err := loadScenario(example("web-service.json"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(scenario.Goal)
	if err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, filepath.Join(t.TempDir(), "goal.json"), string(encoded))
	mustRun(t, fixture.args("submit", "--file", path)...)
}

func TestLoadGoalRejectsDocumentWithoutGoal(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "empty.json"), `{"world": {}}`)
	if _, err := loadGoal(path); err == nil {
		t.Fatal("a document with neither a goal nor a scenario was accepted")
	}
}

func TestRemoteCommandsRequireOperatorKey(t *testing.T) {
	for _, args := range [][]string{
		{"status"}, {"events"}, {"submit", "--file", "x"}, {"cordon", "--node", "base"},
	} {
		_, _, err := runCLI(t, append(args, "--server", "http://127.0.0.1:1")...)
		if err == nil || !strings.Contains(err.Error(), "key-id and operator-key are required") {
			t.Fatalf("%s without a key: want a missing-flag error, got %v", args[0], err)
		}
	}
}

// Authority originates with an operator key the server trusts. A key it does
// not know is refused, however well-formed the request.
func TestServerRefusesUntrustedOperatorKey(t *testing.T) {
	fixture := newRemoteFixture(t)
	stranger := generatedKey(t, "stranger")
	_, _, err := runCLI(t, "status", "--server", fixture.url, "--key-id", "op-1", "--operator-key", stranger)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want the untrusted key refused, got %v", err)
	}
}

func TestCordonAndUndo(t *testing.T) {
	fixture := newRemoteFixture(t)
	stdout := mustRun(t, fixture.args("cordon", "--node", "base", "--reason", "kernel upgrade")...)
	if !strings.Contains(stdout, "node base will accept no new allocations") ||
		!strings.Contains(stdout, "drained") {
		t.Fatalf("unexpected cordon output %q", stdout)
	}
	if !fixture.instance.World().Nodes["base"].Cordoned {
		t.Fatal("the cordon did not take effect")
	}
	if status := mustRun(t, fixture.args("status")...); !strings.Contains(status, "holding back: 1/2 nodes schedulable") {
		t.Fatalf("status should report the cordon: %q", status)
	}

	undo := mustRun(t, fixture.args("cordon", "--node", "base", "--undo")...)
	if strings.TrimSpace(undo) != "node base is schedulable again" {
		t.Fatalf("unexpected uncordon output %q", undo)
	}
	if fixture.instance.World().Nodes["base"].Cordoned {
		t.Fatal("the uncordon did not take effect")
	}
}

func TestCordonUnknownNodeFails(t *testing.T) {
	fixture := newRemoteFixture(t)
	if _, _, err := runCLI(t, fixture.args("cordon", "--node", "ghost")...); err == nil {
		t.Fatal("cordoning a node the server does not know succeeded")
	}
}

func TestRemoteEventsFilters(t *testing.T) {
	fixture := newRemoteFixture(t)
	if none := mustRun(t, fixture.args("events")...); strings.TrimSpace(none) != "no matching events" {
		t.Fatalf("an empty log should say so, got %q", none)
	}
	mustRun(t, fixture.args("cordon", "--node", "base")...)
	mustRun(t, fixture.args("cordon", "--node", "base", "--undo")...)

	all := mustRun(t, fixture.args("events")...)
	if lines := strings.Count(all, "\n"); lines != 2 {
		t.Fatalf("want two events, got %d:\n%s", lines, all)
	}
	limited := mustRun(t, fixture.args("events", "--limit", "1", "--since", "1h")...)
	if lines := strings.Count(limited, "\n"); lines != 1 {
		t.Fatalf("limit 1 printed %d lines:\n%s", lines, limited)
	}
	filtered := mustRun(t, fixture.args("events", "--kind", control.EvidenceNodeUncordoned)...)
	if !strings.Contains(filtered, "returned to service") || strings.Count(filtered, "\n") != 1 {
		t.Fatalf("kind filter returned:\n%s", filtered)
	}
}

// A standby follows the primary's history over the same signed API and ends
// with a byte-for-byte identical chain.
func TestStandbyOnceReplicatesHistory(t *testing.T) {
	fixture := newRemoteFixture(t)
	for range 3 {
		mustRun(t, fixture.args("cordon", "--node", "base")...)
		mustRun(t, fixture.args("cordon", "--node", "base", "--undo")...)
	}
	dir := t.TempDir()
	replica := filepath.Join(dir, "replica.db")
	anchor := filepath.Join(dir, "primary.anchor")
	mustRun(t, fixture.args("standby", "--once", "--event-log", replica, "--anchor", anchor,
		"--file", example("web-service.json"), "--log-level", "error")...)

	follower, err := eventlog.Open(replica)
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()
	want := fixture.instance.Status().Events
	if head := follower.Head(); head.Sequence != want {
		t.Fatalf("replica head is %d, want %d", head.Sequence, want)
	}
}

func TestStandbyRequiresEventLogAndAnchor(t *testing.T) {
	_, _, err := runCLI(t, "standby", "--event-log", filepath.Join(t.TempDir(), "r.db"))
	if err == nil || !strings.Contains(err.Error(), "event-log and anchor are required") {
		t.Fatalf("want a missing-flag error, got %v", err)
	}
}

// An answer larger than the client's bound must be reported as oversized, not
// truncated into JSON that fails to decode for a reason nobody can see.
func TestClientReportsOversizedResponse(t *testing.T) {
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", server.MaxRequestBody+1)))
	}))
	defer huge.Close()
	keyPath := generatedKey(t, "operator")
	_, _, err := runCLI(t, "status", "--server", huge.URL, "--key-id", "op-1", "--operator-key", keyPath)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want an oversized-response error, got %v", err)
	}
}
