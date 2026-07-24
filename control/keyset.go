package control

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// KeyState describes where a controller signing key is in its lifecycle.
//
// Rotation is a three-state process rather than a swap because a signature
// outlives the moment it was made: envelopes signed by the old key are still in
// flight when the new key becomes active. A key must therefore stop signing
// before it stops being trusted.
type KeyState string

const (
	// KeyActive is the key new envelopes are signed with. Exactly one key is
	// active at a time.
	KeyActive KeyState = "active"
	// KeyAccepted still verifies signatures but no longer signs. This is the
	// overlap window that makes rotation safe without a fleet restart.
	KeyAccepted KeyState = "accepted"
	// KeyRetired neither signs nor verifies. Retired keys are kept in the set
	// so an operator can still tell which key produced a historical signature.
	KeyRetired KeyState = "retired"
)

// ControllerKey is one controller signing key and its lifecycle state.
type ControllerKey struct {
	KeyID string `json:"key_id"`
	// PublicKey is the base64 Ed25519 public half. The private half never
	// appears in a keyset: this structure is distributed to nodes.
	PublicKey string    `json:"public_key"`
	State     KeyState  `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	// RotatedAt is when this key stopped signing, set when it becomes accepted.
	RotatedAt time.Time `json:"rotated_at,omitempty"`
	// RetiredAt is when this key stopped being trusted.
	RetiredAt time.Time `json:"retired_at,omitempty"`
}

// KeySet is the distributed record of which controller keys a node trusts.
//
// It is deliberately data rather than configuration flags: a node needs to
// learn about a new controller key without an operator editing a command line
// on every host, and an auditor needs to see when each key entered and left
// service.
type KeySet struct {
	Version int             `json:"version"`
	Keys    []ControllerKey `json:"keys"`
}

// KeySetVersion is the current keyset format version.
const KeySetVersion = 1

// NewKeySet builds a keyset holding one active key.
func NewKeySet(keyID string, public ed25519.PublicKey, now time.Time) (KeySet, error) {
	if keyID == "" {
		return KeySet{}, fmt.Errorf("controller key requires an id")
	}
	if len(public) != ed25519.PublicKeySize {
		return KeySet{}, fmt.Errorf("controller key %q is not a valid ed25519 public key", keyID)
	}
	return KeySet{
		Version: KeySetVersion,
		Keys: []ControllerKey{{
			KeyID:     keyID,
			PublicKey: base64.StdEncoding.EncodeToString(public),
			State:     KeyActive,
			CreatedAt: now.UTC(),
		}},
	}, nil
}

// Validate checks the invariants a usable keyset must hold.
func (s KeySet) Validate() error {
	if s.Version != KeySetVersion {
		return fmt.Errorf("unsupported keyset version %d", s.Version)
	}
	if len(s.Keys) == 0 {
		return fmt.Errorf("keyset holds no keys")
	}
	seen := make(map[string]bool, len(s.Keys))
	active := 0
	for _, key := range s.Keys {
		if key.KeyID == "" {
			return fmt.Errorf("keyset holds a key with no id")
		}
		if seen[key.KeyID] {
			// A duplicate id would make "which key is this" ambiguous, and the
			// answer decides whether a signature verifies.
			return fmt.Errorf("keyset holds duplicate key id %q", key.KeyID)
		}
		seen[key.KeyID] = true

		switch key.State {
		case KeyActive:
			active++
		case KeyAccepted, KeyRetired:
		default:
			return fmt.Errorf("key %q has unknown state %q", key.KeyID, key.State)
		}
		if _, err := key.Decode(); err != nil {
			return err
		}
	}
	if active != 1 {
		// Two active keys would make it unclear which one a verifier should
		// expect; none would mean nothing can be signed.
		return fmt.Errorf("keyset must hold exactly one active key, found %d", active)
	}
	return nil
}

// Decode returns the parsed public key.
func (k ControllerKey) Decode() (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("key %q is not valid base64", k.KeyID)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("key %q is not a valid ed25519 public key", k.KeyID)
	}
	return ed25519.PublicKey(raw), nil
}

// Active returns the key new envelopes must be signed with.
func (s KeySet) Active() (ControllerKey, error) {
	for _, key := range s.Keys {
		if key.State == KeyActive {
			return key, nil
		}
	}
	return ControllerKey{}, fmt.Errorf("keyset holds no active key")
}

// TrustMap returns the keys a verifier should accept: active and accepted, but
// never retired.
//
// This is the function that makes rotation safe. A node built from this map
// verifies envelopes signed by the previous key for as long as that key remains
// accepted, and stops the moment it is retired.
func (s KeySet) TrustMap() (map[string]ed25519.PublicKey, error) {
	keys := make(map[string]ed25519.PublicKey, len(s.Keys))
	for _, key := range s.Keys {
		if key.State == KeyRetired {
			continue
		}
		public, err := key.Decode()
		if err != nil {
			return nil, err
		}
		keys[key.KeyID] = public
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("keyset trusts no keys")
	}
	return keys, nil
}

// Rotate introduces a new active key and demotes the current one to accepted.
//
// The previous key keeps verifying, which is what lets a fleet rotate without
// a coordinated restart: nodes that have not yet seen the new keyset continue
// to accept envelopes, and envelopes already in flight remain valid.
func (s KeySet) Rotate(keyID string, public ed25519.PublicKey, now time.Time) (KeySet, error) {
	if err := s.Validate(); err != nil {
		return KeySet{}, err
	}
	if keyID == "" {
		return KeySet{}, fmt.Errorf("controller key requires an id")
	}
	if len(public) != ed25519.PublicKeySize {
		return KeySet{}, fmt.Errorf("controller key %q is not a valid ed25519 public key", keyID)
	}
	for _, key := range s.Keys {
		if key.KeyID == keyID {
			// Reusing an id would silently change what a historical signature
			// means, which destroys the audit trail's value.
			return KeySet{}, fmt.Errorf("key id %q is already in the keyset", keyID)
		}
	}

	rotated := KeySet{Version: KeySetVersion, Keys: make([]ControllerKey, 0, len(s.Keys)+1)}
	for _, key := range s.Keys {
		if key.State == KeyActive {
			key.State = KeyAccepted
			key.RotatedAt = now.UTC()
		}
		rotated.Keys = append(rotated.Keys, key)
	}
	rotated.Keys = append(rotated.Keys, ControllerKey{
		KeyID:     keyID,
		PublicKey: base64.StdEncoding.EncodeToString(public),
		State:     KeyActive,
		CreatedAt: now.UTC(),
	})
	return rotated, rotated.Validate()
}

// Retire stops trusting a key.
//
// Retiring the active key is refused: it would leave the control plane unable
// to sign anything, turning a key-hygiene step into an outage. Rotate first.
func (s KeySet) Retire(keyID string, now time.Time) (KeySet, error) {
	if err := s.Validate(); err != nil {
		return KeySet{}, err
	}
	found := false
	retired := KeySet{Version: KeySetVersion, Keys: make([]ControllerKey, 0, len(s.Keys))}
	for _, key := range s.Keys {
		if key.KeyID == keyID {
			found = true
			if key.State == KeyActive {
				return KeySet{}, fmt.Errorf(
					"refusing to retire the active key %q: rotate to a new key first", keyID)
			}
			if key.State != KeyRetired {
				key.State = KeyRetired
				key.RetiredAt = now.UTC()
			}
		}
		retired.Keys = append(retired.Keys, key)
	}
	if !found {
		return KeySet{}, fmt.Errorf("key %q is not in the keyset", keyID)
	}
	return retired, retired.Validate()
}

// Sorted returns the keys in a stable order for display: active first, then
// accepted, then retired, each group newest first.
func (s KeySet) Sorted() []ControllerKey {
	keys := make([]ControllerKey, len(s.Keys))
	copy(keys, s.Keys)
	rank := map[KeyState]int{KeyActive: 0, KeyAccepted: 1, KeyRetired: 2}
	sort.SliceStable(keys, func(i, j int) bool {
		if rank[keys[i].State] != rank[keys[j].State] {
			return rank[keys[i].State] < rank[keys[j].State]
		}
		return keys[i].CreatedAt.After(keys[j].CreatedAt)
	})
	return keys
}

// DecodeKeySet strictly decodes a keyset document.
func DecodeKeySet(raw []byte) (KeySet, error) {
	var set KeySet
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&set); err != nil {
		return KeySet{}, fmt.Errorf("decode keyset: %w", err)
	}
	if decoder.More() {
		return KeySet{}, fmt.Errorf("keyset has trailing content")
	}
	if err := set.Validate(); err != nil {
		return KeySet{}, err
	}
	return set, nil
}
