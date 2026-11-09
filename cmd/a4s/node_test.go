package main

import (
	"testing"

	a4snode "github.com/arcbjorn/a4s/node"
)

// A secret is bound where the goal declared it, not at the node's tmpfs path.
func TestSecretMountSpecsUseDeclaredDestination(t *testing.T) {
	specs := secretMountSpecs([]a4snode.SecretMount{
		{Name: "db", Version: "v1", Path: "/run/a4s/secrets/web-0/db", MountPath: "/run/secrets/db"},
	})
	if len(specs) != 1 {
		t.Fatalf("want one mount, got %d", len(specs))
	}
	if specs[0].Source != "/run/a4s/secrets/web-0/db" {
		t.Fatalf("source = %q, want the node's tmpfs path", specs[0].Source)
	}
	if specs[0].Destination != "/run/secrets/db" {
		t.Fatalf("destination = %q, want the declared mount path", specs[0].Destination)
	}
}

func TestSecretMountSpecsEmpty(t *testing.T) {
	if specs := secretMountSpecs(nil); len(specs) != 0 {
		t.Fatalf("no mounts should produce no specs, got %v", specs)
	}
}
