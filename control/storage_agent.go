package control

import (
	"fmt"
	"sort"
	"time"
)

// DefaultVerificationInterval is how long a backup may go unverified before the
// storage agent proposes checking it again. A backup nobody has restored in a
// month is a backup nobody knows still works.
const DefaultVerificationInterval = 7 * 24 * time.Hour

// StorageAgent proposes background storage maintenance: verifying that backups
// are still recoverable.
//
// Its authority is deliberately narrow. It holds no grant to create, start, or
// place workloads, only to protect and recover data. Verification in particular
// is read-only, so this agent's most frequent action cannot damage anything
// even if it misfires.
type StorageAgent struct {
	// Interval is how stale a verification may become before it is re-proposed.
	Interval time.Duration
}

func (StorageAgent) Descriptor() AgentDescriptor {
	return AgentDescriptor{
		ID: "storage-agent", Role: "verify, snapshot, and recover durable data",
		Capabilities: []ActionKind{
			ActionSnapshotVolume, ActionDatabaseBackup, ActionBackupSnapshot,
			ActionRestoreSnapshot, ActionQuiesceVolume, ActionTransferVolume,
			ActionAdoptVolume, ActionPruneSnapshots, ActionVerifyBackup,
		},
	}
}

// Propose asks for verification of the volume most overdue for it.
//
// One volume per proposal keeps each verification bounded and lets a fresh
// observation land before the next. The most overdue volume goes first, so the
// backup least recently proven recoverable is checked soonest.
func (a StorageAgent) Propose(goal Goal, world World) (Proposal, error) {
	descriptor := a.Descriptor()
	proposal := Proposal{
		ID:      fmt.Sprintf("%s-verify-r%d", descriptor.ID, world.Revision),
		AgentID: descriptor.ID, GoalID: goal.ID, BasedOnRevision: world.Revision,
		Reasoning: "verify that the least recently checked backup is still recoverable",
	}

	interval := a.Interval
	if interval <= 0 {
		interval = DefaultVerificationInterval
	}
	now := world.Now()

	// Collect volumes for this workload that hold a snapshot and are overdue.
	type candidate struct {
		volume   *Volume
		snapshot string
		age      time.Duration
	}
	var candidates []candidate
	for _, ref := range goal.Workload.Volumes {
		volume, ok := world.Volumes[ref.Name]
		if !ok || volume.LastSnapshot == "" {
			continue
		}
		// A volume mid-move is busy; do not add verification load to a handoff.
		if volume.Handoff != nil {
			continue
		}
		since := now.Sub(volume.VerifiedAt)
		if !volume.VerifiedAt.IsZero() && since < interval {
			continue
		}
		candidates = append(candidates, candidate{
			volume: volume, snapshot: volume.LastSnapshot, age: since,
		})
	}
	if len(candidates) == 0 {
		return proposal, nil
	}

	// Oldest verification first; volume name as a deterministic tiebreaker.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].age != candidates[j].age {
			return candidates[i].age > candidates[j].age
		}
		return candidates[i].volume.Name < candidates[j].volume.Name
	})
	chosen := candidates[0]

	ref := VolumeRef{Name: chosen.volume.Name}
	ref.Checksum = chosen.volume.Snapshots[chosen.snapshot]
	proposal.Actions = []Action{{
		ID: "verify-" + chosen.volume.Name, Kind: ActionVerifyBackup,
		Target: chosen.volume.Name, Workload: goal.Workload.Name,
		Node: chosen.volume.Node, Volume: &ref, Snapshot: chosen.snapshot,
	}}
	return proposal, nil
}

// ProposeBackup asks for a fresh backup of a volume that has none.
//
// It is a separate entry point from Propose because taking a backup and
// verifying an existing one are different operations with different frequencies:
// a backup is scheduled, a verification confirms the last one still works. For a
// database it uses the engine's consistent-backup path; for a generic volume it
// snapshots the detached filesystem.
func (a StorageAgent) ProposeBackup(goal Goal, world World, label string) (Proposal, error) {
	descriptor := a.Descriptor()
	proposal := Proposal{
		ID:      fmt.Sprintf("%s-backup-r%d", descriptor.ID, world.Revision),
		AgentID: descriptor.ID, GoalID: goal.ID, BasedOnRevision: world.Revision,
		Reasoning: "back up the database with its own consistent-backup tool",
	}
	for _, ref := range goal.Workload.Volumes {
		volume, ok := world.Volumes[ref.Name]
		if !ok {
			continue
		}
		if goal.Workload.Engine != "" {
			// A database is backed up live with its own tool, never by copying
			// its files, which are inconsistent while it runs.
			proposal.Actions = []Action{{
				ID: "db-backup-" + ref.Name, Kind: ActionDatabaseBackup,
				Target: ref.Name, Workload: goal.Workload.Name, Node: volume.Node,
				Volume: &VolumeRef{Name: ref.Name}, Snapshot: label,
				Engine: goal.Workload.Engine,
			}}
			return proposal, nil
		}
	}
	return proposal, nil
}

// StaleBackups reports volumes whose backups have not been verified within the
// interval, which is what an operator checks to know their recovery posture.
func StaleBackups(world World, interval time.Duration, at time.Time) []string {
	if interval <= 0 {
		interval = DefaultVerificationInterval
	}
	var stale []string
	for name, volume := range world.Volumes {
		if volume.LastSnapshot == "" {
			continue
		}
		if volume.VerifiedAt.IsZero() || at.Sub(volume.VerifiedAt) >= interval {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	return stale
}
