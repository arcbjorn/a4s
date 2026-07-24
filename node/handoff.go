package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/arcbjorn/a4s/control"
)

// quiesce confirms the volume is at rest, so a move can begin without a writer
// changing data under the snapshot.
//
// The node does not stop the writer here; the control plane detaches the
// allocation first. Quiesce is the node asserting there is nothing left to
// disturb, which is what the snapshot depends on.
func (v *Volumes) quiesce(action control.Action) (control.Evidence, error) {
	v.mu.Lock()
	record, ok := v.volumes[action.Volume.Name]
	v.mu.Unlock()
	if !ok {
		return control.Evidence{}, fmt.Errorf("volume %q does not exist on this node", action.Volume.Name)
	}
	if record.Owner != "" {
		return control.Evidence{}, fmt.Errorf(
			"volume %q is held by %q and cannot be quiesced", record.Name, record.Owner)
	}
	if action.Node == "" {
		return control.Evidence{}, fmt.Errorf("quiesce requires a target node")
	}
	return volumeEvidence(control.EvidenceVolumeQuiesced, record.Name, map[string]string{
		"to": action.Node,
	}), nil
}

// transfer runs on the target node. It fetches the moving snapshot from the
// shared store and proves it holds the data by reproducing the checksum.
//
// The origin is untouched: it still owns the volume and holds every byte. Only
// after the target proves it has the data does the control plane advance the
// move. A target that cannot reproduce the checksum has not received the data,
// so ownership never reaches it.

// transfer runs on the target node. It fetches the moving snapshot from the
// shared store and proves it holds the data by reproducing the checksum.
//
// The origin is untouched: it still owns the volume and holds every byte. Only
// after the target proves it has the data does the control plane advance the
// move. A target that cannot reproduce the checksum has not received the data,
// so ownership never reaches it.
func (v *Volumes) transfer(ctx context.Context, action control.Action) (control.Evidence, error) {
	if v.backups == nil {
		return control.Evidence{}, fmt.Errorf("node has no backup store to transfer through")
	}
	if action.Snapshot == "" {
		return control.Evidence{}, fmt.Errorf("transfer requires a snapshot id")
	}
	name := action.Volume.Name

	// The target may not know the volume yet. Create a local record so the
	// arriving data has a home, without an owner: nothing runs against it until
	// adoption completes.
	v.mu.Lock()
	record, ok := v.volumes[name]
	if !ok {
		record = VolumeRecord{Name: name, Path: filepath.Join(v.root, name)}
		if err := os.MkdirAll(record.Path, 0o750); err != nil {
			v.mu.Unlock()
			return control.Evidence{}, fmt.Errorf("prepare target volume %q: %w", name, err)
		}
		v.volumes[name] = record
		if err := v.persist(); err != nil {
			v.mu.Unlock()
			return control.Evidence{}, err
		}
	}
	v.mu.Unlock()

	// Fetch the snapshot into the target's own snapshot area, so a later
	// adoption restores from it exactly as any other snapshot.
	destination := filepath.Join(v.snapshots, name, action.Snapshot)
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return control.Evidence{}, fmt.Errorf("prepare snapshot directory: %w", err)
	}
	if _, err := os.Stat(destination); err != nil {
		if err := v.backups.Fetch(ctx, name, action.Snapshot, destination); err != nil {
			return control.Evidence{}, fmt.Errorf("fetch snapshot for transfer: %w", err)
		}
	}
	checksum, err := checksumTree(destination)
	if err != nil {
		return control.Evidence{}, err
	}
	// Reproducing the checksum is the proof of receipt. A mismatch means the
	// target does not hold what was snapshotted, so the move must not proceed.
	if action.Volume.Checksum != "" && action.Volume.Checksum != checksum {
		_ = os.RemoveAll(destination)
		return control.Evidence{}, fmt.Errorf(
			"transferred snapshot %q of volume %q failed verification: recorded %s, found %s",
			action.Snapshot, name, action.Volume.Checksum, checksum)
	}
	return volumeEvidence(control.EvidenceVolumeTransferred, name, map[string]string{
		"node": action.Node, "snapshot": action.Snapshot, "checksum": checksum,
	}), nil
}

// adopt runs on the target node once the control plane has confirmed the
// transfer. It restores the volume from the snapshot it received and takes
// ownership of the record.

// adopt runs on the target node once the control plane has confirmed the
// transfer. It restores the volume from the snapshot it received and takes
// ownership of the record.
func (v *Volumes) adopt(ctx context.Context, action control.Action) (control.Evidence, error) {
	name := action.Volume.Name
	if action.Snapshot == "" {
		return control.Evidence{}, fmt.Errorf("adopt requires the transferred snapshot id")
	}

	source := filepath.Join(v.snapshots, name, action.Snapshot)
	if _, err := os.Stat(source); err != nil {
		return control.Evidence{}, fmt.Errorf(
			"snapshot %q for volume %q was not transferred to this node", action.Snapshot, name)
	}
	checksum, err := checksumTree(source)
	if err != nil {
		return control.Evidence{}, err
	}
	// Verify again before writing. The snapshot could have been damaged between
	// transfer and adoption.
	if action.Volume.Checksum != "" && action.Volume.Checksum != checksum {
		return control.Evidence{}, fmt.Errorf(
			"snapshot %q of volume %q failed verification at adoption", action.Snapshot, name)
	}

	v.mu.Lock()
	record, ok := v.volumes[name]
	if !ok {
		v.mu.Unlock()
		return control.Evidence{}, fmt.Errorf("volume %q was not transferred to this node", name)
	}
	v.mu.Unlock()

	if err := copyTreeReplacing(source, record.Path); err != nil {
		return control.Evidence{}, fmt.Errorf("materialize adopted volume %q: %w", name, err)
	}
	return volumeEvidence(control.EvidenceVolumeAdopted, name, map[string]string{
		"node": action.Node, "snapshot": action.Snapshot, "checksum": checksum,
	}), nil
}

// verifyBackup proves a snapshot is recoverable without touching the live
// volume.
//
// It restores the snapshot into a scratch directory, checksums the result, and
// discards it. Nothing about the live volume changes, so this is safe to run on
// a schedule against a volume in active use. A restore test that could damage
// the data it protects would be worse than no test at all.
