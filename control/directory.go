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
}

// BuildRouteSnapshots resolves every route to its currently serving endpoints.
//
// A route with no healthy endpoint is still included, carrying an empty
// endpoint list. Dropping it would make the gateway silently forget a hostname
// the operator asked for, turning a transient outage into a 404 from an
// unrelated site; keeping it lets the gateway answer honestly instead.
func BuildRouteSnapshots(world World, ports map[string]int) []RouteSnapshot {
	directory := BuildDirectory(world, ports)

	hosts := make([]string, 0, len(world.Routes))
	for host := range world.Routes {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	snapshots := make([]RouteSnapshot, 0, len(hosts))
	for _, host := range hosts {
		route := world.Routes[host]
		snapshots = append(snapshots, RouteSnapshot{
			Host: route.Host, Workload: route.Workload,
			Port: route.Port, Exposure: route.Exposure,
			Endpoints: directory[route.Workload].Endpoints,
		})
	}
	return snapshots
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
