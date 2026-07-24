package obs

import (
	"maps"
	"sort"
	"sync"
	"time"
)

// Metrics holds process counters and gauges.
//
// The implementation is deliberately a plain in-process registry rather than a
// Prometheus client. a4s is stdlib-only in its core packages, and the values an
// operator needs during an incident are few and known in advance. A text
// exposition endpoint keeps a scraper usable without taking the dependency.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]uint64
	gauges   map[string]int64
	started  time.Time
	now      func() time.Time
}

// NewMetrics builds an empty registry.
func NewMetrics() *Metrics {
	return newMetricsAt(time.Now)
}

func newMetricsAt(now func() time.Time) *Metrics {
	return &Metrics{
		counters: make(map[string]uint64),
		gauges:   make(map[string]int64),
		started:  now(),
		now:      now,
	}
}

// Count increments a named counter.
func (m *Metrics) Count(name string) { m.Add(name, 1) }

// Add increases a named counter by delta.
func (m *Metrics) Add(name string, delta uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[name] += delta
}

// SetGauge records the current value of a named gauge.
func (m *Metrics) SetGauge(name string, value int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = value
}

// CountRequest records an operator API request outcome. Outcomes are a small
// closed set (accepted, denied, replayed) so the series cannot grow without
// bound from caller-supplied values.
func (m *Metrics) CountRequest(outcome string) {
	switch outcome {
	case "accepted", "denied", "replayed":
		m.Count("a4s_operator_requests_total_" + outcome)
	default:
		m.Count("a4s_operator_requests_total_other")
	}
}

// Snapshot returns a copy of the current values.
type Snapshot struct {
	Counters map[string]uint64 `json:"counters"`
	Gauges   map[string]int64  `json:"gauges"`
	// UptimeSeconds is how long this process has been running, which is the
	// first thing worth knowing when a control plane is misbehaving.
	UptimeSeconds float64 `json:"uptime_seconds"`
}

func (m *Metrics) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{Counters: map[string]uint64{}, Gauges: map[string]int64{}}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{
		Counters:      maps.Clone(m.counters),
		Gauges:        maps.Clone(m.gauges),
		UptimeSeconds: m.now().Sub(m.started).Seconds(),
	}
}

// Text renders the registry in Prometheus text exposition format, so an
// existing scraper can read it without a4s taking a client dependency.
func (m *Metrics) Text() string {
	snapshot := m.Snapshot()
	names := make([]string, 0, len(snapshot.Counters)+len(snapshot.Gauges)+1)
	for name := range snapshot.Counters {
		names = append(names, name)
	}
	for name := range snapshot.Gauges {
		names = append(names, name)
	}
	sort.Strings(names)

	var builder textBuilder
	for _, name := range names {
		if value, ok := snapshot.Counters[name]; ok {
			builder.metric(name, "counter", float64(value))
			continue
		}
		builder.metric(name, "gauge", float64(snapshot.Gauges[name]))
	}
	builder.metric("a4s_uptime_seconds", "gauge", snapshot.UptimeSeconds)
	return builder.String()
}
