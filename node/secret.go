package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"

	"github.com/arcbjorn/a4s/control"
)

// SecretMaterial is decrypted secret content.
//
// It is a distinct type rather than a plain []byte so that every place material
// crosses a boundary is visible in the type signature, and so it can refuse to
// render itself into a log line.
type SecretMaterial struct {
	value []byte
}

// String deliberately hides the value. Anything that formats a struct with
// %v — a log line, an error, a debug print — would otherwise leak material.
func (m SecretMaterial) String() string { return "[redacted]" }

// GoString hides the value from %#v as well.
func (m SecretMaterial) GoString() string { return "SecretMaterial{[redacted]}" }

// MarshalJSON refuses to serialize material. Evidence, events, and the world
// projection are all JSON, so a secret that could marshal would eventually be
// written to the durable log.
func (m SecretMaterial) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("secret material must never be serialized")
}

// Bytes exposes the material for writing to a mount. It is the single
// deliberate exit point.
func (m SecretMaterial) Bytes() []byte { return m.value }

// Len reports the size, which is safe to log and useful for diagnosis.
func (m SecretMaterial) Len() int { return len(m.value) }

// SecretBroker supplies node-scoped material for a reference.
//
// The broker never receives a goal, a proposal, or agent context: it is given a
// name and version and returns material for this node only. Replacing it with
// Vault or another backend changes nothing above this interface.
type SecretBroker interface {
	// Fetch returns material for a reference, or an error if this node is not
	// authorized for it.
	Fetch(context.Context, control.SecretRef) (SecretMaterial, error)
	Close() error
}

// SecretMount is where material was placed and which version it was.
type SecretMount struct {
	Name    string
	Version string
	Path    string
	// AlreadyMounted reports an idempotent repeat.
	AlreadyMounted bool
}

// Secrets handles secret actions on the node. It is a separate capability from
// the container runtime and the network because holding decryption authority is
// the most sensitive thing the node does.
type Secrets struct {
	broker SecretBroker
	// Root is the tmpfs directory under which material is written. Everything
	// beneath it is expected to be memory-backed so material never reaches a
	// disk that could later be imaged or recovered.
	root string

	mu     sync.Mutex
	mounts map[string]map[string]SecretMount
}

func NewSecrets(broker SecretBroker, root string) (*Secrets, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("secret root must be an absolute path")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create secret root: %w", err)
	}
	return &Secrets{
		broker: broker, root: root,
		mounts: make(map[string]map[string]SecretMount),
	}, nil
}

func (s *Secrets) Execute(ctx context.Context, action control.Action) (control.Evidence, error) {
	if s == nil || s.broker == nil {
		return control.Evidence{}, fmt.Errorf("node has no secret capability")
	}
	if action.Kind != control.ActionMountSecret {
		return control.Evidence{}, fmt.Errorf("secrets do not support action kind %q", action.Kind)
	}
	if action.Target == "" || action.Secret == nil {
		return control.Evidence{}, fmt.Errorf("mount secret requires target and reference")
	}
	ref := *action.Secret

	s.mu.Lock()
	if existing, ok := s.mounts[action.Target][ref.Name]; ok && existing.Version == ref.Version {
		s.mu.Unlock()
		// A replayed mount returns the existing one rather than re-fetching,
		// which would ask the broker for material the node already holds.
		return mountEvidence(action.Target, existing, true), nil
	}
	s.mu.Unlock()

	material, err := s.broker.Fetch(ctx, ref)
	if err != nil {
		// The error deliberately names only the reference. A broker error that
		// echoed material would defeat every other control here.
		return control.Evidence{}, fmt.Errorf("fetch secret %q version %q: %w",
			ref.Name, ref.Version, err)
	}
	if material.Len() == 0 {
		return control.Evidence{}, fmt.Errorf("secret %q version %q is empty", ref.Name, ref.Version)
	}

	path, err := s.write(action.Target, ref, material)
	if err != nil {
		return control.Evidence{}, err
	}
	mount := SecretMount{Name: ref.Name, Version: ref.Version, Path: path}

	s.mu.Lock()
	if s.mounts[action.Target] == nil {
		s.mounts[action.Target] = make(map[string]SecretMount)
	}
	s.mounts[action.Target][ref.Name] = mount
	s.mu.Unlock()

	return mountEvidence(action.Target, mount, false), nil
}

// mountEvidence reports version and location only. Material never appears.
func mountEvidence(allocation string, mount SecretMount, repeated bool) control.Evidence {
	return control.Evidence{
		Kind: control.EvidenceSecretMounted, Target: allocation,
		Observed: map[string]string{
			"secret":     mount.Name,
			"version":    mount.Version,
			"mount_path": mount.Path,
			"repeated":   fmt.Sprintf("%t", repeated),
		},
	}
}

// write places material under the allocation's directory with restrictive
// permissions.
func (s *Secrets) write(allocation string, ref control.SecretRef, material SecretMaterial) (string, error) {
	directory := filepath.Join(s.root, allocation)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create secret directory: %w", err)
	}
	// The mount path is goal-declared and already validated to be absolute and
	// free of traversal, but joining defensively means a future validation gap
	// cannot place material outside this allocation's directory.
	path := filepath.Join(directory, filepath.Base(ref.MountPath))
	if err := os.WriteFile(path, material.Bytes(), 0o400); err != nil {
		return "", fmt.Errorf("write secret %q: %w", ref.Name, err)
	}
	return path, nil
}

// Mounts reports the secret mounts for an allocation, so the container runtime
// can bind them into the workload.
func (s *Secrets) Mounts(allocation string) []SecretMount {
	s.mu.Lock()
	defer s.mu.Unlock()
	mounts := make([]SecretMount, 0, len(s.mounts[allocation]))
	for _, mount := range s.mounts[allocation] {
		mounts = append(mounts, mount)
	}
	return mounts
}

// Release removes an allocation's material. It is called during delete, because
// a deleted workload must not leave credentials readable on the node.
func (s *Secrets) Release(allocation string) error {
	s.mu.Lock()
	delete(s.mounts, allocation)
	s.mu.Unlock()
	directory := filepath.Join(s.root, allocation)
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove secret directory for %q: %w", allocation, err)
	}
	return nil
}

func (s *Secrets) Close() error {
	if s == nil || s.broker == nil {
		return nil
	}
	return s.broker.Close()
}

// SealedSecret is material encrypted to one node's identity.
//
// Sealing to the node means the control plane can distribute material it cannot
// itself read, and a stolen file is useless without that node's key.
type SealedSecret struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	NodeID       string `json:"node_id"`
	EphemeralKey []byte `json:"ephemeral_key"`
	Nonce        []byte `json:"nonce"`
	Ciphertext   []byte `json:"ciphertext"`
}

// FileBroker decrypts sealed secrets from a directory using the node's identity
// key. It is the initial backend; Vault or another store satisfies the same
// interface without changing anything above it.
type FileBroker struct {
	dir    string
	nodeID string
	// secretKey is the X25519 private key derived from the node's Ed25519
	// identity, so a node needs exactly one key rather than two to manage.
	secretKey [32]byte
}

func NewFileBroker(dir, nodeID string, identity ed25519.PrivateKey) (*FileBroker, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("secret directory must be an absolute path")
	}
	if len(identity) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("node identity key is invalid")
	}
	return &FileBroker{dir: dir, nodeID: nodeID, secretKey: x25519FromEd25519(identity)}, nil
}

func (b *FileBroker) Fetch(_ context.Context, ref control.SecretRef) (SecretMaterial, error) {
	path := filepath.Join(b.dir, ref.Name+"."+ref.Version+".sealed")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SecretMaterial{}, fmt.Errorf("secret %q version %q is not available to this node",
				ref.Name, ref.Version)
		}
		return SecretMaterial{}, fmt.Errorf("read sealed secret: %w", err)
	}
	var sealed SealedSecret
	if err := json.Unmarshal(raw, &sealed); err != nil {
		return SecretMaterial{}, fmt.Errorf("decode sealed secret %q: %w", ref.Name, err)
	}
	// The sealed file names its own identity. Refusing a mismatch stops a
	// misplaced file from being mounted as a secret it is not.
	if sealed.Name != ref.Name || sealed.Version != ref.Version {
		return SecretMaterial{}, fmt.Errorf("sealed secret does not match reference %q version %q",
			ref.Name, ref.Version)
	}
	if sealed.NodeID != b.nodeID {
		return SecretMaterial{}, fmt.Errorf("secret %q is sealed for another node", ref.Name)
	}

	value, err := unseal(sealed, b.secretKey)
	if err != nil {
		return SecretMaterial{}, err
	}
	return SecretMaterial{value: value}, nil
}

func (b *FileBroker) Close() error { return nil }

// Seal encrypts material to a node's public identity. It lives here so the
// sealing and unsealing formats cannot drift apart.
func Seal(name, version, nodeID string, nodeKey ed25519.PublicKey, value []byte) (SealedSecret, error) {
	if len(nodeKey) != ed25519.PublicKeySize {
		return SealedSecret{}, fmt.Errorf("node public key is invalid")
	}
	recipient, err := x25519FromEd25519Public(nodeKey)
	if err != nil {
		return SealedSecret{}, err
	}

	var ephemeralSecret [32]byte
	if _, err := io.ReadFull(rand.Reader, ephemeralSecret[:]); err != nil {
		return SealedSecret{}, fmt.Errorf("generate ephemeral key: %w", err)
	}
	ephemeralPublic, err := curve25519.X25519(ephemeralSecret[:], curve25519.Basepoint)
	if err != nil {
		return SealedSecret{}, fmt.Errorf("derive ephemeral public key: %w", err)
	}
	shared, err := curve25519.X25519(ephemeralSecret[:], recipient[:])
	if err != nil {
		return SealedSecret{}, fmt.Errorf("derive shared key: %w", err)
	}

	aead, err := chacha20poly1305.New(deriveKey(shared, ephemeralPublic, recipient[:]))
	if err != nil {
		return SealedSecret{}, fmt.Errorf("create cipher: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return SealedSecret{}, fmt.Errorf("generate nonce: %w", err)
	}
	// The name, version, and node bind the ciphertext to its identity, so a
	// sealed file cannot be renamed into a different secret.
	associated := []byte(name + "\x00" + version + "\x00" + nodeID)
	return SealedSecret{
		Name: name, Version: version, NodeID: nodeID,
		EphemeralKey: ephemeralPublic, Nonce: nonce,
		Ciphertext: aead.Seal(nil, nonce, value, associated),
	}, nil
}

func unseal(sealed SealedSecret, secretKey [32]byte) ([]byte, error) {
	if len(sealed.EphemeralKey) != 32 {
		return nil, fmt.Errorf("sealed secret has an invalid ephemeral key")
	}
	shared, err := curve25519.X25519(secretKey[:], sealed.EphemeralKey)
	if err != nil {
		return nil, fmt.Errorf("derive shared key: %w", err)
	}
	recipient, err := curve25519.X25519(secretKey[:], curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("derive recipient key: %w", err)
	}
	aead, err := chacha20poly1305.New(deriveKey(shared, sealed.EphemeralKey, recipient))
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	associated := []byte(sealed.Name + "\x00" + sealed.Version + "\x00" + sealed.NodeID)
	value, err := aead.Open(nil, sealed.Nonce, sealed.Ciphertext, associated)
	if err != nil {
		// The failure is reported without detail. A padding-oracle style error
		// distinction would leak information about the ciphertext.
		return nil, fmt.Errorf("secret %q version %q could not be decrypted", sealed.Name, sealed.Version)
	}
	return value, nil
}

// deriveKey mixes the shared secret with both public keys, so a key is bound to
// the exact pair it was derived for.
func deriveKey(shared, ephemeral, recipient []byte) []byte {
	digest := sha256.New()
	digest.Write(shared)
	digest.Write(ephemeral)
	digest.Write(recipient)
	return digest.Sum(nil)
}
