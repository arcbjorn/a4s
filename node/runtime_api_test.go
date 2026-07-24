package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// socketPath returns a short path for a Unix socket.
//
// Socket paths are capped near 104 bytes on macOS and BSDs, and the per-test
// temporary directory is long enough to exceed it. Tests get a short path of
// their own rather than the production code compensating for a test detail.
func socketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "a4s")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "r.sock")
}

// runtimeRig is a node serving the workload-facing API, with one metered
// allocation bound to a queue.
type runtimeRig struct {
	api    *RuntimeAPI
	agents *Agents
	queue  *Queue
	token  string
	client *http.Client
}

func newRuntimeRig(t *testing.T, allocation string, tools ...control.ToolGrant) *runtimeRig {
	t.Helper()
	directory := t.TempDir()
	agents := meteredAgents(t, allocation, tools...)
	api, err := ServeRuntimeAPI(socketPath(t), agents)
	if err != nil {
		t.Fatalf("serve runtime api: %v", err)
	}
	t.Cleanup(func() { _ = api.Close() })
	api.TokenRoot = filepath.Join(directory, "tokens")

	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	api.Serve("intake", &QueueBroker{Queue: queue, Agents: agents})
	api.Bind(allocation, "intake")

	token, err := agents.IssueToken(allocation)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	rig := &runtimeRig{api: api, agents: agents, queue: queue, token: token}
	rig.client = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", api.Socket())
			},
		},
	}
	return rig
}

func (r *runtimeRig) call(t *testing.T, token, path string, body any) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = encoded
	} else {
		payload = []byte("{}")
	}
	request, err := http.NewRequest(http.MethodPost, "http://runtime"+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := r.client.Do(request)
	if err != nil {
		t.Fatalf("call %s: %v", path, err)
	}
	defer response.Body.Close()

	decoded := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded
}

// The runtime never names itself. Identity comes from a node-issued credential,
// which is what makes every budget and envelope gate hold.
func TestRuntimeIdentityComesFromToken(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")

	status, body := rig.call(t, rig.token, "/v1/identity", nil)
	if status != http.StatusOK {
		t.Fatalf("expected identity to succeed, got %d %v", status, body)
	}
	if body["allocation"] != "triage-0" {
		t.Fatalf("expected the node to name the allocation, got %v", body)
	}
	if body["version"] != RuntimeAPIVersion {
		t.Fatalf("expected the served contract version, got %v", body["version"])
	}
}

// A forged or revoked credential must not reach any operation.
func TestRuntimeRefusesUnknownToken(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")

	for _, path := range []string{
		"/v1/identity", "/v1/task/claim", "/v1/spend", "/v1/tool/authorize",
	} {
		if status, _ := rig.call(t, "forged", path, nil); status != http.StatusUnauthorized {
			t.Fatalf("%s accepted a forged token with status %d", path, status)
		}
		if status, _ := rig.call(t, "", path, nil); status != http.StatusUnauthorized {
			t.Fatalf("%s accepted a missing token with status %d", path, status)
		}
	}
}

// This is the attack the token design exists to prevent: one instance spending
// another's budget or borrowing its capabilities by naming it.
func TestRuntimeCannotActAsAnotherAllocation(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")
	// A second instance with its own credential and its own meter.
	rig.agents.Reserve("triage-1", testBudget())
	other, err := rig.agents.IssueToken("triage-1")
	if err != nil {
		t.Fatal(err)
	}

	// Spend reported with triage-1's token must land on triage-1, whatever the
	// body says. There is deliberately no allocation field to abuse.
	status, _ := rig.call(t, other, "/v1/spend", spendRequest{Tokens: 400})
	if status != http.StatusOK {
		t.Fatalf("expected spend to succeed, got %d", status)
	}
	spentZero, _ := rig.agents.Spent("triage-0")
	spentOne, _ := rig.agents.Spent("triage-1")
	if spentZero.Tokens != 0 {
		t.Fatalf("one instance's spend landed on another: %d", spentZero.Tokens)
	}
	if spentOne.Tokens != 400 {
		t.Fatalf("expected spend on the calling instance, got %d", spentOne.Tokens)
	}
}

// A request naming an allocation must be refused outright rather than silently
// ignored, so a runtime built against a wrong contract fails loudly.
func TestRuntimeRefusesUnknownRequestFields(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")

	status, _ := rig.call(t, rig.token, "/v1/spend",
		map[string]any{"tokens": 10, "allocation": "triage-1"})
	if status != http.StatusBadRequest {
		t.Fatalf("expected an unknown field to be refused, got %d", status)
	}
}

// The full loop an agent image runs: claim, work, report, acknowledge.
func TestRuntimeClaimWorkAndAcknowledge(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0",
		control.ToolGrant{Name: "repo.read", Scope: "org/a"})
	if err := rig.queue.Enqueue("task-1", "review pr 12"); err != nil {
		t.Fatal(err)
	}

	status, body := rig.call(t, rig.token, "/v1/task/claim", nil)
	if status != http.StatusOK || body["claimed"] != true {
		t.Fatalf("expected a claim, got %d %v", status, body)
	}
	if body["payload"] != "review pr 12" {
		t.Fatalf("expected the task payload, got %v", body)
	}

	// Holding work must be visible to a drain.
	if !rig.agents.Draining("triage-0") {
		evidence, err := rig.agents.drain(control.Action{
			Kind: control.ActionDrainAllocation, Target: "triage-0",
		})
		if err != nil {
			t.Fatal(err)
		}
		if evidence.Kind != control.EvidenceAgentDraining {
			t.Fatalf("expected a working instance to report draining, got %q", evidence.Kind)
		}
	}

	status, body = rig.call(t, rig.token, "/v1/tool/authorize",
		toolRequest{Tool: "repo.read", Scope: "org/a"})
	if status != http.StatusOK || body["authorized"] != true {
		t.Fatalf("expected the granted tool to be authorized, got %d %v", status, body)
	}

	status, _ = rig.call(t, rig.token, "/v1/spend", spendRequest{Tokens: 120, CostMillis: 8})
	if status != http.StatusOK {
		t.Fatalf("expected spend to be recorded, got %d", status)
	}

	status, _ = rig.call(t, rig.token, "/v1/task/ack", taskRequest{TaskID: "task-1"})
	if status != http.StatusOK {
		t.Fatalf("expected ack to succeed, got %d", status)
	}
	if waiting, inFlight := rig.queue.Depth(); waiting != 0 || inFlight != 0 {
		t.Fatalf("expected the queue to drain, got waiting=%d in_flight=%d", waiting, inFlight)
	}
}

// The envelope is enforced at the boundary the agent actually crosses.
func TestRuntimeRefusesUngrantedTool(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0",
		control.ToolGrant{Name: "repo.read", Scope: "org/a"})

	status, body := rig.call(t, rig.token, "/v1/tool/authorize",
		toolRequest{Tool: "shell.exec", Scope: "/"})
	if status != http.StatusForbidden || body["authorized"] != false {
		t.Fatalf("expected an ungranted tool to be refused, got %d %v", status, body)
	}
	if rig.agents.ToolRefusals("triage-0") != 1 {
		t.Fatal("expected the refusal to be recorded")
	}
}

// An agent cannot complete work it was never given.
func TestRuntimeCannotAcknowledgeAnothersTask(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")
	rig.agents.Reserve("triage-1", testBudget())
	other, err := rig.agents.IssueToken("triage-1")
	if err != nil {
		t.Fatal(err)
	}
	rig.api.Bind("triage-1", "intake")
	if err := rig.queue.Enqueue("task-1", "payload"); err != nil {
		t.Fatal(err)
	}
	if status, _ := rig.call(t, rig.token, "/v1/task/claim", nil); status != http.StatusOK {
		t.Fatalf("expected the first claim to succeed, got %d", status)
	}

	status, _ := rig.call(t, other, "/v1/task/ack", taskRequest{TaskID: "task-1"})
	if status != http.StatusConflict {
		t.Fatalf("expected a non-holder ack to be refused, got %d", status)
	}
}

// Draining and exhaustion are lifecycle states, not faults. A runtime should
// stop asking rather than retry, so they read as a refusal with a reason.
func TestRuntimeReportsDrainingAsEmptyClaim(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")
	if err := rig.queue.Enqueue("task-1", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.agents.drain(control.Action{
		Kind: control.ActionDrainAllocation, Target: "triage-0",
	}); err != nil {
		t.Fatal(err)
	}

	status, body := rig.call(t, rig.token, "/v1/task/claim", nil)
	if status != http.StatusOK {
		t.Fatalf("expected a draining instance to get a clean refusal, got %d", status)
	}
	if body["claimed"] != false || body["reason"] == "" {
		t.Fatalf("expected an explained empty claim, got %v", body)
	}
	// The work must remain for a worker that can run it.
	if waiting, _ := rig.queue.Depth(); waiting != 1 {
		t.Fatalf("expected the task to stay waiting, got %d", waiting)
	}
}

// The node does not depend on a well-behaved runtime honoring Continue: the
// tool gate refuses an exhausted instance regardless.
func TestRuntimeSpendReportsExhaustion(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0",
		control.ToolGrant{Name: "repo.read", Scope: "org/a"})

	status, body := rig.call(t, rig.token, "/v1/spend", spendRequest{Tokens: 5000})
	if status != http.StatusOK {
		t.Fatalf("expected spend to be recorded, got %d", status)
	}
	if body["continue"] != false {
		t.Fatalf("expected the runtime to be told to stop, got %v", body)
	}
	status, _ = rig.call(t, rig.token, "/v1/tool/authorize",
		toolRequest{Tool: "repo.read", Scope: "org/a"})
	if status != http.StatusForbidden {
		t.Fatalf("expected an exhausted instance to be refused a granted tool, got %d", status)
	}
}

// An agent naming its own queue could drain another workload's backlog, so the
// binding comes from what the control plane authorized.
func TestRuntimeRefusesClaimWithoutQueueBinding(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")
	rig.api.Unbind("triage-0")

	status, body := rig.call(t, rig.token, "/v1/task/claim", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected an unbound allocation to be refused, got %d %v", status, body)
	}
}

// Re-issuing on restart invalidates the old credential, so one that leaked from
// a previous incarnation stops working.
func TestReissuingTokenInvalidatesTheOld(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")

	replacement, err := rig.agents.IssueToken("triage-0")
	if err != nil {
		t.Fatal(err)
	}
	if replacement == rig.token {
		t.Fatal("expected a distinct credential")
	}
	if status, _ := rig.call(t, rig.token, "/v1/identity", nil); status != http.StatusUnauthorized {
		t.Fatalf("expected the superseded token to stop working, got %d", status)
	}
	if status, _ := rig.call(t, replacement, "/v1/identity", nil); status != http.StatusOK {
		t.Fatalf("expected the new token to work, got %d", status)
	}
}

// A credential must not outlive the workload that held it.
func TestReleaseRevokesTheToken(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")
	if err := rig.api.Provision("triage-0", rig.token); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rig.api.TokenPath("triage-0")); err != nil {
		t.Fatalf("expected the credential to be provisioned: %v", err)
	}

	rig.agents.Release("triage-0")
	if err := rig.api.Revoke("triage-0"); err != nil {
		t.Fatal(err)
	}
	if status, _ := rig.call(t, rig.token, "/v1/identity", nil); status != http.StatusUnauthorized {
		t.Fatalf("expected a revoked token to stop working, got %d", status)
	}
	if _, err := os.Stat(rig.api.TokenPath("triage-0")); !os.IsNotExist(err) {
		t.Fatal("expected the credential file to be removed")
	}
}

// A live credential in an environment variable would appear in process listings
// and crash dumps, so it is written owner-readable instead.
func TestProvisionedTokenIsOwnerReadable(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")
	if err := rig.api.Provision("triage-0", rig.token); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(rig.api.TokenPath("triage-0"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("expected owner-only credential, got %o", mode)
	}
}

// Creating an agent allocation must leave it able to identify itself before its
// container starts.
func TestCreateAllocationProvisionsRuntimeCredential(t *testing.T) {
	directory := t.TempDir()
	agents := NewAgents(directory)
	api, err := ServeRuntimeAPI(socketPath(t), agents)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	api.TokenRoot = filepath.Join(directory, "tokens")

	runtime := &CompositeRuntime{
		Containers: NewContainerRuntime(&fakeBackend{
			state: BackendState{Exists: true, Running: false},
		}),
		Agents: agents, RuntimeAPI: api,
	}
	if _, err := runtime.Execute(context.Background(), control.Action{
		Kind: control.ActionCreateAllocation, Target: "triage-0",
		Workload: "triage", Node: "base", Image: testImage, Budget: testBudget(),
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(api.TokenPath("triage-0"))
	if err != nil {
		t.Fatalf("expected a provisioned credential: %v", err)
	}
	allocation, err := agents.Resolve(string(raw))
	if err != nil || allocation != "triage-0" {
		t.Fatalf("expected the credential to resolve to its allocation, got %q %v", allocation, err)
	}
}

// An ordinary workload gets no runtime credential, since it has no budget and
// no envelope to spend against.
func TestOrdinaryWorkloadGetsNoRuntimeCredential(t *testing.T) {
	directory := t.TempDir()
	agents := NewAgents(directory)
	api, err := ServeRuntimeAPI(socketPath(t), agents)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	api.TokenRoot = filepath.Join(directory, "tokens")

	runtime := &CompositeRuntime{
		Containers: NewContainerRuntime(&fakeBackend{
			state: BackendState{Exists: true, Running: false},
		}),
		Agents: agents, RuntimeAPI: api,
	}
	if _, err := runtime.Execute(context.Background(), control.Action{
		Kind: control.ActionCreateAllocation, Target: "web-0", Workload: "web",
		Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(api.TokenPath("web-0")); !os.IsNotExist(err) {
		t.Fatal("provisioned a credential for a workload with no budget")
	}
}

// The API is reachable only through the node's socket, never a port, so a
// per-instance credential cannot become a cluster-wide attack surface.
func TestRuntimeAPIListensOnUnixSocketOnly(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")

	info, err := os.Stat(rig.api.Socket())
	if err != nil {
		t.Fatalf("expected a socket at the reported path: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a unix socket, got mode %v", info.Mode())
	}
}

// A node whose previous process died must still be able to serve, rather than
// failing on a leftover socket file.
func TestRuntimeAPIReplacesStaleSocket(t *testing.T) {
	directory := t.TempDir()
	socket := socketPath(t)
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	api, err := ServeRuntimeAPI(socket, NewAgents(directory))
	if err != nil {
		t.Fatalf("expected a stale socket to be replaced: %v", err)
	}
	_ = api.Close()
}

// An untrusted caller must not be able to exhaust node memory without spending
// a token.
func TestRuntimeRejectsOversizedRequest(t *testing.T) {
	rig := newRuntimeRig(t, "triage-0")
	huge := make([]byte, maxRuntimeRequest+1024)
	for i := range huge {
		huge[i] = 'a'
	}

	request, err := http.NewRequest(http.MethodPost, "http://runtime/v1/spend",
		bytes.NewReader([]byte(fmt.Sprintf(`{"tokens":1,"note":%q}`, huge))))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+rig.token)
	response, err := rig.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected an oversized body to be refused, got %d", response.StatusCode)
	}
}
