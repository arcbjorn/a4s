package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strings"
)

// NetworkPolicy is the typed network intent for one workload.
//
// It is deliberately not a rule list. An agent proposing raw nftables rules
// would be proposing arbitrary kernel configuration, and no reviewer could tell
// an intended rule from a mistake. Declaring intent in terms a4s understands
// means the kernel can check it and the node can compile it, so the thing that
// gets authorized is the thing a human can reason about.
type NetworkPolicy struct {
	// Workload names what this policy applies to.
	Workload string `json:"workload"`
	// Ingress lists who may connect to this workload. An empty list denies all
	// inbound traffic, which is the correct default: a workload nobody declared
	// a caller for should not be reachable.
	Ingress []IngressRule `json:"ingress,omitempty"`
	// Egress lists where this workload may connect. An empty list denies all
	// outbound traffic except what a4s itself requires.
	Egress []EgressRule `json:"egress,omitempty"`
}

// IngressRule permits inbound traffic from one source.
type IngressRule struct {
	// FromWorkload permits other allocations of a named a4s workload. This is
	// the common case, and it is expressed by name rather than by address so
	// the rule survives replacement and rescheduling.
	FromWorkload string `json:"from_workload,omitempty"`
	// FromCIDR permits a literal network range, for traffic that does not
	// originate from an a4s workload.
	FromCIDR string `json:"from_cidr,omitempty"`
	// Port is the destination port being opened. Zero means every port the
	// workload declares, which is narrower than it sounds: a workload declares
	// exactly one.
	Port int `json:"port,omitempty"`
}

// EgressRule permits outbound traffic to one destination.
type EgressRule struct {
	// ToWorkload permits connecting to a named a4s workload.
	ToWorkload string `json:"to_workload,omitempty"`
	// ToCIDR permits a literal destination range.
	ToCIDR string `json:"to_cidr,omitempty"`
	// Port is the destination port. Zero means any port, which is why a rule
	// naming a CIDR without a port is refused for anything but a private range.
	Port int `json:"port,omitempty"`
}

// Validate checks that a policy is expressible and safe to compile.
func (p NetworkPolicy) Validate() error {
	if p.Workload == "" {
		return fmt.Errorf("network policy requires a workload")
	}
	for index, rule := range p.Ingress {
		if rule.FromWorkload == "" && rule.FromCIDR == "" {
			return fmt.Errorf("ingress rule %d names no source", index)
		}
		if rule.FromWorkload != "" && rule.FromCIDR != "" {
			// A rule with two sources is ambiguous about what it permits, and
			// the compiled output would silently pick one.
			return fmt.Errorf("ingress rule %d names both a workload and a CIDR", index)
		}
		if rule.FromCIDR != "" {
			if err := validateCIDR(rule.FromCIDR); err != nil {
				return fmt.Errorf("ingress rule %d: %w", index, err)
			}
		}
		if rule.Port < 0 || rule.Port > 65535 {
			return fmt.Errorf("ingress rule %d has port %d outside 0-65535", index, rule.Port)
		}
	}
	for index, rule := range p.Egress {
		if rule.ToWorkload == "" && rule.ToCIDR == "" {
			return fmt.Errorf("egress rule %d names no destination", index)
		}
		if rule.ToWorkload != "" && rule.ToCIDR != "" {
			return fmt.Errorf("egress rule %d names both a workload and a CIDR", index)
		}
		if rule.ToCIDR != "" {
			if err := validateCIDR(rule.ToCIDR); err != nil {
				return fmt.Errorf("egress rule %d: %w", index, err)
			}
		}
		if rule.Port < 0 || rule.Port > 65535 {
			return fmt.Errorf("egress rule %d has port %d outside 0-65535", index, rule.Port)
		}
	}
	return nil
}

// validateCIDR refuses anything that is not a well-formed prefix.
//
// A malformed CIDR compiled into a rule set is not a syntax error the kernel
// catches later: nft would reject the whole ruleset, taking every other
// workload's policy down with it.
func validateCIDR(value string) error {
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		return fmt.Errorf("%q is not a valid CIDR", value)
	}
	if network.IP.To4() == nil {
		// The compiler emits an IPv4 table; accepting a v6 prefix here would
		// produce a rule that silently never matches.
		return fmt.Errorf("%q is not an IPv4 CIDR", value)
	}
	return nil
}

// CompiledPolicy is the nftables ruleset one node should install.
//
// The rules are rendered rather than applied here, so the compilation is
// reviewable, diffable, and testable without a kernel. The node applies exactly
// these bytes, which means what a test asserts is what a host receives.
type CompiledPolicy struct {
	// Node names which node this ruleset was compiled for. Addresses are
	// node-local, so a ruleset is only meaningful on the node it was built for.
	Node string `json:"node"`
	// Table is the nftables table name the ruleset owns entirely.
	Table string `json:"table"`
	// Rules is the complete nft script, one statement per line.
	Rules []string `json:"rules"`
}

// PolicyTable is the nftables table a4s owns.
//
// a4s manages this table exclusively and flushes it on every apply, which is
// what makes the applied state equal the authorized state. Sharing a table with
// another tool would make that guarantee impossible.
const PolicyTable = "a4s"

// CompilePolicies renders typed intent into an nftables ruleset for one node.
//
// Endpoints are resolved through the directory, so a rule naming a workload
// expands to the addresses actually observed serving it. A rule that named a
// workload with nothing running compiles to no addresses, which denies rather
// than permits: failing closed is the only safe direction for a firewall.
func CompilePolicies(world World, ports map[string]int,
	policies []NetworkPolicy, node string) (CompiledPolicy, error) {

	compiled := CompiledPolicy{Node: node, Table: PolicyTable}
	directory := BuildDirectory(world, ports)

	// A fresh table on every apply. Incremental edits would let a rule nobody
	// authorized survive because no one remembered to remove it.
	compiled.Rules = append(compiled.Rules,
		fmt.Sprintf("add table inet %s", PolicyTable),
		fmt.Sprintf("flush table inet %s", PolicyTable),
		fmt.Sprintf("add chain inet %s input { type filter hook input priority 0 ; policy drop ; }", PolicyTable),
		fmt.Sprintf("add chain inet %s output { type filter hook output priority 0 ; policy drop ; }", PolicyTable),
		// Established traffic is always permitted. Without this every reply
		// packet would be dropped and nothing would work at all.
		fmt.Sprintf("add rule inet %s input ct state established,related accept", PolicyTable),
		fmt.Sprintf("add rule inet %s output ct state established,related accept", PolicyTable),
	)

	sorted := append([]NetworkPolicy(nil), policies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Workload < sorted[j].Workload })

	for _, policy := range sorted {
		if err := policy.Validate(); err != nil {
			return CompiledPolicy{}, err
		}
		local := localAddresses(world, policy.Workload, node)
		if len(local) == 0 {
			// This workload has no allocation on this node, so its policy
			// produces no local rules.
			continue
		}

		for _, address := range local {
			for _, rule := range policy.Ingress {
				sources := ruleAddresses(directory, rule.FromWorkload, rule.FromCIDR)
				for _, source := range sources {
					compiled.Rules = append(compiled.Rules,
						ingressRule(address, source, rule.Port))
				}
			}
			for _, rule := range policy.Egress {
				destinations := ruleAddresses(directory, rule.ToWorkload, rule.ToCIDR)
				for _, destination := range destinations {
					compiled.Rules = append(compiled.Rules,
						egressRule(address, destination, rule.Port))
				}
			}
		}
	}
	return compiled, nil
}

func ingressRule(destination, source string, port int) string {
	rule := fmt.Sprintf("add rule inet %s input ip saddr %s ip daddr %s",
		PolicyTable, source, destination)
	if port > 0 {
		rule += fmt.Sprintf(" tcp dport %d", port)
	}
	return rule + " accept"
}

func egressRule(source, destination string, port int) string {
	rule := fmt.Sprintf("add rule inet %s output ip saddr %s ip daddr %s",
		PolicyTable, source, destination)
	if port > 0 {
		rule += fmt.Sprintf(" tcp dport %d", port)
	}
	return rule + " accept"
}

// localAddresses lists the addresses of a workload's allocations on one node.
func localAddresses(world World, workload, node string) []string {
	var addresses []string
	for _, allocation := range world.Allocations {
		if allocation.Workload != workload || allocation.Node != node {
			continue
		}
		if allocation.Address == "" || allocation.Phase == AllocationStopped {
			continue
		}
		addresses = append(addresses, allocation.Address+"/32")
	}
	sort.Strings(addresses)
	return addresses
}

// ruleAddresses expands a rule's peer into concrete prefixes.
func ruleAddresses(directory map[string]Service, workload, cidr string) []string {
	if cidr != "" {
		return []string{cidr}
	}
	service, known := directory[workload]
	if !known {
		return nil
	}
	addresses := make([]string, 0, len(service.Endpoints))
	for _, endpoint := range service.Endpoints {
		addresses = append(addresses, endpoint.Address+"/32")
	}
	sort.Strings(addresses)
	return addresses
}

// Script renders the ruleset as an nft script.
func (c CompiledPolicy) Script() string {
	return strings.Join(c.Rules, "\n") + "\n"
}

// Fingerprint digests the compiled ruleset, so a node can tell an unchanged
// policy from one that needs reapplying.
func (c CompiledPolicy) Fingerprint() string {
	return digestOf(c.Script())
}

// digestOf is the shared content digest used for change detection.
func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
