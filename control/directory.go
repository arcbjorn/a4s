package control

import (
	"net"
	"sort"
	"strconv"
	"time"
)

// Endpoint is one reachable instance of a service.
type Endpoint struct {
	Allocation string `json:"allocation"`
	Node       string `json:"node"`
	Address    string `json:"address"`
	Port       int    `json:"port"`
}

// HostPort renders the endpoint as a dialable address.
func (e Endpoint) HostPort() string {
	return net.JoinHostPort(e.Address, strconv.Itoa(e.Port))
}

// Service is a named workload and the endpoints currently serving it.
type Service struct {
	Name      string     `json:"name"`
	Endpoints []Endpoint `json:"endpoints"`
}

// Directory maps service names to the endpoints observed serving them.
//
// It is derived, never asserted. An endpoint appears only when the world holds
// unexpired readiness evidence for that allocation and CNI has given it an
// address. An agent cannot add an endpoint by proposing one, and a workload
// that has stopped being measured as ready falls out on the next build.
//
// That is the whole point: routing traffic to an instance nobody has recently
// observed serving is how a rollout becomes an outage.
func BuildDirectory(world World, ports map[string]int) map[string]Service {
	now := world.Now()
	services := make(map[string]Service)

	for _, allocation := range world.Allocations {
		// Three independent conditions, all required. Readiness alone is not
		// enough if the address is gone, and an address is meaningless if the
		// allocation is no longer measured as serving.
		if !allocation.ReadyAt(now) {
			continue
		}
		if allocation.Address == "" {
			continue
		}
		port, known := ports[allocation.Workload]
		if !known || port <= 0 {
			continue
		}
		service := services[allocation.Workload]
		service.Name = allocation.Workload
		service.Endpoints = append(service.Endpoints, Endpoint{
			Allocation: allocation.ID, Node: allocation.Node,
			Address: allocation.Address, Port: port,
		})
		services[allocation.Workload] = service
	}

	// Stable ordering makes the resulting gateway snapshot deterministic, so an
	// unchanged world does not produce a config the gateway sees as new.
	for name, service := range services {
		sort.Slice(service.Endpoints, func(i, j int) bool {
			return service.Endpoints[i].Allocation < service.Endpoints[j].Allocation
		})
		services[name] = service
	}
	return services
}

// RouteSnapshot pairs a route with the endpoints that should serve it. It is
// the complete, atomic input a gateway consumes.
type RouteSnapshot struct {
	Host      string     `json:"host"`
	Workload  string     `json:"workload"`
	Port      int        `json:"port"`
	Exposure  string     `json:"exposure"`
	Endpoints []Endpoint `json:"endpoints"`
	// Weighted carries per-endpoint traffic shares when a canary is in progress.
	// It is empty for an ordinary route, so a gateway that ignores it behaves
	// exactly as before.
	Weighted []WeightedEndpoint `json:"weighted,omitempty"`
}

// BuildRouteSnapshots resolves every route to its currently serving endpoints.
//
// A route with no healthy endpoint is still included, carrying an empty
// endpoint list. Dropping it would make the gateway silently forget a hostname
// the operator asked for, turning a transient outage into a 404 from an
// unrelated site; keeping it lets the gateway answer honestly instead.
func BuildRouteSnapshots(world World, ports map[string]int) []RouteSnapshot {
	return BuildWeightedRouteSnapshots(world, ports, nil)
}

// BuildWeightedRouteSnapshots resolves routes and applies canary weights.
//
// Goals are supplied so a route can be weighted by the canary its own goal
// declares. A workload with no goal here, or whose goal declares no canary, gets
// an unweighted snapshot, so this is a superset of BuildRouteSnapshots rather
// than a different behaviour.
func BuildWeightedRouteSnapshots(world World, ports map[string]int,
	goals []Goal) []RouteSnapshot {

	directory := BuildDirectory(world, ports)

	byWorkload := make(map[string]Goal, len(goals))
	for _, goal := range goals {
		byWorkload[goal.Workload.Name] = goal
	}

	hosts := make([]string, 0, len(world.Routes))
	for host := range world.Routes {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	snapshots := make([]RouteSnapshot, 0, len(hosts))
	for _, host := range hosts {
		route := world.Routes[host]
		endpoints := directory[route.Workload].Endpoints
		snapshot := RouteSnapshot{
			Host: route.Host, Workload: route.Workload,
			Port: route.Port, Exposure: route.Exposure,
			Endpoints: endpoints,
		}
		// Weights appear only while a canary is actually splitting traffic. An
		// unchanged route must produce an identical snapshot, or the gateway would
		// see a new config on every reconciliation.
		if goal, ok := byWorkload[route.Workload]; ok && goal.Canary != nil {
			if weighted := WeightEndpoints(goal, world, endpoints); splitsTraffic(weighted) {
				snapshot.Weighted = weighted
			}
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

// splitsTraffic reports whether weights actually differ. Equal weights carry no
// information a gateway does not already have from the endpoint list.
func splitsTraffic(weighted []WeightedEndpoint) bool {
	if len(weighted) < 2 {
		return false
	}
	first := weighted[0].Weight
	for _, endpoint := range weighted[1:] {
		if endpoint.Weight != first {
			return true
		}
	}
	return false
}

// WorkloadPorts collects the container ports declared by accepted goals, which
// is what an endpoint must be dialed on. The route's own port is what the
// gateway listens on and is deliberately separate.
func WorkloadPorts(goals []Goal) map[string]int {
	ports := make(map[string]int, len(goals))
	for _, goal := range goals {
		if goal.Workload.Port > 0 {
			ports[goal.Workload.Name] = goal.Workload.Port
		}
	}
	return ports
}

// StaleAfter reports services whose endpoints have all aged out, which is what
// an operator needs to see when a route stops resolving.
func StaleAfter(world World, ports map[string]int, at time.Time) []string {
	probe := world
	probe.ObservedAt = at
	live := BuildDirectory(probe, ports)

	var stale []string
	for _, route := range world.Routes {
		if len(live[route.Workload].Endpoints) == 0 {
			stale = append(stale, route.Host)
		}
	}
	sort.Strings(stale)
	return stale
}
