package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
)

func validate(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	file := flags.String("file", "scenario.json", "goal and observed world scenario")
	if err := flags.Parse(args); err != nil {
		return err
	}
	scenario, err := loadScenario(*file)
	if err != nil {
		return err
	}
	fmt.Printf("valid goal %q against %d observed nodes\n", scenario.Goal.ID, len(scenario.World.Nodes))
	return nil
}

func simulate(args []string) error {
	flags := flag.NewFlagSet("simulate", flag.ContinueOnError)
	file := flags.String("file", "scenario.json", "goal and observed world scenario")
	jsonOutput := flags.Bool("json", false, "emit JSON events")
	maxRounds := flags.Int("max-rounds", 10, "maximum reconciliation rounds")
	eventLog := flags.String("event-log", "", "absolute path for durable event records")
	if err := flags.Parse(args); err != nil {
		return err
	}
	scenario, err := loadScenario(*file)
	if err != nil {
		return err
	}
	executor := control.NewMemoryExecutor(scenario.World)
	engine := control.NewEngine(executor, control.PlacementAgent{}, control.NetworkAgent{})
	if *eventLog != "" {
		store, err := eventlog.Open(*eventLog)
		if err != nil {
			return err
		}
		defer store.Close()
		engine.WithEventSink(store)
	}
	runErr := engine.Run(scenario.Goal, *maxRounds)
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		for _, event := range engine.Events {
			if err := encoder.Encode(event); err != nil {
				return err
			}
		}
	} else {
		for _, event := range engine.Events {
			fmt.Printf("%02d  %-20s  %-16s  %s\n", event.Sequence, event.Type, event.Actor, event.Message)
		}
		world := executor.World()
		fmt.Printf("\nworld revision %d: %d allocations, %d routes\n", world.Revision, len(world.Allocations), len(world.Routes))
	}
	return runErr
}

func loadScenario(path string) (*control.Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenario: %w", err)
	}
	var scenario control.Scenario
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&scenario); err != nil {
		return nil, fmt.Errorf("decode scenario: %w", err)
	}
	if err := scenario.NormalizeAndValidate(); err != nil {
		return nil, fmt.Errorf("validate scenario: %w", err)
	}
	return &scenario, nil
}
