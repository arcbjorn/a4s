package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// loadPrivateKey reads a base64 Ed25519 private key from a file. Keys are never
// accepted on the command line, where they would appear in shell history and
// process listings.
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	decoded, err := decodeBase64(strings.TrimSpace(string(raw)))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key must be a base64-encoded Ed25519 private key")
	}
	// An Ed25519 private key carries its public half after the seed. A file
	// whose halves disagree signs with one key while claiming another, and
	// every signature it produced would fail verification for a reason that
	// looks like a trust problem rather than a corrupt file.
	key := ed25519.PrivateKey(decoded)
	if !bytes.Equal(ed25519.NewKeyFromSeed(key.Seed()), key) {
		return nil, fmt.Errorf("signing key %s is corrupt: its public half does not match its seed", path)
	}
	return key, nil
}

// loadNodeKeys reads enrolled node public keys from <node-id>.pub files. A node
// absent from this directory cannot enroll.
func loadNodeKeys(dir string) (map[string]ed25519.PublicKey, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read node key directory: %w", err)
	}
	keys := make(map[string]ed25519.PublicKey)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pub") {
			continue
		}
		nodeID := strings.TrimSuffix(entry.Name(), ".pub")
		key, err := loadPublicKey(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", nodeID, err)
		}
		keys[nodeID] = key
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no enrolled node keys found in %s", dir)
	}
	return keys, nil
}

func decodeBase64(encoded string) ([]byte, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return base64.StdEncoding.DecodeString(encoded)
	}
	return decoded, nil
}

func loadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	encoded := strings.TrimSpace(string(raw))
	decoded, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(encoded)
	}
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be a base64-encoded Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}
