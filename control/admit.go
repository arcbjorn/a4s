package control

import (
	"fmt"
	"slices"
)

// Evidence admission.
//
// Evidence is the only input that advances the world, so its source decides
// what it may say. A node answering an action reports what that action did to
// the target it named; a prober reports the measurement it was asked for.
// Neither may report anything else. An executor able to answer a pull with an
// approval, or a prober able to answer a readiness check with a cordon, would
// turn one compromised node into cluster authority, because the signature on
// its report proves only who said it, not that it was theirs to say.
//
// Operator decisions and diagnoses therefore never appear in these tables.
// They reach the log only through the server's signature-verified paths.

// evidenceRule is what one action kind may report and about which target.
type evidenceRule struct {
	kinds  []string
	target func(Action) string
}

func actionTarget(action Action) string { return action.Target }
func nodeTarget(action Action) string   { return action.Node }
func imageTarget(action Action) string  { return action.Image }

func volumeTarget(action Action) string {
	if action.Volume == nil {
		return ""
	}
	return action.Volume.Name
}

func allocationRule(kinds ...string) evidenceRule {
	return evidenceRule{kinds: kinds, target: actionTarget}
}

func volumeRule(kind string) evidenceRule {
	return evidenceRule{kinds: []string{kind}, target: volumeTarget}
}

func nodeRule(kind string) evidenceRule {
	return evidenceRule{kinds: []string{kind}, target: nodeTarget}
}

// actionEvidence is default-deny: an action kind without an entry admits no
// evidence at all. A new action added without a rule therefore fails loudly
// the first time it runs, rather than trusting whatever its executor returns.
var actionEvidence = map[ActionKind]evidenceRule{
	ActionPullImage:        {kinds: []string{EvidenceImagePresent}, target: imageTarget},
	ActionCreateAllocation: allocationRule(EvidenceAllocationCreated),
	ActionAttachNetwork:    allocationRule(EvidenceNetworkAttached),
	ActionMountSecret:      allocationRule(EvidenceSecretMounted),
	ActionStartAllocation:  allocationRule(EvidenceAllocationRunning),
	ActionStopAllocation:   allocationRule(EvidenceAllocationStopped),
	ActionDeleteAllocation: allocationRule(EvidenceAllocationDeleted),
	ActionGrantTools:       allocationRule(EvidenceToolsGranted),
	// A drain either completes or reports the instance still finishing its
	// task; both are honest answers to the same request.
	ActionDrainAllocation: allocationRule(EvidenceAllocationDrained, EvidenceAgentDraining),
	ActionPublishRoute:    allocationRule(EvidenceRouteReachable),
	ActionCordonNode:      allocationRule(EvidenceNodeCordoned),
	ActionUncordonNode:    allocationRule(EvidenceNodeUncordoned),

	ActionCreateVolume:    volumeRule(EvidenceVolumeCreated),
	ActionAttachVolume:    volumeRule(EvidenceVolumeAttached),
	ActionDetachVolume:    volumeRule(EvidenceVolumeDetached),
	ActionSnapshotVolume:  volumeRule(EvidenceVolumeSnapshotted),
	ActionRestoreSnapshot: volumeRule(EvidenceVolumeRestored),
	ActionBackupSnapshot:  volumeRule(EvidenceVolumeBackedUp),
	ActionQuiesceVolume:   volumeRule(EvidenceVolumeQuiesced),
	ActionTransferVolume:  volumeRule(EvidenceVolumeTransferred),
	ActionAdoptVolume:     volumeRule(EvidenceVolumeAdopted),
	ActionPruneSnapshots:  volumeRule(EvidenceSnapshotsPruned),
	ActionVerifyBackup:    volumeRule(EvidenceBackupVerified),
	ActionDatabaseBackup:  volumeRule(EvidenceDatabaseBackedUp),

	ActionApplyPolicy:   nodeRule(EvidencePolicyApplied),
	ActionPublishZone:   nodeRule(EvidenceZonePublished),
	ActionCollectImages: nodeRule(EvidenceImagesCollected),
}

// AdmitActionEvidence reports whether evidence is an acceptable answer to an
// executed action: a kind that action produces, about the target it named.
func AdmitActionEvidence(action Action, evidence Evidence) error {
	rule, ok := actionEvidence[action.Kind]
	if !ok {
		return fmt.Errorf("action kind %q admits no evidence", action.Kind)
	}
	if !slices.Contains(rule.kinds, evidence.Kind) {
		return fmt.Errorf("%s may not be answered with %q evidence", action.Kind, evidence.Kind)
	}
	if want := rule.target(action); evidence.Target != want {
		return fmt.Errorf("%q evidence names %q, but the %s action targeted %q",
			evidence.Kind, evidence.Target, action.Kind, want)
	}
	return nil
}

// probeEvidence is what a prober may report for each check kind. Probes
// measure; none of these kinds carries authority.
var probeEvidence = map[string][]string{
	CheckAllocationReady:   {EvidenceAllocationReady},
	CheckAgentReady:        {EvidenceAgentReady},
	CheckRouteReachable:    {EvidenceRouteReachable},
	CheckAllocationDrained: {EvidenceAllocationDrained, EvidenceAgentDraining},
}

// AdmitProbeEvidence reports whether evidence is an acceptable answer to a
// check: the measurement that check asked for, about the target it named.
func AdmitProbeEvidence(check Check, evidence Evidence) error {
	kinds, ok := probeEvidence[check.Kind]
	if !ok {
		return fmt.Errorf("check kind %q admits no probe evidence", check.Kind)
	}
	if !slices.Contains(kinds, evidence.Kind) {
		return fmt.Errorf("a %s check may not be answered with %q evidence", check.Kind, evidence.Kind)
	}
	if evidence.Target != check.Target {
		return fmt.Errorf("%q evidence names %q, but the check targeted %q",
			evidence.Kind, evidence.Target, check.Target)
	}
	return nil
}
