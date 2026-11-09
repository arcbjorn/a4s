package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	a4snode "github.com/arcbjorn/a4s/node"
)

// seal encrypts secret material to one node's identity.
//
// Material is read from a file rather than an argument, because a command line
// is visible in shell history and process listings. It is never echoed back.
func seal(args []string) error {
	flags := flag.NewFlagSet("seal", flag.ContinueOnError)
	name := flags.String("secret", "", "secret name")
	version := flags.String("version", "", "secret version")
	nodeID := flags.String("node", "", "node this secret is sealed for")
	nodeKeyPath := flags.String("node-key", "", "path to the node's base64 Ed25519 public key")
	in := flags.String("in", "", "file holding the secret material")
	outDir := flags.String("out", "", "directory to write the sealed secret into")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" || *version == "" || *nodeID == "" || *nodeKeyPath == "" || *in == "" || *outDir == "" {
		return fmt.Errorf("secret, version, node, node-key, in, and out are required")
	}
	nodeKey, err := loadPublicKey(*nodeKeyPath)
	if err != nil {
		return err
	}
	material, err := os.ReadFile(*in)
	if err != nil {
		return fmt.Errorf("read secret material: %w", err)
	}
	if len(material) == 0 {
		return fmt.Errorf("secret material is empty")
	}
	sealed, err := a4snode.Seal(*name, *version, *nodeID, nodeKey, material)
	if err != nil {
		return err
	}
	document, err := json.Marshal(sealed)
	if err != nil {
		return fmt.Errorf("encode sealed secret: %w", err)
	}
	if err := os.MkdirAll(*outDir, 0o750); err != nil {
		return fmt.Errorf("create sealed secret directory: %w", err)
	}
	path := filepath.Join(*outDir, *name+"."+*version+".sealed")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		return fmt.Errorf("write sealed secret: %w", err)
	}
	// Report only the reference. The material is never echoed.
	fmt.Printf("sealed %s version %s for node %s: %s\n", *name, *version, *nodeID, path)
	return nil
}
