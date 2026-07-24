package control

import (
	"fmt"
	"sort"
	"strings"
)

// ModelContext is the bounded, redacted view a model-backed agent may see.
//
// It exists because a model is the least trustworthy consumer of state in the
// system: its input may be logged by a provider, retained, or replayed, and its
// output is untrusted text. So the context is built by subtraction. Nothing
// reaches it unless a field was deliberately copied here, which means a new
// field added to World or Event does not silently become model input.
//
// Everything in this struct is a reference, an identifier, a count, or a kind.
// There is nowhere to put a secret value, a task payload, or a model prompt,
// and that is a property of the type rather than of the code that fills it.
type ModelContext struct {
	// GoalID and Objective describe what was asked. The objective is operator
	// prose and is already visible in the event log.
	GoalID    string `json:"goal_id"`
	Objective string `json:"objective"`
	// Workload carries shape, never content.
	Workload ModelWorkload `json:"workload"`
	// Revision is the world revision this context was built from, so an
	// explanation can be attributed to an exact observed state.
	Revision uint64 `json:"revision"`
	// Nodes and Allocations are the facts a diagnosis reasons over.
	Nodes       []ModelNode       `json:"nodes,omitempty"`
	Allocations []ModelAllocation `json:"allocations,omitempty"`
	// Events is the recent history, already reduced to kinds and messages.
	Events []ModelEvent `json:"events,omitempty"`
	// Truncated reports that history was longer than the limit, so a model is
	// told it is seeing a window rather than everything.
	Truncated bool `json:"truncated,omitempty"`
}

// ModelWorkload is a workload's shape without its content.
type ModelWorkload struct {
	Name     string `json:"name"`
	Replicas int    `json:"replicas"`
	// Image is reported as a digest presence rather than the digest itself. The
	// exact digest tells a diagnosis nothing its presence does not.
	ImagePinned bool   `json:"image_pinned"`
	Stateful    bool   `json:"stateful,omitempty"`
	Engine      string `json:"engine,omitempty"`
	// AgentRuntime names the runtime contract when this is an agent workload.
	AgentRuntime string `json:"agent_runtime,omitempty"`
	// SecretNames lists which secrets are declared. Names are opaque handles
	// already constrained by validation; versions and material are excluded
	// because neither helps explain a failure.
	SecretNames []string `json:"secret_names,omitempty"`
	// VolumeNames lists declared storage by name only.
	VolumeNames []string `json:"volume_names,omitempty"`
}

// ModelNode is a node's schedulable facts.
type ModelNode struct {
	ID      string `json:"id"`
	Healthy bool   `json:"healthy"`
	// FreeCPUMillis and FreeMemoryMB are what remains, which is what a placement
	// failure is actually about.
	FreeCPUMillis int `json:"free_cpu_millis"`
	FreeMemoryMB  int `json:"free_memory_mb"`
	// Labels are operator-assigned and appear in constraints, so a diagnosis
	// needs them to explain a placement refusal.
	Labels map[string]string `json:"labels,omitempty"`
	// ReachableProviders lists model providers this node can currently reach.
	ReachableProviders []string `json:"reachable_providers,omitempty"`
}

// ModelAllocation is one instance's observable state.
type ModelAllocation struct {
	ID       string `json:"id"`
	Node     string `json:"node"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Restarts int    `json:"restarts"`
	ExitCode int    `json:"exit_code,omitempty"`
	Draining bool   `json:"draining,omitempty"`
	// Exhausted reports an agent that spent its ceiling, without reporting how
	// much it spent. The fact explains the failure; the amount does not.
	Exhausted bool `json:"exhausted,omitempty"`
}

// ModelEvent is one history entry reduced to what explains an outcome.
type ModelEvent struct {
	Sequence uint64 `json:"sequence"`
	Type     string `json:"type"`
	Actor    string `json:"actor"`
	Target   string `json:"target,omitempty"`
	Kind     string `json:"kind,omitempty"`
	// Message is controller-authored text. It never contains workload output,
	// because no code path writes workload output into an event message.
	Message string `json:"message,omitempty"`
}

// MaxModelEvents bounds how much history reaches a model.
//
// The limit is a cost and safety control at once: an unbounded context grows
// with cluster age, and a diagnosis that needs a thousand events is not a
// diagnosis. Recent history is kept, because the proximate cause is near the
// end.
const MaxModelEvents = 60

// maxModelMessage bounds one message. Controller messages are short; anything
// longer is a bug or an attempt to pad context.
const maxModelMessage = 512

// BuildModelContext reduces a goal, world, and history to what a model may see.
//
// This is the only supported way to produce model input. A caller assembling
// its own context from World directly would bypass every exclusion here.
func BuildModelContext(goal Goal, world World, events []Event) ModelContext {
	context := ModelContext{
		GoalID:    goal.ID,
		Objective: truncateModelText(goal.Objective),
		Revision:  world.Revision,
		Workload: ModelWorkload{
			Name:        goal.Workload.Name,
			Replicas:    goal.Workload.Replicas,
			ImagePinned: strings.Contains(goal.Workload.Image, "@sha256:"),
			Stateful:    goal.Workload.Stateful,
			Engine:      goal.Workload.Engine,
		},
	}
	if runtime := goal.Workload.Runtime; runtime != nil {
		context.Workload.AgentRuntime = runtime.Name
	}
	for _, ref := range goal.Workload.Secrets {
		context.Workload.SecretNames = append(context.Workload.SecretNames, ref.Name)
	}
	for _, ref := range goal.Workload.Volumes {
		context.Workload.VolumeNames = append(context.Workload.VolumeNames, ref.Name)
	}

	now := world.Now()
	nodeIDs := make([]string, 0, len(world.Nodes))
	for id := range world.Nodes {
		nodeIDs = append(nodeIDs, id)
	}
	// Stable order keeps a context deterministic, so the same state produces the
	// same request and a cached or replayed answer stays meaningful.
	sort.Strings(nodeIDs)
	for _, id := range nodeIDs {
		node := world.Nodes[id]
		entry := ModelNode{
			ID: node.ID, Healthy: node.Healthy,
			FreeCPUMillis: node.Capacity.CPUMillis - node.Used.CPUMillis,
			FreeMemoryMB:  node.Capacity.MemoryMB - node.Used.MemoryMB,
		}
		if len(node.Labels) > 0 {
			entry.Labels = make(map[string]string, len(node.Labels))
			for key, value := range node.Labels {
				entry.Labels[key] = value
			}
		}
		for provider := range node.Providers {
			if node.CanReach(provider, now) {
				entry.ReachableProviders = append(entry.ReachableProviders, provider)
			}
		}
		sort.Strings(entry.ReachableProviders)
		context.Nodes = append(context.Nodes, entry)
	}

	allocationIDs := make([]string, 0, len(world.Allocations))
	for id := range world.Allocations {
		allocationIDs = append(allocationIDs, id)
	}
	sort.Strings(allocationIDs)
	for _, id := range allocationIDs {
		allocation := world.Allocations[id]
		if allocation.Workload != goal.Workload.Name {
			// A diagnosis is about this goal. Other workloads' instances are not
			// its business, and including them would widen exposure for nothing.
			continue
		}
		context.Allocations = append(context.Allocations, ModelAllocation{
			ID: allocation.ID, Node: allocation.Node,
			Phase: string(allocation.Phase), Ready: allocation.ReadyAt(now),
			Restarts: allocation.Restarts, ExitCode: allocation.ExitCode,
			Draining: allocation.Draining, Exhausted: allocation.Exhausted(),
		})
	}

	relevant := make([]Event, 0, len(events))
	for _, event := range events {
		if event.GoalID == goal.ID {
			relevant = append(relevant, event)
		}
	}
	if len(relevant) > MaxModelEvents {
		context.Truncated = true
		relevant = relevant[len(relevant)-MaxModelEvents:]
	}
	for _, event := range relevant {
		entry := ModelEvent{
			Sequence: event.Sequence, Type: string(event.Type), Actor: event.Actor,
			Target: event.Target, Kind: event.Kind,
			Message: truncateModelText(event.Message),
		}
		// Evidence values are deliberately not copied. Observed maps carry
		// probe details whose shape varies by kind, and an exclusion list would
		// have to be updated every time a new evidence kind is added. The kind
		// alone is what a diagnosis needs.
		context.Events = append(context.Events, entry)
	}
	return context
}

// truncateModelText bounds one string and strips control characters.
//
// Control characters in a prompt are a formatting-injection vector: an operator
// objective containing newlines and role markers could otherwise appear to a
// model as instructions rather than data.
func truncateModelText(text string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
	cleaned = strings.TrimSpace(cleaned)
	if len(cleaned) > maxModelMessage {
		return cleaned[:maxModelMessage] + "..."
	}
	return cleaned
}

// ModelProvenance records which model produced an explanation.
//
// It is required rather than optional. An explanation whose origin is unknown
// cannot be audited, and an operator reading a diagnosis needs to know whether
// a deterministic rule or a model produced it.
type ModelProvenance struct {
	// Model identifies the exact model, pinned like an image digest.
	Model string `json:"model"`
	// Template is the prompt template version, so a changed prompt is
	// distinguishable from a changed model.
	Template string `json:"template"`
	// Revision is the world revision the context was built from.
	Revision uint64 `json:"revision"`
	// Events is how many history entries were supplied.
	Events int `json:"events"`
	// Fallback reports that the model was unavailable and a deterministic
	// result was used instead.
	Fallback bool `json:"fallback,omitempty"`
	// Reason explains a fallback, for an operator asking why an explanation
	// looks thinner than usual.
	Reason string `json:"reason,omitempty"`
}

// String renders provenance for an event message.
func (p ModelProvenance) String() string {
	if p.Fallback {
		return fmt.Sprintf("deterministic fallback (model %s unavailable: %s)", p.Model, p.Reason)
	}
	return fmt.Sprintf("model %s template %s over revision %d and %d events",
		p.Model, p.Template, p.Revision, p.Events)
}
