// Command emit-policy renders a representative compiled ruleset.
//
// It exists so the nftables check exercises what the compiler actually emits
// rather than a sample that could drift from it.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/arcbjorn/a4s/control"
)

func main() {
	now := time.Now().UTC()
	world := control.World{
		Revision:   1,
		ObservedAt: now,
		Nodes:      map[string]*control.Node{"alpha": {ID: "alpha", Healthy: true}},
		Allocations: map[string]*control.Allocation{
			"api-0": {
				ID: "api-0", Workload: "api", Node: "alpha", Address: "10.42.0.5",
				Phase: control.AllocationRunning, Ready: true,
				ReadyExpiresAt: now.Add(time.Hour),
			},
			"web-0": {
				ID: "web-0", Workload: "web", Node: "alpha", Address: "10.42.0.9",
				Phase: control.AllocationRunning, Ready: true,
				ReadyExpiresAt: now.Add(time.Hour),
			},
		},
	}
	ports := map[string]int{"api": 8000, "web": 80}

	compiled, err := control.CompilePolicies(world, ports, []control.NetworkPolicy{{
		Workload: "api",
		Ingress:  []control.IngressRule{{FromWorkload: "web", Port: 8000}},
		Egress:   []control.EgressRule{{ToCIDR: "10.0.0.0/8", Port: 5432}},
	}}, "alpha")
	if err != nil {
		fmt.Fprintln(os.Stderr, "compile:", err)
		os.Exit(1)
	}
	fmt.Print(compiled.Script())
}
