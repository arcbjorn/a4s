package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/arcbjorn/a4s/internal/fsutil"
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
	decoded, err := decodeBase64(strings.TrimSpace(string(raw)))
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be a base64-encoded Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}

// writeKeypair stores a freshly generated keypair as path and path.pub.
//
// Neither file may already exist. Overwriting a private key destroys an
// identity that nodes, operators, or a keyset still trust, and the only
// recovery is re-enrolling everything that trusted it, so replacing one must be
// a deliberate removal rather than a re-run of the same command.
func writeKeypair(path string, public ed25519.PublicKey, private ed25519.PrivateKey) error {
	publicPath := path + ".pub"
	// Both paths are checked before either is written, so a stray .pub file is
	// reported up front instead of leaving a private key with no public half.
	for _, candidate := range []string{path, publicPath} {
		if _, err := os.Lstat(candidate); err == nil {
			return fmt.Errorf("refusing to overwrite existing key %s", candidate)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("check key path: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	if err := fsutil.WriteFileExclusive(path, []byte(encodeBase64(private)+"\n"), 0o600); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing key %s", path)
		}
		return fmt.Errorf("write private key: %w", err)
	}
	if err := fsutil.WriteFileExclusive(publicPath, []byte(encodeBase64(public)+"\n"), 0o644); err != nil {
		// The private half was created by this call, so removing it leaves no
		// unpaired key behind and the operator can simply retry.
		_ = os.Remove(path)
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing key %s", publicPath)
		}
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}
