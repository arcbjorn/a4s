package obs

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCountersAccumulate(t *testing.T) {
	metrics := NewMetrics()
	metrics.Count("a4s_test_total")
	metrics.Count("a4s_test_total")
	metrics.Add("a4s_test_total", 3)

	if got := metrics.Snapshot().Counters["a4s_test_total"]; got != 5 {
		t.Fatalf("counter = %d, want 5", got)
	}
}

func TestGaugesReplaceRatherThanAccumulate(t *testing.T) {
	metrics := NewMetrics()
	metrics.SetGauge("a4s_nodes", 3)
	metrics.SetGauge("a4s_nodes", 1)

	if got := metrics.Snapshot().Gauges["a4s_nodes"]; got != 1 {
		t.Fatalf("gauge = %d, want 1", got)
	}
}

// Request outcomes come from a closed set so a caller cannot cause unbounded
// series growth by supplying arbitrary outcome names.
func TestRequestOutcomesAreBounded(t *testing.T) {
	metrics := NewMetrics()
	metrics.CountRequest("accepted")
	metrics.CountRequest("denied")
	metrics.CountRequest("replayed")
	metrics.CountRequest("something-invented")
	metrics.CountRequest("another-invention")

	counters := metrics.Snapshot().Counters
	if counters["a4s_operator_requests_total_accepted"] != 1 {
		t.Fatalf("accepted = %d", counters["a4s_operator_requests_total_accepted"])
	}
	if counters["a4s_operator_requests_total_other"] != 2 {
		t.Fatalf("unrecognized outcomes = %d, want 2 collapsed into one series",
			counters["a4s_operator_requests_total_other"])
	}
	if len(counters) != 4 {
		t.Fatalf("series count = %d, want 4: %v", len(counters), counters)
	}
}

// A snapshot must not alias the live maps, or a reader would observe values
// changing underneath it.
func TestSnapshotIsACopy(t *testing.T) {
	metrics := NewMetrics()
	metrics.Count("a4s_test_total")
	snapshot := metrics.Snapshot()
	metrics.Count("a4s_test_total")

	if snapshot.Counters["a4s_test_total"] != 1 {
		t.Fatalf("snapshot changed after the fact: %d", snapshot.Counters["a4s_test_total"])
	}
}

func TestTextRendersExpositionFormat(t *testing.T) {
	metrics := NewMetrics()
	metrics.Add("a4s_events_total", 7)
	metrics.SetGauge("a4s_nodes", 2)

	text := metrics.Text()
	for _, want := range []string{
		"# TYPE a4s_events_total counter",
		"a4s_events_total 7",
		"# TYPE a4s_nodes gauge",
		"a4s_nodes 2",
		"# TYPE a4s_uptime_seconds gauge",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("exposition missing %q:\n%s", want, text)
		}
	}
}

func TestUptimeAdvances(t *testing.T) {
	clock := time.Now()
	metrics := newMetricsAt(func() time.Time { return clock })
	clock = clock.Add(90 * time.Second)

	if got := metrics.Snapshot().UptimeSeconds; got != 90 {
		t.Fatalf("uptime = %v, want 90", got)
	}
}

// A nil registry is usable, so a caller with metrics disabled needs no nil
// checks at every call site.
func TestNilMetricsAreSafe(t *testing.T) {
	var metrics *Metrics
	metrics.Count("ignored")
	metrics.SetGauge("ignored", 1)
	if snapshot := metrics.Snapshot(); len(snapshot.Counters) != 0 {
		t.Fatal("nil registry produced counters")
	}
}

func TestConcurrentUpdatesAreSafe(t *testing.T) {
	metrics := NewMetrics()
	var group sync.WaitGroup
	for range 50 {
		group.Add(1)
		go func() {
			defer group.Done()
			metrics.Count("a4s_test_total")
			metrics.SetGauge("a4s_nodes", 1)
			_ = metrics.Snapshot()
		}()
	}
	group.Wait()

	if got := metrics.Snapshot().Counters["a4s_test_total"]; got != 50 {
		t.Fatalf("counter = %d, want 50", got)
	}
}
