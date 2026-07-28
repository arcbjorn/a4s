package control

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Diagnosis explains why a goal is not converging, synthesized from recorded
// history rather than from live inspection.
//
// This is the one place where agent-style reasoning is unambiguously safe: a
// diagnosis reads the event log and produces text. It proposes no actions,
// holds no capability grants, and cannot mutate the world. A wrong diagnosis
// misleads an operator; it cannot break anything. That is why a model-backed
// implementation can be substituted here long before one belongs anywhere near
// placement.
type Diagnosis struct {
	GoalID string `json:"goal_id"`
	// Converged reports that the log ends with the goal achieved.
	Converged bool `json:"converged"`
	// Findings are ordered most-specific first, because the first cause an
	// operator reads should be the one worth acting on.
	Findings []Finding `json:"findings"`
	// Suggestion is the single next step, when history supports naming one.
	Suggestion string `json:"suggestion,omitempty"`
}

// Finding is one observed cause, with the evidence that supports it.
type Finding struct {
	Cause    string   `json:"cause"`
	Detail   string   `json:"detail"`
	Sequence uint64   `json:"sequence,omitempty"`
	Targets  []string `json:"targets,omitempty"`
}

// Diagnoser turns recorded history into an explanation. A model-backed
// implementation satisfies this interface and receives no additional authority.
type Diagnoser interface {
	Diagnose(goalID string, events []Event, world World) Diagnosis
}

// LogDiagnoser is the deterministic reference implementation. It reads only
// events and the world projection, exactly as any replacement must.
type LogDiagnoser struct{}

func (LogDiagnoser) Diagnose(goalID string, events []Event, world World) Diagnosis {
	diagnosis := Diagnosis{GoalID: goalID}

	var (
		blockages    []Event
		denials      []Event
		dispatched   = make(map[string]Event)
		completed    = make(map[string]bool)
		unreadyProbe []Event
		// touched is this goal's footprint: every target it has acted on. The
		// governor's state is keyed by target and is cluster-wide, so without a
		// footprint a diagnosis would report another goal's backoff as if it
		// were this one's.
		touched = make(map[string]bool)
	)
	for _, event := range events {
		if event.GoalID != goalID {
			continue
		}
		switch event.Type {
		case EventGoalAchieved:
			diagnosis.Converged = true
		case EventGoalAccepted:
			// A re-accepted goal starts a fresh attempt, so earlier failures
			// describe a run that is no longer in progress.
			diagnosis.Converged = false
			blockages, denials, unreadyProbe = nil, nil, nil
			dispatched = make(map[string]Event)
			completed = make(map[string]bool)
			touched = make(map[string]bool)
		case EventGoalBlocked:
			diagnosis.Converged = false
			blockages = append(blockages, event)
		case EventProposalDenied:
			denials = append(denials, event)
		case EventActionDispatched:
			dispatched[event.ActionID] = event
			if event.Target != "" {
				touched[event.Target] = true
			}
		case EventActionCompleted:
			completed[event.ActionID] = true
		case EventObservationRecorded:
			if event.Evidence != nil && event.Evidence.Kind == EvidenceAllocationReady &&
				event.Evidence.Observed["ready"] != "true" {
				unreadyProbe = append(unreadyProbe, event)
			}
		}
	}

	if diagnosis.Converged {
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Cause:  "converged",
			Detail: "the goal was verified from evidence and no later blockage was recorded",
		})
		return diagnosis
	}

	// An action dispatched without a completion is the crash window: the node
	// may have mutated the host without the controller learning the outcome.
	// It is reported first because it is the only finding that implies the
	// recorded world might disagree with reality.
	var incomplete []string
	for actionID, event := range dispatched {
		if !completed[actionID] {
			target := event.Target
			if target == "" {
				target = actionID
			}
			incomplete = append(incomplete, target)
		}
	}
	if len(incomplete) > 0 {
		sort.Strings(incomplete)
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Cause: "action dispatched without recorded completion",
			Detail: "the node may have mutated the host without the controller " +
				"recording the result; query the node ledger for these idempotency keys before retrying",
			Targets: incomplete,
		})
		diagnosis.Suggestion = "reconcile the node ledger against the event log before re-running the goal"
	}

	// The last blockage is the proximate cause an operator is looking for.
	if len(blockages) > 0 {
		last := blockages[len(blockages)-1]
		finding := Finding{
			Cause: "goal blocked", Detail: last.Message, Sequence: last.Sequence,
		}
		if last.Target != "" {
			finding.Targets = []string{last.Target}
		}
		diagnosis.Findings = append(diagnosis.Findings, finding)
		if diagnosis.Suggestion == "" {
			diagnosis.Suggestion = suggestFor(last.Message, world)
		}
	}

	// Denials are the kernel or an agent refusing a plan. They explain why no
	// progress was possible rather than why an action failed.
	if len(denials) > 0 {
		last := denials[len(denials)-1]
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Cause:    fmt.Sprintf("%s refused to act", last.Actor),
			Detail:   last.Message,
			Sequence: last.Sequence,
		})
		// A blockage often reports only that no agent could act, while the
		// denial that preceded it holds the specific reason. Prefer the
		// specific one: "no healthy node satisfies constraints" is actionable,
		// "no agent produced an authorized action" is not.
		if specific := suggestFor(last.Message, world); specific != "" {
			diagnosis.Suggestion = specific
		}
	}

	// The autonomy safeguards each stop a goal on purpose, and a goal stopped on
	// purpose looks exactly like one that is broken unless the diagnosis says
	// which. These findings come from the world rather than from a message,
	// because the reason a goal is not progressing right now may be a state it
	// is waiting out rather than anything that was last logged.
	diagnosis.Findings = append(diagnosis.Findings, governorFindings(world, touched)...)

	if len(unreadyProbe) > 0 {
		targets := make([]string, 0, len(unreadyProbe))
		for _, event := range unreadyProbe {
			targets = append(targets, event.Evidence.Target)
		}
		sort.Strings(targets)
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Cause:   "workload started but never became ready",
			Detail:  "the container ran and an independent probe measured it as not serving",
			Targets: targets,
		})
	}

	// Crashed allocations in the projection corroborate a failed rollout.
	var crashed []string
	for id, allocation := range world.Allocations {
		if allocation.Phase == AllocationStopped && (allocation.ExitCode != 0 || allocation.Restarts > 0) {
			crashed = append(crashed, fmt.Sprintf("%s (exit %d, %d restarts)",
				id, allocation.ExitCode, allocation.Restarts))
		}
	}
	if len(crashed) > 0 {
		sort.Strings(crashed)
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Cause:   "allocations exited abnormally",
			Detail:  "the image does not run successfully on this cluster",
			Targets: crashed,
		})
	}

	if len(diagnosis.Findings) == 0 {
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Cause:  "no recorded cause",
			Detail: "history holds no blockage, denial, or failure for this goal",
		})
	}
	return diagnosis
}

// governorFindings explains the states that hold a goal back deliberately.
//
// Each of these is a safeguard doing its job, which is exactly why they need
// explaining: a cluster that is pacing itself and a cluster that is broken both
// present as a goal that stopped moving, and an operator who cannot tell them
// apart will either wait on a real failure or start fixing a working brake.
func governorFindings(world World, touched map[string]bool) []Finding {
	var findings []Finding

	// Targets waiting out a failure backoff. Reported with the count, because
	// "failed twice" and "failed nine times" call for different responses.
	var waiting []string
	now := world.Now()
	for target := range touched {
		state := world.Backoff[target]
		if !state.Active(now) {
			continue
		}
		waiting = append(waiting, fmt.Sprintf("%s (%d consecutive failures, retryable at %s)",
			target, state.Failures, state.Until.UTC().Format(time.RFC3339)))
	}
	if len(waiting) > 0 {
		sort.Strings(waiting)
		findings = append(findings, Finding{
			Cause: "targets are waiting out a failure backoff",
			Detail: "these failed repeatedly and may not be recreated yet; the delay grows " +
				"with consecutive failures and resets when one is observed ready",
			Targets: waiting,
		})
	}

	// Nodes taken out of service. If nothing is left to place on, that is the
	// whole answer and no other finding will be more useful.
	var cordoned []string
	schedulable := 0
	for id, node := range world.Nodes {
		if node.Schedulable() {
			schedulable++
		}
		if node.Cordoned {
			reason := node.CordonReason
			if reason == "" {
				reason = "no reason recorded"
			}
			cordoned = append(cordoned, fmt.Sprintf("%s (%s)", id, reason))
		}
	}
	sort.Strings(cordoned)
	switch {
	case len(world.Nodes) > 0 && schedulable == 0:
		findings = append(findings, Finding{
			Cause: "no node can accept work",
			Detail: "every node is unhealthy or cordoned, so nothing can be placed " +
				"anywhere until one is restored or uncordoned",
			Targets: cordoned,
		})
	case len(cordoned) > 0:
		findings = append(findings, Finding{
			Cause:   "nodes are cordoned",
			Detail:  "these hold existing work but will not accept new allocations",
			Targets: cordoned,
		})
	}

	// Recent disruption is context rather than a cause on its own: it explains
	// a refusal an operator has already seen, and explains slowness they have
	// not.
	if recent := RecentDisruptions(world, now, DefaultDisruptionWindow); len(recent) > 0 {
		domains := make(map[string]bool, len(recent))
		for _, disruption := range recent {
			domains[disruption.Domain] = true
		}
		findings = append(findings, Finding{
			Cause: "the cluster is pacing disruption",
			Detail: fmt.Sprintf(
				"%d disruptive actions across %d failure domains in the last %s; "+
					"the budget and the one-domain-at-a-time rule slow further changes on purpose",
				len(recent), len(domains), DefaultDisruptionWindow),
		})
	}
	return findings
}

// suggestFor proposes a next step for a blockage whose cause is recognizable.
// It deliberately suggests rather than acts: every suggestion here would change
// operator intent or require authority the diagnoser does not hold.
func suggestFor(message string, world World) string {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "known-good image"):
		return "set the goal image to the last known-good digest, or fix the failing image"
	case strings.Contains(message, "availability floor"):
		return "add a replica or lower the availability floor before retrying the rollout"
	case strings.Contains(message, "is leased by proposal"):
		return "wait for the holding proposal to finish or expire, then retry"
	case strings.Contains(message, "public-route approval"):
		return "record a granted public-route approval for this goal"
	case strings.Contains(message, "no healthy node"):
		return "restore a healthy node that satisfies the placement constraints, or relax them"
	case strings.Contains(message, "lacks capacity"):
		return capacitySuggestion(world)
	case strings.Contains(message, "not connected"):
		return "start the node daemon and confirm it enrolls with the server"

	// The autonomy safeguards. Each names the specific knob, because "the
	// kernel refused it" sends an operator looking in the wrong place.
	// The failure-domain family is ordered most specific first: every message
	// below mentions a failure domain, so a generic match placed earlier would
	// shadow all of them and give the wrong next step for the right cause.
	case strings.Contains(message, "hold only"):
		return "add a failure domain, raise max_per_domain, or lower the replica count"
	case strings.Contains(message, "failure domains at once"):
		return "this is a proposing-agent bug: disruptive actions must name one failure domain"
	case strings.Contains(message, "was disrupted within the last"):
		return "wait out the cooldown; it exists so one domain's disruption is observed before the next"
	case strings.Contains(message, "failure domain"):
		return "add a node in another failure domain, or raise the workload's max_per_domain"
	case strings.Contains(message, "disruption budget exhausted"):
		return "wait for the window to pass; raise MaxDisruptionsPerWindow only if this rate is intended"
	case strings.Contains(message, "is in backoff"):
		return "fix why the target keeps failing; the delay clears when it is observed ready"
	case strings.Contains(message, "is cordoned"):
		return "uncordon the node, or let placement choose another once one is available"
	case strings.Contains(message, "provenance attestation"):
		return "sign the image with a trusted builder key using `a4s attest` and attach it to the goal"
	case strings.Contains(message, "image provenance"):
		return "the attached attestation did not verify; re-sign the exact digest with a trusted key"
	case strings.Contains(message, "ceiling reached"):
		return "raise the cluster ceiling, or release commitment by removing work that is no longer needed"
	default:
		return ""
	}
}

func capacitySuggestion(world World) string {
	for id, node := range world.Nodes {
		free := node.Capacity.Subtract(node.Used)
		return fmt.Sprintf("free capacity on %s is %dm CPU and %dMB memory; reduce the request or add a node",
			id, free.CPUMillis, free.MemoryMB)
	}
	return "reduce the workload's resource request or add a node"
}

// String renders the diagnosis for an operator.
func (d Diagnosis) String() string {
	var out strings.Builder
	if d.Converged {
		fmt.Fprintf(&out, "goal %s converged\n", d.GoalID)
		return out.String()
	}
	fmt.Fprintf(&out, "goal %s did not converge\n\n", d.GoalID)
	for _, finding := range d.Findings {
		fmt.Fprintf(&out, "  %s\n", finding.Cause)
		if finding.Detail != "" {
			fmt.Fprintf(&out, "    %s\n", finding.Detail)
		}
		if len(finding.Targets) > 0 {
			fmt.Fprintf(&out, "    affected: %s\n", strings.Join(finding.Targets, ", "))
		}
	}
	if d.Suggestion != "" {
		fmt.Fprintf(&out, "\nsuggested next step:\n  %s\n", d.Suggestion)
	}
	return out.String()
}
