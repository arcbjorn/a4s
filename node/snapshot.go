package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/arcbjorn/a4s/control"
)

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

// quiesce confirms the volume is at rest, so a move can begin without a writer
// changing data under the snapshot.
//
// The node does not stop the writer here; the control plane detaches the
// allocation first. Quiesce is the node asserting there is nothing left to
// disturb, which is what the snapshot depends on.

// verifyBackup proves a snapshot is recoverable without touching the live
// volume.
//
// It restores the snapshot into a scratch directory, checksums the result, and
// discards it. Nothing about the live volume changes, so this is safe to run on
// a schedule against a volume in active use. A restore test that could damage
// the data it protects would be worse than no test at all.
func (v *Volumes) verifyBackup(ctx context.Context, action control.Action) (control.Evidence, error) {
	v.mu.Lock()
	record, ok := v.volumes[action.Volume.Name]
	v.mu.Unlock()
	if !ok {
		return control.Evidence{}, fmt.Errorf("volume %q does not exist on this node", action.Volume.Name)
	}
	if action.Snapshot == "" {
		return control.Evidence{}, fmt.Errorf("verify requires a snapshot id")
	}

	// Restore into scratch space that is never the live volume path. Sourcing
	// from the local snapshot when present, and the backup store otherwise, so
	// verification covers whichever copy would be used in a real recovery.
	scratch := filepath.Join(v.snapshots, record.Name, action.Snapshot+".verify")
	defer os.RemoveAll(scratch)

	source := filepath.Join(v.snapshots, record.Name, action.Snapshot)
	if _, err := os.Stat(source); err != nil {
		if v.backups == nil {
			return v.verificationFailure(record.Name, action.Snapshot,
				"snapshot is not present on this node and no backup store is configured"), nil
		}
		if err := v.backups.Fetch(ctx, record.Name, action.Snapshot, scratch); err != nil {
			return v.verificationFailure(record.Name, action.Snapshot,
				"backup could not be fetched: "+err.Error()), nil
		}
		source = scratch
	} else {
		// Copy the local snapshot into scratch so the checksum is computed the
		// same way a real restore would materialize it, not read in place.
		if err := copyTree(source, scratch); err != nil {
			return control.Evidence{}, fmt.Errorf("stage verification copy: %w", err)
		}
		source = scratch
	}

	checksum, err := checksumTree(source)
	if err != nil {
		return control.Evidence{}, err
	}
	// A recorded checksum lets the node confirm the copy is what was snapshotted.
	// A mismatch means the backup would not recover the right data.
	if action.Volume.Checksum != "" && action.Volume.Checksum != checksum {
		return v.verificationFailure(record.Name, action.Snapshot,
			"checksum mismatch: recorded "+action.Volume.Checksum+", found "+checksum), nil
	}
	return control.Evidence{
		Kind: control.EvidenceBackupVerified, Target: record.Name,
		Observed: map[string]string{
			"snapshot": action.Snapshot, "verified": "true", "checksum": checksum,
		},
	}, nil
}

// verificationFailure reports a backup that could not be proven recoverable.
// A failure is evidence too: it is exactly what an operator needs to see before
// they depend on a backup that would not work.

// verificationFailure reports a backup that could not be proven recoverable.
// A failure is evidence too: it is exactly what an operator needs to see before
// they depend on a backup that would not work.
func (v *Volumes) verificationFailure(volume, snapshot, reason string) control.Evidence {
	return control.Evidence{
		Kind: control.EvidenceBackupVerified, Target: volume,
		Observed: map[string]string{
			"snapshot": snapshot, "verified": "false", "reason": reason,
		},
	}
}

// prune removes snapshots beyond the retention count, keeping the most recent.
//
// The node applies retention from what is on disk rather than trusting a list
// from the controller, and enforces its own floor: it keeps the most recent
// `retain` and never removes the last snapshot standing. Deleting a snapshot is
// irreversible, so the node errs toward keeping too many rather than too few.

// prune removes snapshots beyond the retention count, keeping the most recent.
//
// The node applies retention from what is on disk rather than trusting a list
// from the controller, and enforces its own floor: it keeps the most recent
// `retain` and never removes the last snapshot standing. Deleting a snapshot is
// irreversible, so the node errs toward keeping too many rather than too few.
func (v *Volumes) prune(action control.Action) (control.Evidence, error) {
	v.mu.Lock()
	record, ok := v.volumes[action.Volume.Name]
	v.mu.Unlock()
	if !ok {
		return control.Evidence{}, fmt.Errorf("volume %q does not exist on this node", action.Volume.Name)
	}
	dir := filepath.Join(v.snapshots, record.Name)
	present, err := listSnapshotDirs(dir)
	if err != nil {
		return control.Evidence{}, err
	}

	retain := action.Retain
	if retain < 1 {
		// The node's own floor. Even a controller asking to keep zero must not
		// produce a volume with no recovery point.
		retain = 1
	}
	// Protect the most recent `retain` snapshots, taking recency from directory
	// modification time so the node does not depend on the controller's order.
	protected, err := recentSnapshots(dir, present, retain)
	if err != nil {
		return control.Evidence{}, err
	}

	var removed []string
	for _, id := range present {
		if protected[id] {
			continue
		}
		// Never remove the last snapshot on disk, whatever retention says. A
		// volume with no recovery point is the state pruning must never produce.
		if len(present)-len(removed) <= 1 {
			break
		}
		if action.DryRun {
			removed = append(removed, id)
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, id)); err != nil {
			return control.Evidence{}, fmt.Errorf("remove snapshot %q: %w", id, err)
		}
		removed = append(removed, id)
	}
	sort.Strings(removed)
	return volumeEvidence(control.EvidenceSnapshotsPruned, record.Name, map[string]string{
		"removed": strings.Join(removed, "\n"),
		"dry_run": fmt.Sprintf("%t", action.DryRun),
	}), nil
}

// listSnapshotDirs returns the snapshot ids present on disk, ignoring staging
// directories left by an interrupted snapshot or fetch.

// listSnapshotDirs returns the snapshot ids present on disk, ignoring staging
// directories left by an interrupted snapshot or fetch.
func listSnapshotDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshot directory: %w", err)
	}
	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".partial") || strings.HasSuffix(name, ".fetched") {
			continue
		}
		ids = append(ids, name)
	}
	sort.Strings(ids)
	return ids, nil
}

// recentSnapshots returns the `retain` most recently modified snapshots, which
// the node protects from pruning independently of the controller's ordering.

// recentSnapshots returns the `retain` most recently modified snapshots, which
// the node protects from pruning independently of the controller's ordering.
func recentSnapshots(dir string, ids []string, retain int) (map[string]bool, error) {
	type dated struct {
		id      string
		modTime int64
	}
	entries := make([]dated, 0, len(ids))
	for _, id := range ids {
		info, err := os.Stat(filepath.Join(dir, id))
		if err != nil {
			return nil, fmt.Errorf("stat snapshot %q: %w", id, err)
		}
		entries = append(entries, dated{id: id, modTime: info.ModTime().UnixNano()})
	}
	// Newest first, with id as a tiebreaker so the result is deterministic when
	// two snapshots share a modification time.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modTime != entries[j].modTime {
			return entries[i].modTime > entries[j].modTime
		}
		return entries[i].id > entries[j].id
	})
	protected := make(map[string]bool)
	for i := 0; i < len(entries) && i < retain; i++ {
		protected[entries[i].id] = true
	}
	return protected, nil
}

// copyTreeReplacing writes source over destination through a staging swap, so a
// failure partway through never leaves a mixture of old and new data.

// copyTreeReplacing writes source over destination through a staging swap, so a
// failure partway through never leaves a mixture of old and new data.
func copyTreeReplacing(source, destination string) error {
	staging := destination + ".adopting"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := copyTree(source, staging); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	previous := destination + ".replaced"
	_ = os.RemoveAll(previous)
	if err := os.Rename(destination, previous); err != nil && !os.IsNotExist(err) {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		_ = os.Rename(previous, destination)
		_ = os.RemoveAll(staging)
		return err
	}
	_ = os.RemoveAll(previous)
	return nil
}

// WithBackupStore attaches an off-host backup store.

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

// snapshotIDPattern keeps snapshot ids to safe path components, so an id can
// never escape the snapshot directory.
var snapshotIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// copyTree copies a directory recursively, preserving file modes.
