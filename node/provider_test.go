package node

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

func monitorAt(t *testing.T, now time.Time, endpoints ...ProviderEndpoint) *ProviderMonitor {
	t.Helper()
	monitor := NewProviderMonitor(endpoints...)
	monitor.Now = func() time.Time { return now }
	return monitor
}

// Reachability is a real network fact, so it is measured rather than assumed.
func TestProviderMonitorMeasuresReachability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	now := time.Unix(1_700_000_000, 0).UTC()
	monitor := monitorAt(t, now, ProviderEndpoint{Name: "anthropic", URL: server.URL})

	observations := monitor.Refresh(context.Background())
	if len(observations) != 1 {
		t.Fatalf("expected one observation, got %d", len(observations))
	}
	if observations[0].Kind != control.EvidenceProviderReachable {
		t.Fatalf("unexpected evidence kind %q", observations[0].Kind)
	}
	if observations[0].Observed["reachable"] != "true" {
		t.Fatalf("expected a reachable provider, got %v", observations[0].Observed)
	}
	if !observations[0].ExpiresAt.After(now) {
		t.Fatal("expected the measurement to carry an expiry")
	}
	if !monitor.Reachable("anthropic") {
		t.Fatal("expected the cached answer to report reachable")
	}
}

// A node that cannot prove it can reach a provider must not attract agents that
// depend on it, so every failure mode reads the same way.
func TestProviderMonitorFailsClosed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	for _, unreachable := range []struct {
		name string
		url  string
	}{
		{"no endpoint", ""},
		{"bad host", "http://127.0.0.1:1/health"},
	} {
		t.Run(unreachable.name, func(t *testing.T) {
			monitor := monitorAt(t, now,
				ProviderEndpoint{Name: "anthropic", URL: unreachable.url})
			observations := monitor.Refresh(context.Background())

			if observations[0].Observed["reachable"] != "false" {
				t.Fatalf("expected unreachable, got %v", observations[0].Observed)
			}
			if observations[0].Observed["detail"] == "" {
				t.Fatal("expected a failure detail to be reported")
			}
			if monitor.Reachable("anthropic") {
				t.Fatal("expected the cached answer to report unreachable")
			}
		})
	}
}

// A provider that is failing is not a usable path, even though packets arrive.
func TestProviderMonitorTreatsServerErrorsAsUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	monitor := monitorAt(t, time.Unix(1_700_000_000, 0).UTC(),
		ProviderEndpoint{Name: "anthropic", URL: server.URL})
	observations := monitor.Refresh(context.Background())

	if observations[0].Observed["reachable"] != "false" {
		t.Fatalf("expected a 503 to read as unreachable, got %v", observations[0].Observed)
	}
	if !strings.Contains(observations[0].Observed["detail"], "503") {
		t.Fatalf("expected the status to be reported, got %v", observations[0].Observed)
	}
}

// The question is whether this node has a working path, not whether the request
// was authorized. Treating a 401 as failure would report a credential problem
// as a network one.
func TestProviderMonitorTreatsUnauthorizedAsReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	monitor := monitorAt(t, time.Unix(1_700_000_000, 0).UTC(),
		ProviderEndpoint{Name: "anthropic", URL: server.URL})
	monitor.Refresh(context.Background())

	if !monitor.Reachable("anthropic") {
		t.Fatal("expected an authenticated endpoint to still prove egress")
	}
}

// An expired entry reads as unreachable rather than triggering a synchronous
// check: absence of a current measurement is not evidence of reach.
func TestProviderMonitorExpiresItsCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	now := time.Unix(1_700_000_000, 0).UTC()
	monitor := monitorAt(t, now, ProviderEndpoint{Name: "anthropic", URL: server.URL})
	monitor.TTL = time.Minute
	monitor.Refresh(context.Background())

	if !monitor.Reachable("anthropic") {
		t.Fatal("expected a fresh measurement to be reachable")
	}
	monitor.Now = func() time.Time { return now.Add(2 * time.Minute) }
	if monitor.Reachable("anthropic") {
		t.Fatal("expected an expired measurement to stop counting")
	}
}

// A provider nobody measured is not reachable.
func TestProviderMonitorRefusesUnknownProvider(t *testing.T) {
	monitor := monitorAt(t, time.Unix(1_700_000_000, 0).UTC())
	if monitor.Reachable("openai") {
		t.Fatal("expected an unmeasured provider to be unreachable")
	}
}

// Evidence order must be deterministic so a durable log replays identically.
func TestProviderMonitorReportsInStableOrder(t *testing.T) {
	monitor := monitorAt(t, time.Unix(1_700_000_000, 0).UTC(),
		ProviderEndpoint{Name: "openai", URL: ""},
		ProviderEndpoint{Name: "anthropic", URL: ""},
	)
	observations := monitor.Refresh(context.Background())

	if len(observations) != 2 {
		t.Fatalf("expected two observations, got %d", len(observations))
	}
	if observations[0].Target != "anthropic" || observations[1].Target != "openai" {
		t.Fatalf("expected stable ordering, got %q then %q",
			observations[0].Target, observations[1].Target)
	}
}

// The projection keys reachability by node, and only the node knows which one
// it is.
func TestSupervisorAttributesProviderEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	supervisor, _, _ := newSupervisorFixture(t)
	supervisor.NodeID = "base"
	supervisor.Providers = monitorAt(t, time.Unix(1_700_000_000, 0).UTC(),
		ProviderEndpoint{Name: "anthropic", URL: server.URL})

	observations, err := supervisor.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var reach *control.Evidence
	for i := range observations {
		if observations[i].Kind == control.EvidenceProviderReachable {
			reach = &observations[i]
		}
	}
	if reach == nil {
		t.Fatalf("expected provider evidence from supervision, got %+v", observations)
	}
	if reach.Observed["node"] != "base" {
		t.Fatalf("expected the node to be named, got %v", reach.Observed)
	}
	if reach.Source != "node-supervisor" {
		t.Fatalf("expected supervisor attribution, got %q", reach.Source)
	}
}

// A node without a provider monitor supervises exactly as before.
func TestSupervisorWithoutProviderMonitorIsUnchanged(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	if err := desired.Record(DesiredAllocation{
		ID: "web-0", Workload: "web", Running: true,
	}); err != nil {
		t.Fatal(err)
	}
	backend.states["web-0"] = BackendState{Exists: true, Running: false, ExitCode: 137}

	observations, err := supervisor.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range observations {
		if evidence.Kind == control.EvidenceProviderReachable {
			t.Fatal("reported provider evidence without a monitor")
		}
	}
	if backend.starts != 1 {
		t.Fatalf("expected ordinary supervision to continue, got %d starts", backend.starts)
	}
}

// The measured monitor is what the agent capability consults, so the two must
// agree without an adapter.
func TestProviderMonitorSatisfiesAgentReachability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	now := time.Unix(1_700_000_000, 0).UTC()
	monitor := monitorAt(t, now, ProviderEndpoint{Name: "anthropic", URL: server.URL})
	monitor.Refresh(context.Background())

	agents := NewAgents(t.TempDir())
	agents.Providers = monitor
	agents.Reserve("triage-0", testBudget())

	ready, _, err := agents.ObserveReadiness(control.ProbeTarget{
		Allocation: "triage-0", Kind: control.ProbeAgent, Provider: "anthropic",
	})
	if err != nil || !ready {
		t.Fatalf("expected a measured provider to make the agent ready: %v %v", ready, err)
	}
}
