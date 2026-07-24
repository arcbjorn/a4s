package control

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Explanation is the causal history of one target: why it exists, who decided
// it, what authorized the decision, and what evidence proved the outcome.
//
// This is possible because the event log records reasoning and authorization
// alongside mutation. A reconciliation loop that only stores desired and
// observed state cannot answer "why does this exist" after the fact; the
// decision is gone. Here it is durable, ordered, and hash-chained.
type Explanation struct {
	Target string      `json:"target"`
	Found  bool        `json:"found"`
	Steps  []Step      `json:"steps"`
	Goals  []string    `json:"goals"`
	Status TargetState `json:"status"`
}

// TargetState is what the log says became of the target.
type TargetState string

const (
	// StateServing means the target was last observed ready or reachable.
	StateServing TargetState = "serving"
	// StatePending means work was dispatched but no completing evidence
	// followed. This is the crash window made visible.
	StatePending TargetState = "pending"
	// StateFailed means the last decisive event was a failure or blockage.
	StateFailed TargetState = "failed"
	// StateRemoved means the target was deliberately stopped or deleted.
	StateRemoved TargetState = "removed"
	// StateUnknown means the log holds no decisive outcome.
	StateUnknown TargetState = "unknown"
)

// Step is one link in the causal chain, rendered for an operator rather than
// for a machine.
type Step struct {
	Sequence uint64    `json:"sequence"`
	At       time.Time `json:"at"`
	Type     EventType `json:"type"`
	Actor    string    `json:"actor"`
	GoalID   string    `json:"goal_id"`
	Summary  string    `json:"summary"`
	// Detail carries the reasoning, denial cause, or observed evidence.
	Detail string `json:"detail,omitempty"`
}

// Explain reconstructs why a target is in its current state.
//
// Relevance is transitive: an event matters if it names the target directly, or
// if it belongs to a proposal that acted on the target. That second hop is what
// recovers the agent's reasoning and the kernel's authorization, which name the
// proposal rather than the target.
func Explain(events []Event, target string) Explanation {
	explanation := Explanation{Target: target}
	if target == "" {
		return explanation
	}

	proposals := make(map[string]bool)
	goals := make(map[string]bool)
	for _, event := range events {
		if !mentionsTarget(event, target) {
			continue
		}
		explanation.Found = true
		if event.ProposalID != "" {
			proposals[event.ProposalID] = true
		}
		if event.GoalID != "" {
			goals[event.GoalID] = true
		}
	}
	if !explanation.Found {
		return explanation
	}

	for _, event := range events {
		relevant := mentionsTarget(event, target) ||
			(event.ProposalID != "" && proposals[event.ProposalID])
		// A goal acceptance explains the origin of everything downstream, but
		// only for goals that actually produced work on this target.
		if event.Type == EventGoalAccepted && goals[event.GoalID] {
			relevant = true
		}
		if !relevant {
			continue
		}
		explanation.Steps = append(explanation.Steps, describe(event))
	}

	explanation.Goals = sortedKeys(goals)
	explanation.Status = deriveState(events, target)
	return explanation
}

// mentionsTarget reports whether an event names the target directly.
func mentionsTarget(event Event, target string) bool {
	if event.Target == target {
		return true
	}
	return event.Evidence != nil && event.Evidence.Target == target
}

// deriveState reduces the log to the target's outcome. Later evidence wins, so
// a target that was ready and then failed reads as failed.
func deriveState(events []Event, target string) TargetState {
	state := StateUnknown
	for _, event := range events {
		if !mentionsTarget(event, target) {
			continue
		}
		switch {
		case event.Type == EventGoalBlocked:
			state = StateFailed
		case event.Type == EventActionDispatched:
			// Dispatch records intent before mutation. If nothing completes it,
			// this is exactly the crash window the ledger exists to close.
			state = StatePending
		case event.Evidence == nil:
			continue
		default:
			switch event.Evidence.Kind {
			case EvidenceAllocationReady:
				if event.Evidence.Observed["ready"] == "true" {
					state = StateServing
				} else {
					state = StateFailed
				}
			case EvidenceRouteReachable:
				state = StateServing
			case EvidenceAllocationRunning, EvidenceAllocationCreated, EvidenceImagePresent:
				if state != StateServing {
					state = StatePending
				}
			case EvidenceAllocationStopped, EvidenceAllocationDeleted, EvidenceRouteRemoved:
				state = StateRemoved
			case EvidenceAllocationFailed:
				state = StateFailed
			}
		}
	}
	return state
}

// describe renders one event as an operator-readable step.
func describe(event Event) Step {
	step := Step{
		Sequence: event.Sequence, At: event.At, Type: event.Type,
		Actor: event.Actor, GoalID: event.GoalID,
	}
	switch event.Type {
	case EventGoalAccepted:
		step.Summary = fmt.Sprintf("operator accepted goal %q", event.GoalID)
		step.Detail = event.Message
	case EventProposalCreated:
		step.Summary = fmt.Sprintf("%s proposed a plan", event.Actor)
		step.Detail = event.Message
	case EventProposalApproved:
		step.Summary = "kernel authorized the whole plan before any mutation"
	case EventProposalDenied:
		step.Summary = fmt.Sprintf("%s refused the plan", event.Actor)
		step.Detail = event.Message
	case EventActionDispatched:
		step.Summary = fmt.Sprintf("dispatched %s", event.Kind)
		step.Detail = "intent recorded before mutation"
	case EventActionCompleted:
		step.Summary = fmt.Sprintf("%s completed", event.Kind)
		step.Detail = describeEvidence(event.Evidence)
	case EventObservationRecorded:
		step.Summary = fmt.Sprintf("probe observed %s", event.Kind)
		step.Detail = describeEvidence(event.Evidence)
	case EventGoalAchieved:
		step.Summary = "goal verified from current evidence"
	case EventGoalBlocked:
		step.Summary = "blocked"
		step.Detail = event.Message
	default:
		step.Summary = string(event.Type)
		step.Detail = event.Message
	}
	return step
}

func describeEvidence(evidence *Evidence) string {
	if evidence == nil {
		return ""
	}
	keys := make([]string, 0, len(evidence.Observed))
	for key := range evidence.Observed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, evidence.Observed[key]))
	}
	if evidence.Source != "" {
		parts = append(parts, "source="+evidence.Source)
	}
	return strings.Join(parts, " ")
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// String renders the explanation as an operator-facing narrative.
func (e Explanation) String() string {
	if !e.Found {
		return fmt.Sprintf("no recorded history for %q", e.Target)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s is %s\n", e.Target, e.Status)
	if len(e.Goals) > 0 {
		fmt.Fprintf(&out, "requested by goal %s\n", strings.Join(e.Goals, ", "))
	}
	out.WriteString("\n")
	for _, step := range e.Steps {
		fmt.Fprintf(&out, "%3d  %-14s  %-16s  %s\n",
			step.Sequence, step.At.UTC().Format("15:04:05"), step.Actor, step.Summary)
		if step.Detail != "" {
			fmt.Fprintf(&out, "     %s\n", step.Detail)
		}
	}
	return out.String()
}
