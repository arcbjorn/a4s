package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arcbjorn/a4s/control"
)

// BackupStore holds snapshots somewhere other than the node that took them.
//
// The interface is deliberately narrow: put a verified snapshot, fetch it back,
// and say whether it exists. Anything richer would tempt the node into treating
// the backup store as a filesystem it can browse, when what matters is only that
// a specific verified snapshot can be retrieved after the origin node is gone.
type BackupStore interface {
	// Put ships a snapshot off-host and returns where it landed.
	Put(ctx context.Context, volume, snapshot, source string) (string, error)
	// Fetch retrieves a backup into a local directory.
	Fetch(ctx context.Context, volume, snapshot, destination string) error
	// Has reports whether a backup is present.
	Has(ctx context.Context, volume, snapshot string) (bool, error)
	Close() error
}

// DirectoryBackupStore writes backups to a filesystem path.
//
// It is the first implementation because it works over any mount an operator
// already trusts: an NFS export, a second disk, a mounted object store. Restic
// or an S3 client satisfies the same interface without changing the node.
//
// The path must not be under the node's own volume root. A backup that lives on
// the same disk as the data it protects does not survive the loss of that disk,
// which is the failure it exists for.
type DirectoryBackupStore struct {
	root string
}

func NewDirectoryBackupStore(root string, volumeRoot string) (*DirectoryBackupStore, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("backup root must be an absolute path")
	}
	// Refusing this is the whole point of an off-host backup. A store beneath
	// the volume root would be lost with the data it is meant to recover.
	if volumeRoot != "" && withinPath(root, volumeRoot) {
		return nil, fmt.Errorf(
			"backup root %q is inside the volume root %q and would not survive host loss",
			root, volumeRoot)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create backup root: %w", err)
	}
	return &DirectoryBackupStore{root: root}, nil
}

func (s *DirectoryBackupStore) path(volume, snapshot string) string {
	return filepath.Join(s.root, volume, snapshot)
}

func (s *DirectoryBackupStore) Put(_ context.Context, volume, snapshot, source string) (string, error) {
	destination := s.path(volume, snapshot)
	if _, err := os.Stat(destination); err == nil {
		// A backup is as immutable as the snapshot it holds. Overwriting one
		// would change what a verified location refers to.
		return destination, nil
	}
	// Stage and rename so an interrupted copy never leaves a partial tree under
	// a name a restore would treat as complete.
	staging := destination + ".partial"
	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("clear staging backup: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	if err := copyTree(source, staging); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("copy snapshot to backup store: %w", err)
	}
	if err := os.Rename(staging, destination); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("finalize backup: %w", err)
	}
	return destination, nil
}

func (s *DirectoryBackupStore) Fetch(_ context.Context, volume, snapshot, destination string) error {
	source := s.path(volume, snapshot)
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("backup of snapshot %q for volume %q is not in the store", snapshot, volume)
	}
	if err := os.RemoveAll(destination); err != nil {
		return fmt.Errorf("clear fetch destination: %w", err)
	}
	if err := copyTree(source, destination); err != nil {
		return fmt.Errorf("fetch backup: %w", err)
	}
	return nil
}

func (s *DirectoryBackupStore) Has(_ context.Context, volume, snapshot string) (bool, error) {
	if _, err := os.Stat(s.path(volume, snapshot)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *DirectoryBackupStore) Close() error { return nil }

// withinPath reports whether candidate lies inside parent.
func withinPath(candidate, parent string) bool {
	candidate = filepath.Clean(candidate)
	parent = filepath.Clean(parent)
	if candidate == parent {
		return true
	}
	return strings.HasPrefix(candidate, parent+string(filepath.Separator))
}

// WithBackupStore attaches an off-host backup store.
func (v *Volumes) WithBackupStore(store BackupStore) *Volumes {
	v.backups = store
	return v
}

// backup ships a verified snapshot off-host.
//
// Only a snapshot already taken and checksummed is shipped. Backing up a live
// volume directly would put a possibly inconsistent copy off-host under a name
// an operator would later trust.
func (v *Volumes) backup(ctx context.Context, action control.Action) (control.Evidence, error) {
	if v.backups == nil {
		return control.Evidence{}, fmt.Errorf("node has no backup store configured")
	}
	v.mu.Lock()
	record, ok := v.volumes[action.Volume.Name]
	v.mu.Unlock()
	if !ok {
		return control.Evidence{}, fmt.Errorf("volume %q does not exist on this node", action.Volume.Name)
	}
	if action.Snapshot == "" {
		return control.Evidence{}, fmt.Errorf("backup requires a snapshot id")
	}

	source := filepath.Join(v.snapshots, record.Name, action.Snapshot)
	if _, err := os.Stat(source); err != nil {
		return control.Evidence{}, fmt.Errorf(
			"snapshot %q of volume %q is not present on this node", action.Snapshot, record.Name)
	}
	// Checksum before shipping, so a snapshot that rotted on local disk is not
	// propagated to the store as though it were good.
	checksum, err := checksumTree(source)
	if err != nil {
		return control.Evidence{}, err
	}
	if action.Volume.Checksum != "" && action.Volume.Checksum != checksum {
		return control.Evidence{}, fmt.Errorf(
			"snapshot %q of volume %q failed verification before backup: recorded %s, found %s",
			action.Snapshot, record.Name, action.Volume.Checksum, checksum)
	}

	location, err := v.backups.Put(ctx, record.Name, action.Snapshot, source)
	if err != nil {
		return control.Evidence{}, fmt.Errorf("back up snapshot %q: %w", action.Snapshot, err)
	}
	return volumeEvidence(control.EvidenceVolumeBackedUp, record.Name, map[string]string{
		"snapshot": action.Snapshot, "location": location, "checksum": checksum,
	}), nil
}
