package node

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

func observerWithState(state BackendState) *RuntimeObserver {
	return NewRuntimeObserver(NewContainerRuntime(&fakeBackend{state: state}))
}

// A stopped or absent task is never ready, whatever else might answer on the
// port. This is the check that stops a leftover listener from masking a dead
// workload.
func TestProbeReportsNotReadyWhenTaskIsNotRunning(t *testing.T) {
	for name, state := range map[string]BackendState{
		"absent":  {},
		"stopped": {Exists: true, Running: false},
	} {
		t.Run(name, func(t *testing.T) {
			observer := observerWithState(state)
			ready, observed, err := observer.ObserveReadiness(control.ProbeTarget{
				Allocation: "web-0", Kind: control.ProbeProcess,
			})
			if err != nil {
				t.Fatal(err)
			}
			if ready {
				t.Fatalf("reported ready for a task that is not running: %+v", observed)
			}
			if observed["reason"] == "" {
				t.Fatalf("missing reason for unreadiness: %+v", observed)
			}
		})
	}
}

func TestProcessProbeReportsReadyForRunningTask(t *testing.T) {
	observer := observerWithState(BackendState{Exists: true, Running: true, PID: 4242})
	ready, observed, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeProcess,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ready || observed["pid"] != "4242" {
		t.Fatalf("unexpected process probe result: ready=%t observed=%+v", ready, observed)
	}
}

func TestTCPProbeMeasuresRealListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	observer := observerWithState(BackendState{Exists: true, Running: true})
	observer.Endpoints["web-0"] = listener.Addr().String()
	ready, observed, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeTCP,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatalf("expected ready against a real listener: %+v", observed)
	}

	// Closing the listener must flip the measurement.
	listener.Close()
	ready, observed, err = observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeTCP,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatalf("expected not ready after the listener closed: %+v", observed)
	}
}

func TestHTTPProbeUsesStatusCode(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()

	observer := observerWithState(BackendState{Exists: true, Running: true})

	observer.Endpoints["web-0"] = strings.TrimPrefix(healthy.URL, "http://")
	ready, observed, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeHTTP, Path: "/healthz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ready || observed["status"] != "200" {
		t.Fatalf("expected healthy probe: ready=%t observed=%+v", ready, observed)
	}

	observer.Endpoints["web-0"] = strings.TrimPrefix(unhealthy.URL, "http://")
	ready, observed, err = observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeHTTP,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready || observed["status"] != "503" {
		t.Fatalf("expected unhealthy probe: ready=%t observed=%+v", ready, observed)
	}
}

// A redirect is the workload's answer, not an instruction to measure elsewhere.
func TestHTTPProbeDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	workload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/internal", http.StatusFound)
	}))
	defer workload.Close()
	observer := observerWithState(BackendState{Exists: true, Running: true})
	observer.Endpoints["web-0"] = strings.TrimPrefix(workload.URL, "http://")
	if _, _, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeHTTP, Path: "/healthz"}); err != nil {
		t.Fatal(err)
	}
	if reached.Load() {
		t.Fatal("the probe followed a workload-controlled redirect to another host")
	}
}

// The probe path must not be able to name a different host.
func TestHTTPProbePathCannotChangeHost(t *testing.T) {
	var reached atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	host := strings.TrimPrefix(elsewhere.URL, "http://")
	observer := observerWithState(BackendState{Exists: true, Running: true})
	observer.Endpoints["web-0"] = "127.0.0.1:1"
	for _, path := range []string{"@" + host + "/", "http://" + host + "/", "relative"} {
		ready, _, _ := observer.ObserveReadiness(control.ProbeTarget{
			Allocation: "web-0", Kind: control.ProbeHTTP, Path: path})
		if reached.Load() || ready {
			t.Fatalf("probe path %q redirected the measurement (ready=%t)", path, ready)
		}
	}
}

// A path with a query string still reaches the workload intact.
func TestHTTPProbeKeepsQueryString(t *testing.T) {
	var gotQuery string
	workload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer workload.Close()
	observer := observerWithState(BackendState{Exists: true, Running: true})
	observer.Endpoints["web-0"] = strings.TrimPrefix(workload.URL, "http://")
	ready, _, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeHTTP, Path: "/healthz?deep=1"})
	if err != nil || !ready {
		t.Fatalf("ready=%t err=%v", ready, err)
	}
	if gotQuery != "deep=1" {
		t.Fatalf("query = %q, want deep=1", gotQuery)
	}
}
