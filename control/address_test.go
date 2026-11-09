package control

import (
	"strings"
	"testing"
)

// A node reports allocation addresses, and those addresses are rendered into
// other nodes' firewall rules. Anything but an address must be refused.
func TestNetworkEvidenceRefusesNonAddress(t *testing.T) {
	for name, address := range map[string]string{
		"nft injection": "10.42.1.5/32 accept\nflush ruleset\n#",
		"prefix":        "10.42.1.5/24",
		"hostname":      "db.internal",
		"ipv6":          "fd00::5",
		"mapped ipv6":   "::ffff:10.42.1.5",
	} {
		t.Run(name, func(t *testing.T) {
			world, _ := policyWorld()
			err := projectInto(&world, Evidence{Kind: EvidenceNetworkAttached, Target: "api-0",
				Observed: map[string]string{"address": address}})
			if err == nil {
				t.Fatalf("projection accepted %q as an address", address)
			}
			if world.Allocations["api-0"].Address != "10.42.0.5" {
				t.Fatal("a refused address still replaced the recorded one")
			}
		})
	}
}

func TestNetworkEvidenceStoresCanonicalAddress(t *testing.T) {
	world, _ := policyWorld()
	if err := projectInto(&world, Evidence{Kind: EvidenceNetworkAttached, Target: "api-0",
		Observed: map[string]string{"address": "10.42.0.7"}}); err != nil {
		t.Fatal(err)
	}
	if got := world.Allocations["api-0"].Address; got != "10.42.0.7" {
		t.Fatalf("address = %q", got)
	}
}

// Defense in depth: an address that reached the world some other way, such as
// a hand-written base world, must still never reach rule text.
func TestCompilerSkipsMalformedAddresses(t *testing.T) {
	world, ports := policyWorld()
	world.Allocations["web-0"].Address = "10.42.0.9/32 accept\nflush ruleset\n#"
	policy := NetworkPolicy{Workload: "api", Ingress: []IngressRule{{FromWorkload: "web", Port: 8000}}}
	compiled, err := CompilePolicies(world, ports, []NetworkPolicy{policy}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if script := compiled.Script(); strings.Contains(script, "flush ruleset") {
		t.Fatalf("a malformed address injected a statement:\n%s", script)
	}
}

func TestValidateCIDRRefusesMappedIPv6(t *testing.T) {
	for _, cidr := range []string{"::ffff:10.0.0.0/104", "fd00::/8", "10.0.0.0/33", "not-a-cidr"} {
		if err := validateCIDR(cidr); err == nil {
			t.Errorf("validateCIDR accepted %q", cidr)
		}
	}
	for _, cidr := range []string{"10.0.0.0/8", "192.168.1.0/24", "0.0.0.0/0"} {
		if err := validateCIDR(cidr); err != nil {
			t.Errorf("validateCIDR refused %q: %v", cidr, err)
		}
	}
}
