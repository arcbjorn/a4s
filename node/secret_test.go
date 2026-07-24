package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

const secretValue = "hunter2-database-password"

func secretRef() control.SecretRef {
	return control.SecretRef{Name: "db-password", Version: "v3", MountPath: "/run/secrets/db"}
}

// sealedFixture writes a secret sealed to a node's identity, returning a broker
// that can open it.
func sealedFixture(t *testing.T, value string) (*FileBroker, string, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ref := secretRef()
	sealed, err := Seal(ref.Name, ref.Version, "base", public, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ref.Name+"."+ref.Version+".sealed")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	broker, err := NewFileBroker(dir, "base", private)
	if err != nil {
		t.Fatal(err)
	}
	return broker, dir, private
}

// Material sealed to a node must round-trip through that node's identity key.
func TestSealedSecretRoundTrips(t *testing.T) {
	broker, _, _ := sealedFixture(t, secretValue)
	material, err := broker.Fetch(context.Background(), secretRef())
	if err != nil {
		t.Fatal(err)
	}
	if string(material.Bytes()) != secretValue {
		t.Fatal("decrypted material does not match what was sealed")
	}
}

// A secret sealed for one node must be useless to another. This is what makes
// distributing sealed files safe.
func TestSecretSealedForAnotherNodeIsRefused(t *testing.T) {
	_, dir, _ := sealedFixture(t, secretValue)
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewFileBroker(dir, "base", otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Fetch(context.Background(), secretRef()); err == nil {
		t.Fatal("a foreign node decrypted the secret")
	}
}

// A sealed file renamed to impersonate another secret must be refused, since
// the identity is bound into the ciphertext.
func TestRenamedSealedSecretIsRefused(t *testing.T) {
	broker, dir, _ := sealedFixture(t, secretValue)
	ref := secretRef()
	original := filepath.Join(dir, ref.Name+"."+ref.Version+".sealed")
	renamed := filepath.Join(dir, "api-token.v1.sealed")
	if err := os.Rename(original, renamed); err != nil {
		t.Fatal(err)
	}
	_, err := broker.Fetch(context.Background(), control.SecretRef{
		Name: "api-token", Version: "v1", MountPath: "/run/secrets/api",
	})
	if err == nil || !strings.Contains(err.Error(), "does not match reference") {
		t.Fatalf("a renamed secret was accepted: %v", err)
	}
}

// Tampered ciphertext must fail, and the failure must not describe why.
func TestTamperedSecretIsRefused(t *testing.T) {
	broker, dir, _ := sealedFixture(t, secretValue)
	ref := secretRef()
	path := filepath.Join(dir, ref.Name+"."+ref.Version+".sealed")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sealed SealedSecret
	if err := json.Unmarshal(raw, &sealed); err != nil {
		t.Fatal(err)
	}
	sealed.Ciphertext[0] ^= 0xff
	tampered, _ := json.Marshal(sealed)
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = broker.Fetch(context.Background(), ref)
	if err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	if !strings.Contains(err.Error(), "could not be decrypted") {
		t.Fatalf("decryption failure leaked detail: %v", err)
	}
}

// Mounting writes material to the node's tmpfs with restrictive permissions and
// reports only version and location.
func TestMountWritesMaterialAndReportsVersionOnly(t *testing.T) {
	broker, _, _ := sealedFixture(t, secretValue)
	secrets, err := NewSecrets(broker, filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	ref := secretRef()
	evidence, err := secrets.Execute(context.Background(), control.Action{
		Kind: control.ActionMountSecret, Target: "web-0", Workload: "web", Secret: &ref,
	})
	if err != nil {
		t.Fatal(err)
	}

	if evidence.Observed["version"] != "v3" || evidence.Observed["secret"] != "db-password" {
		t.Fatalf("evidence lost the reference: %+v", evidence.Observed)
	}
	// The critical assertion: no observed field carries material.
	for key, value := range evidence.Observed {
		if strings.Contains(value, secretValue) {
			t.Fatalf("evidence field %q leaked secret material", key)
		}
	}

	path := evidence.Observed["mount_path"]
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("material was not written: %v", err)
	}
	if string(written) != secretValue {
		t.Fatal("mounted material does not match the sealed value")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o400 {
		t.Fatalf("secret file has permissive mode %v", mode)
	}
}

// A replayed mount must return the existing one rather than re-fetching.
func TestMountIsIdempotent(t *testing.T) {
	broker, _, _ := sealedFixture(t, secretValue)
	secrets, err := NewSecrets(broker, filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	ref := secretRef()
	action := control.Action{
		Kind: control.ActionMountSecret, Target: "web-0", Workload: "web", Secret: &ref,
	}
	first, err := secrets.Execute(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	second, err := secrets.Execute(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed["mount_path"] != second.Observed["mount_path"] {
		t.Fatal("replayed mount changed the path")
	}
	if second.Observed["repeated"] != "true" {
		t.Fatalf("replayed mount was not reported as a repeat: %+v", second.Observed)
	}
}

// Deleting an allocation must remove its material, so a decommissioned workload
// leaves no readable credentials behind.
func TestReleaseRemovesMaterial(t *testing.T) {
	broker, _, _ := sealedFixture(t, secretValue)
	root := filepath.Join(t.TempDir(), "secrets")
	secrets, err := NewSecrets(broker, root)
	if err != nil {
		t.Fatal(err)
	}
	ref := secretRef()
	evidence, err := secrets.Execute(context.Background(), control.Action{
		Kind: control.ActionMountSecret, Target: "web-0", Workload: "web", Secret: &ref,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := evidence.Observed["mount_path"]

	if err := secrets.Release("web-0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("secret material survived release: %v", err)
	}
	if len(secrets.Mounts("web-0")) != 0 {
		t.Fatal("release left mount records behind")
	}
}

// Deleting through the composite runtime must release secrets, or a forgotten
// call would leave credentials on the node.
func TestDeleteReleasesSecrets(t *testing.T) {
	broker, _, _ := sealedFixture(t, secretValue)
	secrets, err := NewSecrets(broker, filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	ref := secretRef()
	if _, err := secrets.Execute(context.Background(), control.Action{
		Kind: control.ActionMountSecret, Target: "web-0", Workload: "web", Secret: &ref,
	}); err != nil {
		t.Fatal(err)
	}

	runtime := &CompositeRuntime{
		Containers: NewContainerRuntime(&supervisedBackend{states: map[string]BackendState{}}),
		Secrets:    secrets,
	}
	if _, err := runtime.Execute(context.Background(), control.Action{
		Kind: control.ActionDeleteAllocation, Target: "web-0", Workload: "web",
	}); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Mounts("web-0")) != 0 {
		t.Fatal("delete left secret mounts in place")
	}
}

// A missing secret must fail the mount rather than starting a workload without
// the credentials it was promised.
func TestMissingSecretFailsMount(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewFileBroker(t.TempDir(), "base", private)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := NewSecrets(broker, filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	ref := secretRef()
	_, err = secrets.Execute(context.Background(), control.Action{
		Kind: control.ActionMountSecret, Target: "web-0", Workload: "web", Secret: &ref,
	})
	if err == nil || !strings.Contains(err.Error(), "not available to this node") {
		t.Fatalf("expected a missing-secret failure, got %v", err)
	}
}

// Material must refuse to serialize. Evidence, events, and the world projection
// are all JSON, so anything marshalable eventually reaches the durable log.
func TestSecretMaterialRefusesSerialization(t *testing.T) {
	material := SecretMaterial{value: []byte(secretValue)}

	if _, err := json.Marshal(material); err == nil {
		t.Fatal("secret material serialized to JSON")
	}
	// Formatting verbs used by log lines and error messages must not expose it.
	for _, rendered := range []string{
		material.String(),
		material.GoString(),
	} {
		if strings.Contains(rendered, secretValue) {
			t.Fatalf("secret material leaked through formatting: %s", rendered)
		}
	}
}

// The container must receive secrets as read-only bind mounts of node-side
// files, never as values that passed through the control plane.
func TestContainerReceivesSecretMountsReadOnly(t *testing.T) {
	backend := &fakeBackend{}
	runtime := NewContainerRuntime(backend)
	runtime.SecretMountsFor = func(string) []SecretMountSpec {
		return []SecretMountSpec{{Source: "/run/a4s/secrets/web-0/db", Destination: "/run/secrets/db"}}
	}

	if _, err := runtime.Execute(context.Background(), control.Action{
		Kind: control.ActionCreateAllocation, Target: "web-0", Workload: "web",
		Image: testImage, Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}); err != nil {
		t.Fatal(err)
	}
	if len(backend.created.SecretMounts) != 1 {
		t.Fatalf("container spec omitted secret mounts: %+v", backend.created)
	}
	mount := backend.created.SecretMounts[0]
	if mount.Destination != "/run/secrets/db" {
		t.Fatalf("unexpected mount destination: %+v", mount)
	}
}
