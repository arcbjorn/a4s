package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
	"github.com/arcbjorn/a4s/reason"
)

// explain reconstructs why a target is in its current state from durable
// history. It is read-only: it opens the log, walks the causal chain, and
// mutates nothing.
func explain(args []string) error {
	flags := flag.NewFlagSet("explain", flag.ContinueOnError)
	eventLog := flags.String("event-log", "", "absolute path to the durable event log")
	target := flags.String("target", "", "allocation id or route host to explain")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *eventLog == "" || *target == "" {
		return fmt.Errorf("event-log and target are required")
	}
	events, err := readEvents(*eventLog)
	if err != nil {
		return err
	}
	explanation := control.Explain(events, *target)
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(explanation)
	}
	fmt.Print(explanation)
	if !explanation.Found {
		return fmt.Errorf("no history for %q", *target)
	}
	return nil
}

// plan reports what reconciliation would do without touching anything.
func plan(args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	file := flags.String("file", "scenario.json", "goal and observed world scenario")
	eventLog := flags.String("event-log", "", "optional event log to rebuild the world from")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	scenario, err := loadScenario(*file)
	if err != nil {
		return err
	}

	world := scenario.World
	if *eventLog != "" {
		// Planning against recorded history rather than a declared world shows
		// what would happen to the cluster as it actually is.
		store, err := eventlog.Open(*eventLog)
		if err != nil {
			return err
		}
		defer store.Close()
		projector, err := control.NewDurableProjector(scenario.World, store)
		if err != nil {
			return err
		}
		world = projector.World()
	}

	kernel := control.Kernel{Policy: control.DefaultPolicy()}
	result := control.DryRun(kernel, world, scenario.Goal,
		control.RemediationAgent{}, control.RolloutAgent{},
		control.PlacementAgent{}, control.NetworkAgent{})
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	fmt.Print(result)
	return nil
}

// diagnose explains why a goal is not converging. The diagnoser reads history
// and writes text; it holds no capability grants and proposes no actions.
func diagnose(args []string) error {
	flags := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	eventLog := flags.String("event-log", "", "absolute path to the durable event log")
	goalID := flags.String("goal", "", "goal id to diagnose")
	file := flags.String("file", "", "optional scenario supplying node inventory")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	// Deterministic mode is an explicit escape hatch rather than the default,
	// so an operator debugging an explanation can remove the model from the
	// picture without unsetting credentials.
	deterministic := flags.Bool("deterministic", false,
		"explain without consulting a model, even if one is configured")
	model := flags.String("model", reason.DefaultModel, "model to consult")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *eventLog == "" || *goalID == "" {
		return fmt.Errorf("event-log and goal are required")
	}

	base := control.World{}
	if *file != "" {
		scenario, err := loadScenario(*file)
		if err != nil {
			return err
		}
		base = scenario.World
	}
	store, err := eventlog.Open(*eventLog)
	if err != nil {
		return err
	}
	defer store.Close()
	projector, err := control.NewDurableProjector(base, store)
	if err != nil {
		return err
	}
	events, err := store.ReplayEvents()
	if err != nil {
		return err
	}

	world := projector.World()
	if *deterministic {
		result := control.LogDiagnoser{}.Diagnose(*goalID, events, world)
		if *jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		fmt.Print(result)
		return nil
	}

	// The goal supplies the objective and workload shape a diagnosis reasons
	// over. Without a scenario there is still a useful explanation to give, so
	// an id-only goal is enough rather than an error.
	goal := control.Goal{ID: *goalID}
	if *file != "" {
		scenario, err := loadScenario(*file)
		if err != nil {
			return err
		}
		if scenario.Goal.ID == *goalID {
			goal = scenario.Goal
		}
	}

	diagnoser := reason.New(reason.NewAnthropic(), *model)
	audited := diagnoser.ExplainAudited(context.Background(), goal, world, events)

	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(audited)
	}
	fmt.Print(audited.Diagnosis)
	// Provenance is printed rather than optional: an explanation whose origin
	// is unknown cannot be audited, and a thin answer is only explicable once
	// an operator can see it came from a fallback.
	fmt.Printf("\nsource: %s\n", audited.Provenance)
	return nil
}

// readEvents opens an event log read-only and returns its recorded events.
func readEvents(path string) ([]control.Event, error) {
	store, err := eventlog.Open(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.ReplayEvents()
}
