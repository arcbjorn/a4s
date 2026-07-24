package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// keys manages controller signing key custody.
//
// Rotation is deliberately three steps rather than one. A key that stops
// signing must keep verifying until every envelope it signed has expired and
// every node has seen the new keyset; only then is retiring it safe. Collapsing
// that into a single command would make the unsafe path the easy one.
func keys(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("keys requires a subcommand: init, rotate, retire, or list")
	}
	switch args[0] {
	case "init":
		return keysInit(args[1:])
	case "rotate":
		return keysRotate(args[1:])
	case "retire":
		return keysRetire(args[1:])
	case "list":
		return keysList(args[1:])
	default:
		return fmt.Errorf("unknown keys subcommand %q", args[0])
	}
}

func keysInit(args []string) error {
	flags := flag.NewFlagSet("keys init", flag.ContinueOnError)
	keysetPath := flags.String("keyset", "", "path to write the controller keyset to")
	keyID := flags.String("key-id", "control-1", "identifier for the first signing key")
	out := flags.String("out", "", "path to write the new private signing key to")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keysetPath == "" || *out == "" {
		return fmt.Errorf("keyset and out are required")
	}
	if _, err := os.Stat(*keysetPath); err == nil {
		// Overwriting a keyset would silently strip the trust an existing fleet
		// depends on, so it must be a deliberate act rather than a re-run.
		return fmt.Errorf("keyset %s already exists; use keys rotate", *keysetPath)
	}

	public, err := generateSigningKey(*out)
	if err != nil {
		return err
	}
	set, err := control.NewKeySet(*keyID, public, nowUTC())
	if err != nil {
		return err
	}
	if err := writeKeySet(*keysetPath, set); err != nil {
		return err
	}
	fmt.Printf("initialized keyset %s with active key %s\n", *keysetPath, *keyID)
	fmt.Printf("private key written to %s\n", *out)
	return nil
}

func keysRotate(args []string) error {
	flags := flag.NewFlagSet("keys rotate", flag.ContinueOnError)
	keysetPath := flags.String("keyset", "", "path to the controller keyset")
	keyID := flags.String("key-id", "", "identifier for the new signing key")
	out := flags.String("out", "", "path to write the new private signing key to")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keysetPath == "" || *keyID == "" || *out == "" {
		return fmt.Errorf("keyset, key-id, and out are required")
	}

	set, err := readKeySet(*keysetPath)
	if err != nil {
		return err
	}
	previous, err := set.Active()
	if err != nil {
		return err
	}
	public, err := generateSigningKey(*out)
	if err != nil {
		return err
	}
	rotated, err := set.Rotate(*keyID, public, nowUTC())
	if err != nil {
		return err
	}
	if err := writeKeySet(*keysetPath, rotated); err != nil {
		return err
	}

	fmt.Printf("rotated: %s is now active, %s still verifies\n", *keyID, previous.KeyID)
	fmt.Printf("private key written to %s\n", *out)
	fmt.Printf("\nDistribute the keyset to every node, restart the server with the new\n")
	fmt.Printf("signing key, and only then run:\n  a4s keys retire --keyset %s --key-id %s\n",
		*keysetPath, previous.KeyID)
	return nil
}

func keysRetire(args []string) error {
	flags := flag.NewFlagSet("keys retire", flag.ContinueOnError)
	keysetPath := flags.String("keyset", "", "path to the controller keyset")
	keyID := flags.String("key-id", "", "key to stop trusting")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keysetPath == "" || *keyID == "" {
		return fmt.Errorf("keyset and key-id are required")
	}

	set, err := readKeySet(*keysetPath)
	if err != nil {
		return err
	}
	retired, err := set.Retire(*keyID, nowUTC())
	if err != nil {
		return err
	}
	if err := writeKeySet(*keysetPath, retired); err != nil {
		return err
	}
	fmt.Printf("retired %s; envelopes signed by it are no longer accepted\n", *keyID)
	fmt.Printf("distribute the updated keyset to every node\n")
	return nil
}

func keysList(args []string) error {
	flags := flag.NewFlagSet("keys list", flag.ContinueOnError)
	keysetPath := flags.String("keyset", "", "path to the controller keyset")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keysetPath == "" {
		return fmt.Errorf("keyset is required")
	}
	set, err := readKeySet(*keysetPath)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(set)
	}
	fmt.Printf("%-16s  %-9s  %-20s  %s\n", "KEY ID", "STATE", "CREATED", "NOTE")
	for _, key := range set.Sorted() {
		note := ""
		switch key.State {
		case control.KeyAccepted:
			note = "verifies but no longer signs"
		case control.KeyRetired:
			note = "no longer trusted"
		case control.KeyActive:
			note = "signs new envelopes"
		}
		fmt.Printf("%-16s  %-9s  %-20s  %s\n", key.KeyID, key.State,
			key.CreatedAt.Format("2006-01-02 15:04:05"), note)
	}
	return nil
}

// generateSigningKey writes a new private signing key and returns its public
// half. The private key is written 0600 and never printed.
func generateSigningKey(path string) (ed25519.PublicKey, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("refusing to overwrite existing key %s", path)
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(encodeBase64(private)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write signing key: %w", err)
	}
	if err := os.WriteFile(path+".pub", []byte(encodeBase64(public)+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("write public key: %w", err)
	}
	return public, nil
}

func readKeySet(path string) (control.KeySet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return control.KeySet{}, fmt.Errorf("read keyset: %w", err)
	}
	return control.DecodeKeySet(raw)
}

func writeKeySet(path string, set control.KeySet) error {
	encoded, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return fmt.Errorf("encode keyset: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create keyset directory: %w", err)
	}
	// The keyset is public material, but it decides what a node trusts, so it
	// is written read-only to others to make tampering require intent.
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// encodeBase64 matches the encoding keygen uses, so keys produced by either
// command load through the same path.
func encodeBase64(raw []byte) string {
	return base64.RawStdEncoding.EncodeToString(raw)
}

func nowUTC() time.Time { return time.Now().UTC() }
