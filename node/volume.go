package node

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"github.com/arcbjorn/a4s/control"
)

// VolumeRecord is the node's durable record of one volume and who holds it.
//
// It survives node restart because ownership is the property that must not be
// forgotten: a node that came back believing a volume was free could hand it to
// a second writer while the first is still running.
type VolumeRecord struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Owner is the allocation currently permitted to write.
	Owner string `json:"owner,omitempty"`
	// Generation is the ownership generation this node last observed. A
	// controller carrying an older generation has been superseded and is
	// refused, which is what fences a writer that was unreachable.
	Generation uint64 `json:"generation"`
}

// Volumes manages durable storage on one node.
//
// It is a separate capability from the container runtime because storage
// outlives every process that uses it, and because the failure mode here is
// permanent data loss rather than a restart.
type Volumes struct {
	root string
	// snapshots holds immutable copies, kept separate from live volumes so a
	// snapshot cannot be mistaken for one and mounted.
	snapshots string
	state     string
	// backups ships verified snapshots off-host. Without it a snapshot dies
	// with the node that holds it.
	backups BackupStore

	mu      sync.Mutex
	volumes map[string]VolumeRecord
}

func OpenVolumes(root, statePath string) (*Volumes, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(statePath) {
		return nil, fmt.Errorf("volume root and state path must be absolute")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create volume root: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o750); err != nil {
		return nil, fmt.Errorf("create volume state directory: %w", err)
	}
	snapshots := filepath.Join(filepath.Dir(root), "snapshots")
	if err := os.MkdirAll(snapshots, 0o750); err != nil {
		return nil, fmt.Errorf("create snapshot root: %w", err)
	}
	volumes := &Volumes{
		root: root, snapshots: snapshots, state: statePath,
		volumes: make(map[string]VolumeRecord),
	}
	if err := volumes.load(); err != nil {
		return nil, err
	}
	return volumes, nil
}

func (v *Volumes) load() error {
	file, err := os.Open(v.state)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open volume state: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		var record VolumeRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("decode volume state line %d: %w", line, err)
		}
		if record.Name == "" {
			return fmt.Errorf("volume state line %d has no name", line)
		}
		v.volumes[record.Name] = record
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read volume state: %w", err)
	}
	return nil
}

// persist rewrites the whole state file atomically, so a crash leaves either
// the old ownership or the new one and never a mixture.
func (v *Volumes) persist() error {
	names := make([]string, 0, len(v.volumes))
	for name := range v.volumes {
		names = append(names, name)
	}
	sort.Strings(names)

	var buffer []byte
	for _, name := range names {
		record, err := json.Marshal(v.volumes[name])
		if err != nil {
			return fmt.Errorf("encode volume record: %w", err)
		}
		buffer = append(append(buffer, record...), '\n')
	}
	return writeAtomic(v.state, buffer)
}

func (v *Volumes) Execute(ctx context.Context, action control.Action) (control.Evidence, error) {
	if v == nil {
		return control.Evidence{}, fmt.Errorf("node has no volume capability")
	}
	if action.Volume == nil {
		return control.Evidence{}, fmt.Errorf("%s requires a volume reference", action.Kind)
	}
	switch action.Kind {
	case control.ActionCreateVolume:
		return v.create(action)
	case control.ActionAttachVolume:
		return v.attach(action)
	case control.ActionDetachVolume:
		return v.detach(action)
	case control.ActionSnapshotVolume:
		return v.snapshot(ctx, action)
	case control.ActionRestoreSnapshot:
		return v.restore(ctx, action)
	case control.ActionBackupSnapshot:
		return v.backup(ctx, action)
	default:
		return control.Evidence{}, fmt.Errorf("volumes do not support action kind %q", action.Kind)
	}
}

func (v *Volumes) create(action control.Action) (control.Evidence, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	name := action.Volume.Name
	if existing, ok := v.volumes[name]; ok {
		// Re-creating must never touch the data or the ownership. A create that
		// silently emptied an existing volume would be indistinguishable from
		// data loss.
		return volumeEvidence(control.EvidenceVolumeCreated, name, map[string]string{
			"node": action.Node, "path": existing.Path, "created": "false",
		}), nil
	}
	path := filepath.Join(v.root, name)
	if err := os.MkdirAll(path, 0o750); err != nil {
		return control.Evidence{}, fmt.Errorf("create volume %q: %w", name, err)
	}
	v.volumes[name] = VolumeRecord{Name: name, Path: path}
	if err := v.persist(); err != nil {
		return control.Evidence{}, err
	}
	return volumeEvidence(control.EvidenceVolumeCreated, name, map[string]string{
		"node": action.Node, "path": path, "created": "true",
	}), nil
}

func (v *Volumes) attach(action control.Action) (control.Evidence, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	name := action.Volume.Name
	record, ok := v.volumes[name]
	if !ok {
		return control.Evidence{}, fmt.Errorf("volume %q does not exist on this node", name)
	}
	// The node's own single-writer check. The kernel enforces this too, but a
	// node that trusted the controller alone would hand a volume to a second
	// writer whenever the controller's view was stale.
	if record.Owner != "" && record.Owner != action.Target {
		return control.Evidence{}, fmt.Errorf("volume %q is held by allocation %q", name, record.Owner)
	}
	if record.Owner != action.Target {
		record.Owner = action.Target
		record.Generation++
		v.volumes[name] = record
		if err := v.persist(); err != nil {
			return control.Evidence{}, err
		}
	}
	return volumeEvidence(control.EvidenceVolumeAttached, name, map[string]string{
		"allocation": action.Target,
		"path":       record.Path,
		"mount_path": action.Volume.MountPath,
		"generation": fmt.Sprintf("%d", record.Generation),
	}), nil
}

func (v *Volumes) detach(action control.Action) (control.Evidence, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	name := action.Volume.Name
	record, ok := v.volumes[name]
	if !ok {
		// A replayed detach must succeed rather than fail on absence.
		return volumeEvidence(control.EvidenceVolumeDetached, name, map[string]string{
			"allocation": action.Target, "released": "false",
		}), nil
	}
	if record.Owner != "" && record.Owner != action.Target {
		// A stale detach from a fenced writer must not release a volume the
		// current owner is actively using.
		return control.Evidence{}, fmt.Errorf("allocation %q does not hold volume %q", action.Target, name)
	}
	record.Owner = ""
	record.Generation++
	v.volumes[name] = record
	if err := v.persist(); err != nil {
		return control.Evidence{}, err
	}
	return volumeEvidence(control.EvidenceVolumeDetached, name, map[string]string{
		"allocation": action.Target, "released": "true",
		"generation": fmt.Sprintf("%d", record.Generation),
	}), nil
}

// snapshot records a checksummed, immutable copy of a quiesced volume.
//
// The first implementation copies the directory. A filesystem-native snapshot
// satisfies the same contract, because what the control plane requires is an
// identifier and a checksum, not a particular mechanism.
func (v *Volumes) snapshot(_ context.Context, action control.Action) (control.Evidence, error) {
	v.mu.Lock()
	record, ok := v.volumes[action.Volume.Name]
	v.mu.Unlock()
	if !ok {
		return control.Evidence{}, fmt.Errorf("volume %q does not exist on this node", action.Volume.Name)
	}
	// Snapshotting a live writer produces a copy that may be internally
	// inconsistent. Refusing is safer than recording a snapshot an operator
	// would later trust for restore.
	if record.Owner != "" {
		return control.Evidence{}, fmt.Errorf(
			"volume %q is attached to %q; quiesce it before snapshotting", record.Name, record.Owner)
	}

	id := action.Snapshot
	if id == "" {
		return control.Evidence{}, fmt.Errorf("snapshot requires an id")
	}
	if !snapshotIDPattern.MatchString(id) {
		return control.Evidence{}, fmt.Errorf("snapshot id %q must be lowercase alphanumeric", id)
	}
	destination := filepath.Join(v.snapshots, record.Name, id)
	if _, err := os.Stat(destination); err == nil {
		// A snapshot is immutable. Overwriting one would silently change what a
		// previously verified id refers to.
		checksum, err := checksumTree(destination)
		if err != nil {
			return control.Evidence{}, err
		}
		return volumeEvidence(control.EvidenceVolumeSnapshotted, record.Name, map[string]string{
			"snapshot": id, "checksum": checksum, "repeated": "true",
		}), nil
	}

	// Copy into a staging directory first, so an interrupted snapshot never
	// leaves a partial tree under a name that looks complete.
	staging := destination + ".partial"
	if err := os.RemoveAll(staging); err != nil {
		return control.Evidence{}, fmt.Errorf("clear staging snapshot: %w", err)
	}
	if err := copyTree(record.Path, staging); err != nil {
		_ = os.RemoveAll(staging)
		return control.Evidence{}, fmt.Errorf("snapshot volume %q: %w", record.Name, err)
	}
	checksum, err := checksumTree(staging)
	if err != nil {
		_ = os.RemoveAll(staging)
		return control.Evidence{}, err
	}
	if err := os.Rename(staging, destination); err != nil {
		_ = os.RemoveAll(staging)
		return control.Evidence{}, fmt.Errorf("finalize snapshot: %w", err)
	}
	return volumeEvidence(control.EvidenceVolumeSnapshotted, record.Name, map[string]string{
		"snapshot": id, "checksum": checksum, "repeated": "false",
	}), nil
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

// restore overwrites a volume from a snapshot, after verifying the snapshot is
// intact.
//
// Verification happens before anything is overwritten. Restoring a corrupt
// snapshot would destroy the only remaining copy of the data and replace it
// with something unusable, which is worse than the failure being restored from.
func (v *Volumes) restore(ctx context.Context, action control.Action) (control.Evidence, error) {
	v.mu.Lock()
	record, ok := v.volumes[action.Volume.Name]
	v.mu.Unlock()
	if !ok {
		return control.Evidence{}, fmt.Errorf("volume %q does not exist on this node", action.Volume.Name)
	}
	if record.Owner != "" {
		return control.Evidence{}, fmt.Errorf(
			"volume %q is attached to %q; detach it before restoring", record.Name, record.Owner)
	}
	if action.Snapshot == "" {
		return control.Evidence{}, fmt.Errorf("restore requires a snapshot id")
	}

	source := filepath.Join(v.snapshots, record.Name, action.Snapshot)
	fetched := ""
	if _, err := os.Stat(source); err != nil {
		// The local snapshot is gone. This is the host-loss case the backup
		// store exists for, so fall back to it rather than failing.
		if v.backups == nil {
			return control.Evidence{}, fmt.Errorf(
				"snapshot %q of volume %q is not present on this node", action.Snapshot, record.Name)
		}
		fetched = filepath.Join(v.snapshots, record.Name, action.Snapshot+".fetched")
		if err := v.backups.Fetch(ctx, record.Name, action.Snapshot, fetched); err != nil {
			return control.Evidence{}, err
		}
		defer os.RemoveAll(fetched)
		source = fetched
	}
	checksum, err := checksumTree(source)
	if err != nil {
		return control.Evidence{}, err
	}
	// The controller supplies the checksum it recorded when the snapshot was
	// taken. A mismatch means the snapshot has changed since, so it is refused
	// rather than written over live data.
	if action.Volume.Checksum != "" && action.Volume.Checksum != checksum {
		return control.Evidence{}, fmt.Errorf(
			"snapshot %q of volume %q failed verification: recorded %s, found %s",
			action.Snapshot, record.Name, action.Volume.Checksum, checksum)
	}

	// Restore into a staging directory and swap, so a failure part way through
	// does not leave the volume holding a mixture of old and restored data.
	staging := record.Path + ".restoring"
	if err := os.RemoveAll(staging); err != nil {
		return control.Evidence{}, fmt.Errorf("clear staging restore: %w", err)
	}
	if err := copyTree(source, staging); err != nil {
		_ = os.RemoveAll(staging)
		return control.Evidence{}, fmt.Errorf("restore volume %q: %w", record.Name, err)
	}
	// Verify the restored copy before it replaces anything.
	restored, err := checksumTree(staging)
	if err != nil || restored != checksum {
		_ = os.RemoveAll(staging)
		return control.Evidence{}, fmt.Errorf(
			"restored copy of volume %q does not match the snapshot", record.Name)
	}

	previous := record.Path + ".replaced"
	_ = os.RemoveAll(previous)
	if err := os.Rename(record.Path, previous); err != nil && !os.IsNotExist(err) {
		_ = os.RemoveAll(staging)
		return control.Evidence{}, fmt.Errorf("set aside volume %q: %w", record.Name, err)
	}
	if err := os.Rename(staging, record.Path); err != nil {
		// Put the original back rather than leaving the volume missing.
		_ = os.Rename(previous, record.Path)
		_ = os.RemoveAll(staging)
		return control.Evidence{}, fmt.Errorf("replace volume %q: %w", record.Name, err)
	}
	_ = os.RemoveAll(previous)

	observed := map[string]string{"snapshot": action.Snapshot, "checksum": checksum}
	if fetched != "" {
		// Recording the source matters after an incident: an operator needs to
		// know whether recovery came from local state or from off-host backup.
		observed["source"] = "backup-store"
	} else {
		observed["source"] = "local-snapshot"
	}
	return volumeEvidence(control.EvidenceVolumeRestored, record.Name, observed), nil
}

// snapshotIDPattern keeps snapshot ids to safe path components, so an id can
// never escape the snapshot directory.
var snapshotIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// copyTree copies a directory recursively, preserving file modes.
func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		// Only regular files are copied. A symlink or device node in a snapshot
		// could point outside the volume when restored elsewhere.
		if !info.Mode().IsRegular() {
			return fmt.Errorf("volume contains a non-regular file %q", relative)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, content, info.Mode().Perm())
	})
}

// checksumTree computes a checksum over a directory's contents and structure.
//
// Paths are included alongside content, so moving a file between names changes
// the checksum. Entries are sorted so the result depends on the tree rather than
// on filesystem iteration order.
func checksumTree(root string) (string, error) {
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk %q: %w", root, err)
	}
	sort.Strings(paths)

	digest := sha256.New()
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		digest.Write([]byte(relative))
		digest.Write([]byte{0})
		digest.Write(content)
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func volumeEvidence(kind, target string, observed map[string]string) control.Evidence {
	return control.Evidence{Kind: kind, Target: target, Observed: observed}
}

// Mounts reports the volume mounts for an allocation, so the container runtime
// can bind them into the workload.
func (v *Volumes) Mounts(allocation string, refs []control.VolumeRef) []VolumeMountSpec {
	v.mu.Lock()
	defer v.mu.Unlock()
	mounts := make([]VolumeMountSpec, 0, len(refs))
	for _, ref := range refs {
		record, ok := v.volumes[ref.Name]
		if !ok || record.Owner != allocation {
			continue
		}
		mounts = append(mounts, VolumeMountSpec{
			Source: record.Path, Destination: ref.MountPath, ReadOnly: ref.ReadOnly,
		})
	}
	return mounts
}

// Owner reports which allocation holds a volume, which is what lets the node
// refuse a second writer.
func (v *Volumes) Owner(name string) (string, uint64, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	record, ok := v.volumes[name]
	if !ok {
		return "", 0, false
	}
	return record.Owner, record.Generation, true
}

func (v *Volumes) Close() error { return nil }
