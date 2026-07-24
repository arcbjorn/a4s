package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
	a4snode "github.com/arcbjorn/a4s/node"
	"github.com/arcbjorn/a4s/server"
)

const version = "0.2.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "a4s:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("command is required")
	}
	switch args[0] {
	case "validate":
		return validate(args[1:])
	case "simulate":
		return simulate(args[1:])
	case "node":
		return runNode(args[1:])
	case "server":
		return runServer(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// runServer starts the control plane against a durable event log and reports
// the world it recovered. Recovery is the normal startup path, so the same code
// runs whether the log is empty or holds a full history.
func runServer(args []string) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	eventLog := flags.String("event-log", "", "absolute path to the durable event log")
	file := flags.String("file", "", "optional scenario supplying node inventory and approvals")
	statusOnly := flags.Bool("status", false, "report recovered state and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *eventLog == "" {
		return fmt.Errorf("event-log is required")
	}

	base := control.World{}
	var goal *control.Goal
	if *file != "" {
		scenario, err := loadScenario(*file)
		if err != nil {
			return err
		}
		base = scenario.World
		goal = &scenario.Goal
	}

	instance, err := server.Open(server.Config{EventLog: *eventLog, Base: base},
		control.RolloutAgent{}, control.PlacementAgent{}, control.NetworkAgent{})
	if err != nil {
		return err
	}
	defer instance.Close()

	if goal != nil {
		if err := instance.Submit(*goal); err != nil {
			return err
		}
	}
	status := instance.Status()
	fmt.Printf("recovered revision %d from %d events: %d nodes, %d allocations, %d routes, %d goals\n",
		status.Revision, status.Events, status.Nodes, status.Allocations, status.Routes, status.Goals)
	if *statusOnly {
		return nil
	}
	// Reconciliation requires a connected node executor, which needs the
	// authenticated transport that is not implemented yet. Refusing here is
	// clearer than pretending to serve.
	return fmt.Errorf("no node transport is configured; run with --status until node enrollment exists")
}

func runNode(args []string) error {
	flags := flag.NewFlagSet("node", flag.ContinueOnError)
	nodeID := flags.String("node-id", "", "identity of this node")
	keyID := flags.String("key-id", "", "trusted server signing key id")
	publicKeyPath := flags.String("public-key", "", "path to base64 Ed25519 public key")
	ledgerPath := flags.String("ledger", "/var/lib/a4s/node-ledger.jsonl", "durable idempotency ledger")
	desiredPath := flags.String("desired-state", "/var/lib/a4s/desired-state.jsonl", "durable desired allocation state")
	superviseInterval := flags.Duration("supervise-interval", 10*time.Second, "local reconciliation interval (0 disables)")
	containerdAddress := flags.String("containerd", "/run/containerd/containerd.sock", "containerd socket")
	containerdNamespace := flags.String("namespace", "a4s", "containerd namespace")
	snapshotter := flags.String("snapshotter", "", "containerd snapshotter override")
	logDir := flags.String("log-dir", "/var/log/a4s/allocations", "allocation log directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *nodeID == "" || *keyID == "" || *publicKeyPath == "" {
		return fmt.Errorf("node-id, key-id, and public-key are required")
	}
	publicKey, err := loadPublicKey(*publicKeyPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	runtime, err := a4snode.OpenContainerd(ctx, a4snode.ContainerdConfig{
		Address:     *containerdAddress,
		Namespace:   *containerdNamespace,
		Snapshotter: *snapshotter,
		LogDir:      *logDir,
	})
	if err != nil {
		return err
	}
	defer runtime.Close()
	ledger, err := a4snode.OpenFileLedger(*ledgerPath)
	if err != nil {
		return err
	}
	defer ledger.Close()
	desired, err := a4snode.OpenDesiredState(*desiredPath)
	if err != nil {
		return err
	}
	dispatcher := a4snode.Dispatcher{
		NodeID:  *nodeID,
		Keys:    map[string]ed25519.PublicKey{*keyID: publicKey},
		Runtime: runtime,
		Ledger:  ledger,
		Desired: desired,
		Now:     time.Now,
	}

	// Supervision runs alongside the action stream so a crashed workload is
	// restarted even while the server is unreachable.
	supervisor := a4snode.NewSupervisor(runtime, desired)
	supervisorCtx, stopSupervisor := context.WithCancel(ctx)
	defer stopSupervisor()
	go superviseLoop(supervisorCtx, supervisor, *superviseInterval)

	return a4snode.Serve(ctx, &dispatcher, os.Stdin, os.Stdout)
}

// superviseLoop periodically reconciles observed local state toward the node's
// last server-authorized desired state.
func superviseLoop(ctx context.Context, supervisor *a4snode.Supervisor, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			observations, err := supervisor.Reconcile(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "a4s: supervise:", err)
				continue
			}
			for _, evidence := range observations {
				fmt.Fprintf(os.Stderr, "a4s: supervise %s %s %v\n", evidence.Kind, evidence.Target, evidence.Observed)
			}
		}
	}
}

func loadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	encoded := strings.TrimSpace(string(raw))
	decoded, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(encoded)
	}
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be a base64-encoded Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}

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

func usage() {
	fmt.Fprintln(os.Stderr, `a4s - agentic infrastructure control-plane spike

Usage:
  a4s validate --file scenario.json
  a4s simulate --file scenario.json [--json] [--event-log /path] [--max-rounds N]
  a4s node --node-id ID --key-id ID --public-key /path [runtime flags]
  a4s server --event-log /path [--file scenario.json] [--status]
  a4s version`)
}
