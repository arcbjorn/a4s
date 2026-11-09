package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// generatedKey runs keygen into a fresh directory and returns the private key
// path; the public key sits beside it with a .pub suffix.
func generatedKey(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	mustRun(t, "keygen", "--out", path)
	return path
}

func TestKeygenWritesMatchingRestrictedKeypair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "operator")
	stdout := mustRun(t, "keygen", "--out", path)

	private, err := loadPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	public, err := loadPublicKey(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if !public.Equal(private.Public()) {
		t.Fatal("the public key file does not match the private key")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("private key mode %v is readable beyond its owner", info.Mode().Perm())
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm()&0o077 != 0 {
		t.Fatalf("key directory mode %v is accessible beyond its owner", dir.Mode().Perm())
	}
	// Key material must never reach a terminal.
	if strings.Contains(stdout, base64.RawStdEncoding.EncodeToString(private)) ||
		strings.Contains(stdout, base64.RawStdEncoding.EncodeToString(private.Seed())) {
		t.Fatal("keygen printed private key material")
	}
}

func TestKeygenRefusesToOverwritePrivateKey(t *testing.T) {
	path := generatedKey(t, "node")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, "keygen", "--out", path)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("want an overwrite refusal, got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused keygen still replaced the existing private key")
	}
}

func TestKeygenRefusesStrayPublicKeyWithoutWritingPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node")
	writeFile(t, path+".pub", "stale\n")
	_, _, err := runCLI(t, "keygen", "--out", path)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("want an overwrite refusal, got %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused keygen left an unpaired private key behind")
	}
}

func TestKeygenRequiresOut(t *testing.T) {
	_, _, err := runCLI(t, "keygen")
	if err == nil || !strings.Contains(err.Error(), "out is required") {
		t.Fatalf("want a missing-flag error, got %v", err)
	}
}

func TestKeysLifecycle(t *testing.T) {
	dir := t.TempDir()
	keyset := filepath.Join(dir, "keyset.json")
	first := filepath.Join(dir, "control-1")
	second := filepath.Join(dir, "control-2")

	mustRun(t, "keys", "init", "--keyset", keyset, "--key-id", "control-1", "--out", first)
	listed := mustRun(t, "keys", "list", "--keyset", keyset)
	if !strings.Contains(listed, "control-1") || !strings.Contains(listed, "signs new envelopes") {
		t.Fatalf("a fresh keyset should list one active key:\n%s", listed)
	}

	rotated := mustRun(t, "keys", "rotate", "--keyset", keyset, "--key-id", "control-2", "--out", second)
	if !strings.Contains(rotated, "keys retire --keyset "+keyset+" --key-id control-1") {
		t.Fatalf("rotation should print the retire step for the previous key:\n%s", rotated)
	}
	set := readKeySetJSON(t, keyset)
	trust, err := set.TrustMap()
	if err != nil {
		t.Fatal(err)
	}
	if len(trust) != 2 {
		t.Fatalf("both keys must verify during rotation, trust map has %d", len(trust))
	}
	assertTrusts(t, trust, "control-2", second)

	mustRun(t, "keys", "retire", "--keyset", keyset, "--key-id", "control-1")
	set = readKeySetJSON(t, keyset)
	trust, err = set.TrustMap()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := trust["control-1"]; ok || len(trust) != 1 {
		t.Fatalf("a retired key must stop verifying, trust map: %v", trust)
	}
	if listed := mustRun(t, "keys", "list", "--keyset", keyset); !strings.Contains(listed, "no longer trusted") {
		t.Fatalf("list should mark the retired key:\n%s", listed)
	}
}

func TestKeysInitRefusesExistingKeyset(t *testing.T) {
	dir := t.TempDir()
	keyset := filepath.Join(dir, "keyset.json")
	mustRun(t, "keys", "init", "--keyset", keyset, "--out", filepath.Join(dir, "k1"))
	_, _, err := runCLI(t, "keys", "init", "--keyset", keyset, "--out", filepath.Join(dir, "k2"))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want an existing-keyset refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "k2")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused init still generated a private key")
	}
}

func TestKeysRotateRejectedLeavesNoOrphanKey(t *testing.T) {
	dir := t.TempDir()
	keyset := filepath.Join(dir, "keyset.json")
	mustRun(t, "keys", "init", "--keyset", keyset, "--key-id", "control-1", "--out", filepath.Join(dir, "k1"))
	before, err := os.ReadFile(keyset)
	if err != nil {
		t.Fatal(err)
	}

	// Reusing a key id is refused by the keyset. That refusal must happen
	// before a private key is written, or the orphan blocks the next attempt.
	orphan := filepath.Join(dir, "k-dup")
	_, _, err = runCLI(t, "keys", "rotate", "--keyset", keyset, "--key-id", "control-1", "--out", orphan)
	if err == nil {
		t.Fatal("rotating to an existing key id must be refused")
	}
	if _, err := os.Stat(orphan); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused rotation left a private key nothing trusts")
	}
	after, err := os.ReadFile(keyset)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused rotation changed the keyset")
	}
}

func TestKeysRotateRefusesExistingKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyset := filepath.Join(dir, "keyset.json")
	existing := filepath.Join(dir, "k1")
	mustRun(t, "keys", "init", "--keyset", keyset, "--key-id", "control-1", "--out", existing)
	before, err := os.ReadFile(keyset)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, "keys", "rotate", "--keyset", keyset, "--key-id", "control-2", "--out", existing)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("want an overwrite refusal, got %v", err)
	}
	after, err := os.ReadFile(keyset)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the keyset must not trust a key whose private half was never written")
	}
}

func TestKeysRetireUnknownKeyFails(t *testing.T) {
	dir := t.TempDir()
	keyset := filepath.Join(dir, "keyset.json")
	mustRun(t, "keys", "init", "--keyset", keyset, "--out", filepath.Join(dir, "k1"))
	if _, _, err := runCLI(t, "keys", "retire", "--keyset", keyset, "--key-id", "absent"); err == nil {
		t.Fatal("retiring a key the keyset does not hold must fail")
	}
}

func TestKeysSubcommandErrors(t *testing.T) {
	if _, _, err := runCLI(t, "keys"); err == nil || !strings.Contains(err.Error(), "requires a subcommand") {
		t.Fatalf("want a missing-subcommand error, got %v", err)
	}
	if _, _, err := runCLI(t, "keys", "burn"); err == nil || !strings.Contains(err.Error(), "unknown keys subcommand") {
		t.Fatalf("want an unknown-subcommand error, got %v", err)
	}
	if _, _, err := runCLI(t, "keys", "list"); err == nil || !strings.Contains(err.Error(), "keyset is required") {
		t.Fatalf("want a missing-flag error, got %v", err)
	}
}

func TestLoadPrivateKeyRejectsMalformedKeys(t *testing.T) {
	dir := t.TempDir()
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append(ed25519.PrivateKey(nil), private...)
	corrupt[len(corrupt)-1] ^= 0xff

	tests := map[string]string{
		"not base64":   "%%%not-base64%%%",
		"wrong length": base64.RawStdEncoding.EncodeToString([]byte("short")),
		"public half":  base64.RawStdEncoding.EncodeToString(private.Public().(ed25519.PublicKey)),
		"corrupt pair": base64.RawStdEncoding.EncodeToString(corrupt),
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, filepath.Join(dir, strings.ReplaceAll(name, " ", "-")), content+"\n")
			if _, err := loadPrivateKey(path); err == nil {
				t.Fatal("a malformed private key must be refused")
			}
		})
	}
	if _, err := loadPrivateKey(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("a missing key file must be refused")
	}
}

func TestLoadKeysAcceptPaddedEncoding(t *testing.T) {
	dir := t.TempDir()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	privatePath := writeFile(t, filepath.Join(dir, "padded"), base64.StdEncoding.EncodeToString(private)+"\n")
	publicPath := writeFile(t, filepath.Join(dir, "padded.pub"), base64.StdEncoding.EncodeToString(public)+"\n")
	if _, err := loadPrivateKey(privatePath); err != nil {
		t.Fatalf("padded private key: %v", err)
	}
	if _, err := loadPublicKey(publicPath); err != nil {
		t.Fatalf("padded public key: %v", err)
	}
}

func TestLoadNodeKeys(t *testing.T) {
	dir := t.TempDir()
	for _, node := range []string{"alpha", "beta"} {
		public, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, node+".pub"), base64.RawStdEncoding.EncodeToString(public))
	}
	writeFile(t, filepath.Join(dir, "README"), "not a key")
	if err := os.Mkdir(filepath.Join(dir, "nested.pub"), 0o700); err != nil {
		t.Fatal(err)
	}

	keys, err := loadNodeKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys["alpha"] == nil || keys["beta"] == nil {
		t.Fatalf("want alpha and beta, got %v", keys)
	}

	writeFile(t, filepath.Join(dir, "broken.pub"), "garbage")
	if _, err := loadNodeKeys(dir); err == nil || !strings.Contains(err.Error(), `"broken"`) {
		t.Fatalf("a malformed key should be refused by name, got %v", err)
	}
	if _, err := loadNodeKeys(t.TempDir()); err == nil {
		t.Fatal("a directory without keys must be refused, or no node could ever enroll")
	}
}

func readKeySetJSON(t *testing.T, path string) control.KeySet {
	t.Helper()
	stdout := mustRun(t, "keys", "list", "--keyset", path, "--json")
	set, err := control.DecodeKeySet([]byte(stdout))
	if err != nil {
		t.Fatalf("keys list --json is not a keyset: %v\n%s", err, stdout)
	}
	if !json.Valid([]byte(stdout)) {
		t.Fatal("keys list --json emitted invalid JSON")
	}
	return set
}

func assertTrusts(t *testing.T, trust map[string]ed25519.PublicKey, keyID, privatePath string) {
	t.Helper()
	private, err := loadPrivateKey(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !trust[keyID].Equal(private.Public()) {
		t.Fatalf("keyset trusts a different key for %s than the one written", keyID)
	}
}
