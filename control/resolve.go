package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// ServiceDomain is the suffix under which a4s service names resolve.
//
// A dedicated suffix keeps a4s names from colliding with public DNS: a
// workload called "api" resolves as api.a4s.internal, so a lookup that escapes
// to the public resolver fails rather than silently reaching a stranger's host.
const ServiceDomain = "a4s.internal"

// ResolvedEndpoint is one address a caller on a specific node should dial.
//
// It differs from Endpoint in that it has already been translated for the
// caller's vantage point. An allocation address is only meaningful on the node
// that owns it, so a caller elsewhere gets its owning node's gateway instead.
type ResolvedEndpoint struct {
	// Address and Port are what the caller dials.
	Address string `json:"address"`
	Port    int    `json:"port"`
	// Allocation and Node name what is ultimately being reached, so an
	// operator debugging a resolution can see past the forwarding hop.
	Allocation string `json:"allocation"`
	Node       string `json:"node"`
	// Local reports whether this endpoint is on the caller's own node. A local
	// endpoint is dialed directly, which is both faster and one less component
	// that can fail.
	Local bool `json:"local"`
}

// HostPort renders the endpoint as a dialable address.
func (e ResolvedEndpoint) HostPort() string {
	return net.JoinHostPort(e.Address, strconv.Itoa(e.Port))
}

// ServiceName renders a workload's fully qualified internal name.
func ServiceName(workload string) string {
	return workload + "." + ServiceDomain
}

// ParseServiceName extracts the workload from a fully qualified internal name.
//
// A bare workload name is accepted too, so a caller that already knows it is
// talking about an a4s service does not have to append the suffix.
func ParseServiceName(name string) (string, bool) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" {
		return "", false
	}
	if suffix := "." + ServiceDomain; strings.HasSuffix(name, suffix) {
		workload := strings.TrimSuffix(name, suffix)
		if workload == "" || strings.Contains(workload, ".") {
			// A multi-label prefix is not a workload name, and treating it as
			// one would resolve something nobody declared.
			return "", false
		}
		return workload, true
	}
	if strings.Contains(name, ".") {
		return "", false
	}
	return name, true
}

// Resolve answers where a caller on the given node should send traffic for a
// service.
//
// Endpoints on the caller's own node are returned first and dialed directly.
// Everything else is reached through the owning node's gateway, because an
// allocation address assigned by CNI is only routable on the node that assigned
// it. That indirection is what makes a service name mean the same thing from
// anywhere in the cluster.
//
// A node with no reachable address contributes no endpoints. Returning an
// address that cannot be dialed would turn a resolvable name into a connection
// timeout, which is strictly worse than an honest empty answer.
func Resolve(world World, ports map[string]int, workload, fromNode string) []ResolvedEndpoint {
	directory := BuildDirectory(world, ports)
	service, known := directory[workload]
	if !known {
		return nil
	}

	resolved := make([]ResolvedEndpoint, 0, len(service.Endpoints))
	for _, endpoint := range service.Endpoints {
		if endpoint.Node == fromNode {
			resolved = append(resolved, ResolvedEndpoint{
				Address: endpoint.Address, Port: endpoint.Port,
				Allocation: endpoint.Allocation, Node: endpoint.Node, Local: true,
			})
			continue
		}
		node, ok := world.Nodes[endpoint.Node]
		if !ok || node.Address == "" || node.GatewayPort <= 0 {
			// Nothing off-node can reach this allocation, so it is not an
			// endpoint from here even though it is serving.
			continue
		}
		resolved = append(resolved, ResolvedEndpoint{
			Address: node.Address, Port: node.GatewayPort,
			Allocation: endpoint.Allocation, Node: endpoint.Node,
		})
	}

	// Local endpoints first, then a stable order, so a caller prefers the
	// instance on its own node and repeated lookups agree.
	sort.SliceStable(resolved, func(i, j int) bool {
		if resolved[i].Local != resolved[j].Local {
			return resolved[i].Local
		}
		return resolved[i].Allocation < resolved[j].Allocation
	})
	return resolved
}

// ResolveName answers a fully qualified internal name.
func ResolveName(world World, ports map[string]int, name, fromNode string) ([]ResolvedEndpoint, error) {
	workload, ok := ParseServiceName(name)
	if !ok {
		return nil, fmt.Errorf("%q is not an a4s service name", name)
	}
	return Resolve(world, ports, workload, fromNode), nil
}

// ServiceZone is the complete set of names a node's resolver should answer.
//
// It is built from the same directory the gateway consumes, so a name resolves
// exactly when the route layer would also route it. Two independently derived
// views would eventually disagree, and a name that resolves to an instance the
// gateway will not serve is worse than one that does not resolve at all.
type ServiceZone struct {
	// Node is the vantage point this zone was built for.
	Node string `json:"node"`
	// Records maps a fully qualified name to its resolved endpoints.
	Records map[string][]ResolvedEndpoint `json:"records"`
}

// Fingerprint is a stable digest of everything the zone publishes.
//
// It exists so a publisher can tell whether a node's resolver already holds the
// current view. Comparing digests rather than republishing unconditionally
// keeps an unchanged cluster from generating a zone update every round.
func (z ServiceZone) Fingerprint() string {
	names := make([]string, 0, len(z.Records))
	for name := range z.Records {
		names = append(names, name)
	}
	sort.Strings(names)

	hash := sha256.New()
	fmt.Fprintf(hash, "node=%s\n", z.Node)
	for _, name := range names {
		fmt.Fprintf(hash, "%s\n", name)
		for _, endpoint := range z.Records[name] {
			fmt.Fprintf(hash, "  %s %s %s\n",
				endpoint.HostPort(), endpoint.Allocation, endpoint.Node)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// BuildServiceZone renders every service name resolvable from one node.
func BuildServiceZone(world World, ports map[string]int, fromNode string) ServiceZone {
	directory := BuildDirectory(world, ports)
	zone := ServiceZone{Node: fromNode, Records: make(map[string][]ResolvedEndpoint, len(directory))}
	for workload := range directory {
		endpoints := Resolve(world, ports, workload, fromNode)
		if len(endpoints) == 0 {
			// A name with no reachable endpoint is omitted rather than
			// published empty, so a caller gets NXDOMAIN instead of a
			// successful lookup it cannot connect to.
			continue
		}
		zone.Records[ServiceName(workload)] = endpoints
	}
	return zone
}
