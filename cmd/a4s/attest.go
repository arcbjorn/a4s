package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// attest signs a build provenance statement for one image.
//
// It exists so provenance is producible rather than theoretical. A policy that
// refuses unattested images is only enforceable if a build pipeline has some way
// to make an attestation, and requiring one without shipping the tool to create
// it would mean the setting could never be turned on.
//
// The signing key is read from a file rather than an argument, for the same
// reason every other key in this CLI is: a command line is visible in shell
// history and in process listings on a shared host.
func attest(args []string) error {
	flags := flag.NewFlagSet("attest", flag.ContinueOnError)
	image := flags.String("image", "", "digest-pinned image the statement covers")
	builder := flags.String("builder", "", "pipeline or person that produced the image")
	keyPath := flags.String("key", "", "path to the base64 Ed25519 signing key")
	keyID := flags.String("key-id", "", "identifier of the signing key")
	lifetime := flags.Duration("lifetime", 0,
		"how long the attestation stands; zero means it does not expire")
	out := flags.String("out", "", "path to write the attestation to; defaults to stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *image == "" || *builder == "" {
		return fmt.Errorf("image and builder are required")
	}
	if *keyPath == "" || *keyID == "" {
		return fmt.Errorf("key and key-id are required")
	}

	key, err := loadPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	builtAt := time.Now().UTC()
	statement := control.ImageStatement{
		Image: *image, Builder: *builder, BuiltAt: builtAt,
	}
	if *lifetime > 0 {
		statement.ExpiresAt = builtAt.Add(*lifetime)
	}
	signed, err := control.SignImageAttestation(statement, *keyID, key)
	if err != nil {
		return err
	}
	// Compact, never indented. The statement travels as a raw message so a
	// verifier checks the exact bytes that were signed, and indenting the
	// enclosing document rewrites those bytes: every attestation this command
	// wrote would then fail verification for a reason that looks like a bad
	// key.
	payload, err := json.Marshal(signed)
	if err != nil {
		return fmt.Errorf("encode attestation: %w", err)
	}
	payload = append(payload, '\n')

	if *out == "" {
		fmt.Print(string(payload))
		return nil
	}
	// An attestation is a public statement, not secret material, so it is
	// written world-readable like a public key rather than 0600 like a private
	// one.
	if err := os.WriteFile(*out, payload, 0o644); err != nil {
		return fmt.Errorf("write attestation: %w", err)
	}
	fmt.Printf("wrote %s for %s\n", *out, *image)
	return nil
}
