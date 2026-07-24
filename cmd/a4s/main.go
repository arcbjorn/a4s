package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
	a4snode "github.com/arcbjorn/a4s/node"
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

func runNode(args []string) error {
	flags := flag.NewFlagSet("node", flag.ContinueOnError)
	nodeID := flags.String("node-id", "", "identity of this node")
	keyID := flags.String("key-id", "", "trusted server signing key id")
	publicKeyPath := flags.String("public-key", "", "path to base64 Ed25519 public key")
	ledgerPath := flags.String("ledger", "/var/lib/a4s/node-ledger.jsonl", "durable idempotency ledger")
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
	dispatcher := a4snode.Dispatcher{
		NodeID:  *nodeID,
		Keys:    map[string]ed25519.PublicKey{*keyID: publicKey},
		Runtime: runtime,
		Ledger:  ledger,
		Now:     time.Now,
	}

	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	encoder := json.NewEncoder(os.Stdout)
	for {
		var signed a4snode.SignedAction
		if err := decoder.Decode(&signed); err != nil {
			if err == io.EOF {
				return nil
			}
			// A malformed frame desynchronizes the stream, so the reader cannot
			// safely continue. Every other failure is reported per message.
			return fmt.Errorf("decode signed action: %w", err)
		}
		// A rejected or failed action must not terminate the node. The daemon
		// reports the failure and stays available for subsequent envelopes.
		result, err := dispatcher.Dispatch(ctx, signed)
		if err != nil {
			response := a4snode.DispatchResponse{
				EnvelopeID: signed.Envelope().ID,
				Error:      err.Error(),
			}
			if encodeErr := encoder.Encode(response); encodeErr != nil {
				return fmt.Errorf("encode dispatch error: %w", encodeErr)
			}
			continue
		}
		response := a4snode.DispatchResponse{
			EnvelopeID: signed.Envelope().ID,
			Result:     &result,
		}
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("encode dispatch result: %w", err)
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
  a4s version`)
}
