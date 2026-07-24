// Package reason holds model-backed control agents.
//
// It is deliberately separate from control. The kernel and its deterministic
// agents must build and run without a model provider, so control stays
// standard-library-only and nothing in it imports this package. Dependencies
// point inward: reason depends on control, never the reverse.
package reason

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// DefaultTimeout bounds one model call.
//
// A diagnosis is an operator convenience, not a control-loop dependency. It must
// not be able to stall anything, so the timeout is short and expiry falls back
// rather than failing.
const DefaultTimeout = 20 * time.Second

// Completer is the narrow model interface this package needs.
//
// It is one method taking a prompt and returning text, so a provider SDK, an
// HTTP client, or a test double all satisfy it without this package knowing
// which. Keeping it this small is what stops provider concerns leaking into
// control-plane code.
type Completer interface {
	// Complete returns the model's raw response. Implementations must respect
	// context cancellation.
	Complete(ctx context.Context, prompt string) (string, error)
}

// Diagnoser explains a goal using a model, falling back to the deterministic
// diagnoser when the model is unavailable or unusable.
//
// The fallback is the whole reason this is safe to deploy. Every failure mode of
// the model path — provider down, timeout, malformed output, a response naming
// things that do not exist — lands on the same deterministic result the system
// would have produced without a model at all. A model can improve an
// explanation here; it can never remove one.
type Diagnoser struct {
	// Model is the provider client. A nil client means every diagnosis falls
	// back, which is the correct behavior for a node with no model configured.
	Model Completer
	// ModelID pins the exact model, recorded in provenance so an explanation is
	// attributable.
	ModelID string
	// Fallback is the deterministic diagnoser. It is required: without it there
	// is no result when the model is unavailable.
	Fallback control.Diagnoser
	// Timeout bounds one call.
	Timeout time.Duration
	// OnFallback reports why a diagnosis fell back, for operator visibility.
	// Optional.
	OnFallback func(control.ModelProvenance)
}

// New builds a model-backed diagnoser over a deterministic fallback.
func New(model Completer, modelID string) *Diagnoser {
	return &Diagnoser{
		Model: model, ModelID: modelID,
		Fallback: control.LogDiagnoser{}, Timeout: DefaultTimeout,
	}
}

// Explain produces a diagnosis and the provenance that describes where it came
// from.
//
// Provenance is returned rather than optional, because an operator reading an
// explanation needs to know whether a rule or a model wrote it. A diagnosis
// without that attribution is not auditable.
func (d *Diagnoser) Explain(ctx context.Context, goal control.Goal, world control.World,
	events []control.Event) (control.Diagnosis, control.ModelProvenance) {

	fallback := d.fallback()
	modelContext := control.BuildModelContext(goal, world, events)
	provenance := control.ModelProvenance{
		Model: d.ModelID, Template: control.ModelTemplateVersion,
		Revision: modelContext.Revision, Events: len(modelContext.Events),
	}

	diagnosis, err := d.consult(ctx, goal, world, modelContext)
	if err != nil {
		provenance.Fallback = true
		provenance.Reason = err.Error()
		if d.OnFallback != nil {
			d.OnFallback(provenance)
		}
		return fallback.Diagnose(goal.ID, events, world), provenance
	}
	return diagnosis, provenance
}

// Diagnose satisfies control.Diagnoser, so a model-backed diagnoser is
// substitutable wherever the deterministic one is used.
//
// It discards provenance, which is why Explain is the preferred entry point:
// callers that can record where an explanation came from should.
func (d *Diagnoser) Diagnose(goalID string, events []control.Event, world control.World) control.Diagnosis {
	goal := control.Goal{ID: goalID}
	diagnosis, _ := d.Explain(context.Background(), goal, world, events)
	return diagnosis
}

// consult performs the model call and decodes the result.
func (d *Diagnoser) consult(ctx context.Context, goal control.Goal, world control.World,
	modelContext control.ModelContext) (control.Diagnosis, error) {

	if d.Model == nil {
		return control.Diagnosis{}, errors.New("no model configured")
	}
	prompt, err := control.RenderModelPrompt(modelContext)
	if err != nil {
		return control.Diagnosis{}, err
	}

	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	raw, err := d.Model.Complete(ctx, prompt)
	if err != nil {
		return control.Diagnosis{}, fmt.Errorf("model call failed: %w", err)
	}
	// The decoder is where untrusted output stops being text. It refuses
	// anything malformed, oversized, or naming things the world does not
	// contain, and a refusal falls back rather than surfacing a bad explanation.
	diagnosis, err := control.DecodeModelDiagnosis(goal.ID, []byte(raw), world)
	if err != nil {
		return control.Diagnosis{}, err
	}
	if len(diagnosis.Findings) == 0 {
		// A model that found nothing has not diagnosed anything. The
		// deterministic path at least reports the recorded blockage.
		return control.Diagnosis{}, errors.New("model returned no findings")
	}
	return diagnosis, nil
}

// Audited pairs a diagnosis with the provenance that produced it.
//
// The two travel together because an explanation without its origin cannot be
// audited: an operator reading a finding needs to know whether a deterministic
// rule or a model wrote it, which model, and against which observed revision.
type Audited struct {
	Diagnosis  control.Diagnosis
	Provenance control.ModelProvenance
}

// Event renders provenance as an observation for the durable log.
//
// It records the model, template version, world revision, and how many events
// were supplied — never the context itself, and never the model's raw output.
// The audit answers "what produced this explanation", which does not require
// storing what was sent.
func (a Audited) Event(goalID string) control.Event {
	observed := map[string]string{
		"model":     a.Provenance.Model,
		"template":  a.Provenance.Template,
		"revision":  fmt.Sprint(a.Provenance.Revision),
		"events":    fmt.Sprint(a.Provenance.Events),
		"findings":  fmt.Sprint(len(a.Diagnosis.Findings)),
		"fallback":  fmt.Sprint(a.Provenance.Fallback),
		"converged": fmt.Sprint(a.Diagnosis.Converged),
	}
	if a.Provenance.Reason != "" {
		observed["reason"] = a.Provenance.Reason
	}
	return control.Event{
		Type: control.EventObservationRecorded, Actor: "diagnoser", GoalID: goalID,
		Kind: control.EvidenceDiagnosisRecorded, Message: a.Provenance.String(),
		Evidence: &control.Evidence{
			Kind: control.EvidenceDiagnosisRecorded, Target: goalID,
			Source: "diagnoser", Observed: observed,
		},
	}
}

// ExplainAudited produces a diagnosis together with its provenance event.
//
// This is the entry point a server should use: it returns the explanation and
// the record proving where it came from, so the two cannot drift apart.
func (d *Diagnoser) ExplainAudited(ctx context.Context, goal control.Goal, world control.World,
	events []control.Event) Audited {

	diagnosis, provenance := d.Explain(ctx, goal, world, events)
	return Audited{Diagnosis: diagnosis, Provenance: provenance}
}

func (d *Diagnoser) fallback() control.Diagnoser {
	if d.Fallback != nil {
		return d.Fallback
	}
	return control.LogDiagnoser{}
}
