package node

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// DefaultProviderTTL bounds how long a reachability measurement is trusted.
//
// It is longer than a readiness TTL because egress changes on the scale of
// network events rather than process health, and every measurement costs a real
// request to somebody else's service. It is still short enough that a node which
// loses its route stops attracting agent placements within a scheduling cycle
// or two.
const DefaultProviderTTL = 2 * time.Minute

// DefaultProviderTimeout bounds one reachability check.
const DefaultProviderTimeout = 5 * time.Second

// ProviderEndpoint is what a node must reach for a provider to be usable.
type ProviderEndpoint struct {
	// Name is the provider identifier a goal refers to.
	Name string
	// URL is the endpoint whose reachability stands in for the provider's. It
	// should be a cheap, unauthenticated health or models endpoint: this runs on
	// a timer against a third party, and a heavyweight check would be both
	// expensive and rude.
	URL string
}

// ProviderMonitor measures and caches egress to model providers.
//
// Reachability is a real network fact, so it is measured rather than
// configured. It is cached because the scheduler asks on every placement and the
// answer changes far more slowly than it is read.
//
// The monitor fails closed. An error, a timeout, and a refused connection all
// produce "unreachable", because a node that cannot prove it can reach a
// provider must not attract agents that depend on it.
type ProviderMonitor struct {
	// Endpoints are the providers this node claims to serve.
	Endpoints []ProviderEndpoint
	// TTL is how long a measurement stays valid.
	TTL time.Duration
	// Timeout bounds one check.
	Timeout time.Duration
	// Client performs the check. Tests supply their own.
	Client *http.Client
	// Now is the clock, injectable so expiry is testable without sleeping.
	Now func() time.Time

	mu      sync.Mutex
	results map[string]providerResult
}

type providerResult struct {
	reachable  bool
	detail     string
	observedAt time.Time
	expiresAt  time.Time
}

func NewProviderMonitor(endpoints ...ProviderEndpoint) *ProviderMonitor {
	return &ProviderMonitor{
		Endpoints: endpoints,
		TTL:       DefaultProviderTTL,
		Timeout:   DefaultProviderTimeout,
		Client:    &http.Client{Timeout: DefaultProviderTimeout},
		Now:       time.Now,
		results:   make(map[string]providerResult),
	}
}

// Reachable implements ProviderReach for the agent capability.
//
// It answers from cache only. A readiness probe must not block on a third
// party's response time, and an expired entry reads as unreachable rather than
// triggering a synchronous check: absence of a current measurement is not
// evidence of reach.
func (p *ProviderMonitor) Reachable(provider string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	result, ok := p.results[provider]
	if !ok {
		return false
	}
	if !result.expiresAt.IsZero() && !p.now().Before(result.expiresAt) {
		return false
	}
	return result.reachable
}

// Refresh measures every declared provider and returns evidence for each.
//
// This is what the node runs on a timer. Measuring all providers together keeps
// one slow provider from starving the others of observations, since each check
// is independently bounded.
func (p *ProviderMonitor) Refresh(ctx context.Context) []control.Evidence {
	endpoints := append([]ProviderEndpoint(nil), p.Endpoints...)
	// Stable order keeps evidence sequences deterministic, which matters for
	// replaying a durable log.
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].Name < endpoints[j].Name })

	observations := make([]control.Evidence, 0, len(endpoints))
	for _, endpoint := range endpoints {
		reachable, detail := p.measure(ctx, endpoint)
		now := p.now()
		expires := now.Add(p.ttl())

		p.mu.Lock()
		if p.results == nil {
			p.results = make(map[string]providerResult)
		}
		p.results[endpoint.Name] = providerResult{
			reachable: reachable, detail: detail,
			observedAt: now, expiresAt: expires,
		}
		p.mu.Unlock()

		observed := map[string]string{"reachable": fmt.Sprint(reachable)}
		if detail != "" {
			observed["detail"] = detail
		}
		observations = append(observations, control.Evidence{
			Kind: control.EvidenceProviderReachable, Target: endpoint.Name,
			ObservedAt: now, ExpiresAt: expires, Observed: observed,
		})
	}
	return observations
}

// measure performs one reachability check.
//
// Any non-server response counts as reachable. The question is whether this node
// has a working path to the provider, not whether the request was authorized: a
// 401 proves egress just as well as a 200, and treating it as failure would
// report a credential problem as a network one.
func (p *ProviderMonitor) measure(ctx context.Context, endpoint ProviderEndpoint) (bool, string) {
	if endpoint.URL == "" {
		return false, "no endpoint configured"
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.URL, nil)
	if err != nil {
		return false, err.Error()
	}
	response, err := p.client().Do(request)
	if err != nil {
		return false, err.Error()
	}
	defer response.Body.Close()

	// A 5xx means the provider itself is failing, which is not a usable path
	// even though packets arrived.
	if response.StatusCode >= 500 {
		return false, fmt.Sprintf("provider returned %d", response.StatusCode)
	}
	return true, ""
}

func (p *ProviderMonitor) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: p.timeout()}
}

func (p *ProviderMonitor) ttl() time.Duration {
	if p.TTL > 0 {
		return p.TTL
	}
	return DefaultProviderTTL
}

func (p *ProviderMonitor) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultProviderTimeout
}

func (p *ProviderMonitor) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// Detail reports why a provider was last judged unreachable, so an operator can
// tell a DNS failure from a provider outage without reading node logs.
func (p *ProviderMonitor) Detail(provider string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.results[provider].detail
}

// verify ProviderMonitor satisfies the interface the agent capability needs.
var _ ProviderReach = (*ProviderMonitor)(nil)
