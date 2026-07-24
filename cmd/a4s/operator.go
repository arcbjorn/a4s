package main

import (
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
)

// approve issues an operator-signed grant for one gated decision.
//
// The signing key is read from a file rather than an argument, for the same
// reason secret material is: a command line is visible in shell history and in
// process listings on a shared host.
func approve(args []string) error {
	flags := flag.NewFlagSet("approve", flag.ContinueOnError)
	eventLog := flags.String("event-log", "", "absolute path to the durable event log")
	goalID := flags.String("goal", "", "goal the approval authorizes")
	scope := flags.String("scope", "", "decision to authorize")
	issuedBy := flags.String("operator", "", "operator principal issuing the approval")
	keyPath := flags.String("key", "", "path to the operator private key")
	keyID := flags.String("key-id", "", "identifier of the signing key")
	reason := flags.String("reason", "", "why this is being approved")
	lifetime := flags.Duration("lifetime", control.DefaultApprovalLifetime,
		"how long the approval stands")
	id := flags.String("id", "", "approval id; defaults to goal-scope")
	revoke := flags.Bool("revoke", false, "withdraw a previously granted approval")
	listScopes := flags.Bool("scopes", false, "list the decisions that can be approved")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *listScopes {
		return printScopes()
	}
	if *eventLog == "" || *goalID == "" || *scope == "" {
		return fmt.Errorf("event-log, goal, and scope are required")
	}
	if *issuedBy == "" || *keyPath == "" || *keyID == "" {
		return fmt.Errorf("operator, key, and key-id are required")
	}
	if _, known := control.ApprovalScopes[*scope]; !known {
		return fmt.Errorf("scope %q is not one the kernel gates on; run --scopes to list them", *scope)
	}

	key, err := loadPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	approvalID := *id
	if approvalID == "" {
		approvalID = *goalID + "-" + *scope
	}

	store, err := eventlog.Open(*eventLog)
	if err != nil {
		return err
	}
	defer store.Close()
	projector, err := control.NewDurableProjector(control.World{}, store)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	grant := control.ApprovalGrant{
		ID: approvalID, GoalID: *goalID, Scope: *scope, IssuedBy: *issuedBy,
		IssuedAt: now, ExpiresAt: now.Add(*lifetime),
		Revision: projector.World().Revision, Reason: *reason,
	}
	signed, err := control.SignApproval(grant, *keyID, key)
	if err != nil {
		return err
	}
	// The signature is verified here even though this process just produced it.
	// Signing and accepting are separate steps everywhere else in a4s, and a
	// path that skipped verification would be the one place a malformed grant
	// could enter the log.
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("operator key is not an ed25519 key")
	}
	verified, err := control.VerifyApproval(signed,
		map[string]ed25519.PublicKey{*keyID: public}, now)
	if err != nil {
		return fmt.Errorf("refusing to record an approval that does not verify: %w", err)
	}

	if *revoke {
		if _, granted := projector.World().Approvals[verified.ID]; !granted {
			return fmt.Errorf("approval %q was never granted", verified.ID)
		}
		revocation := control.Evidence{
			Kind: control.EvidenceApprovalRevoked, Target: verified.ID,
			Source: "operator:" + verified.IssuedBy, ObservedAt: now,
			Observed: map[string]string{
				"goal": verified.GoalID, "scope": verified.Scope,
				"revoked_by": verified.IssuedBy, "reason": verified.Reason,
			},
		}
		if err := recordOperatorDecision(store, projector, verified, revocation,
			"approval revoked"); err != nil {
			return err
		}
		fmt.Printf("revoked %s (%s for %s)\n", verified.ID, verified.Scope, verified.GoalID)
		return nil
	}

	if err := recordOperatorDecision(store, projector, verified, verified.Evidence(),
		"approval granted"); err != nil {
		return err
	}
	fmt.Printf("approved %s: %s for goal %s\n", verified.ID, verified.Scope, verified.GoalID)
	fmt.Printf("issued by %s, expires %s\n",
		verified.IssuedBy, verified.ExpiresAt.Format(time.RFC3339))
	return nil
}

// recordOperatorDecision appends an operator decision to durable history and
// applies it to the projection.
//
// The log is written first. A projection updated without a durable record would
// vanish on restart, silently withdrawing an authorization the operator was
// told had been granted — the failure mode that matters most here, because the
// operator has already moved on believing the decision stands.
func recordOperatorDecision(store *eventlog.File, projector *control.DurableProjector,
	grant control.ApprovalGrant, evidence control.Evidence, message string) error {

	if err := store.Append(control.Event{
		Sequence: store.NextSequence(), At: evidence.ObservedAt,
		Type: control.EventObservationRecorded, Actor: "operator:" + grant.IssuedBy,
		GoalID: grant.GoalID, Target: grant.ID, Kind: evidence.Kind,
		Message: message + ": " + grant.Scope, Evidence: &evidence,
	}); err != nil {
		return fmt.Errorf("record operator decision: %w", err)
	}
	return projector.Project(evidence)
}

func printScopes() error {
	names := make([]string, 0, len(control.ApprovalScopes))
	for scope := range control.ApprovalScopes {
		names = append(names, scope)
	}
	sort.Strings(names)
	for _, scope := range names {
		fmt.Printf("  %-22s %s\n", scope, control.ApprovalScopes[scope])
	}
	return nil
}

// history answers what happened, narrowed to what the operator asked about.
//
// The unfiltered log stops being readable on the first busy day, and an
// operator who cannot find the relevant entries will stop consulting it, which
// defeats the point of keeping durable history at all.
func history(args []string) error {
	flags := flag.NewFlagSet("history", flag.ContinueOnError)
	eventLog := flags.String("event-log", "", "absolute path to the durable event log")
	goalID := flags.String("goal", "", "only events for this goal")
	target := flags.String("target", "", "only events naming this target")
	kind := flags.String("kind", "", "only this event type or evidence kind")
	since := flags.Duration("since", 0, "only events within this window")
	limit := flags.Int("limit", 0, "keep at most this many of the most recent events")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *eventLog == "" {
		return fmt.Errorf("event-log is required")
	}

	events, err := readEvents(*eventLog)
	if err != nil {
		return err
	}
	var window time.Time
	if *since > 0 {
		window = time.Now().Add(-*since)
	}

	var matched []control.Event
	for _, event := range events {
		if *goalID != "" && event.GoalID != *goalID {
			continue
		}
		if *target != "" && event.Target != *target {
			if event.Evidence == nil || event.Evidence.Target != *target {
				continue
			}
		}
		if *kind != "" && string(event.Type) != *kind && event.Kind != *kind {
			continue
		}
		if !window.IsZero() && event.At.Before(window) {
			continue
		}
		matched = append(matched, event)
	}
	if *limit > 0 && len(matched) > *limit {
		matched = matched[len(matched)-*limit:]
	}

	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(matched)
	}
	if len(matched) == 0 {
		fmt.Println("no matching events")
		return nil
	}
	for _, event := range matched {
		line := fmt.Sprintf("%02d  %s  %-22s %-16s %s",
			event.Sequence, event.At.UTC().Format(time.RFC3339),
			event.Type, event.Actor, event.Message)
		if event.Target != "" {
			line += fmt.Sprintf(" [%s]", event.Target)
		}
		fmt.Println(strings.TrimRight(line, " "))
	}
	return nil
}
