package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

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
	case "keygen":
		return keygen(args[1:])
	case "keys":
		return keys(args[1:])
	case "seal":
		return seal(args[1:])
	case "attest":
		return attest(args[1:])
	case "explain":
		return explain(args[1:])
	case "plan":
		return plan(args[1:])
	case "diagnose":
		return diagnose(args[1:])
	case "approve":
		return approve(args[1:])
	case "history":
		return history(args[1:])
	case "backup":
		return backup(args[1:])
	case "restore":
		return restore(args[1:])
	case "submit":
		return submit(args[1:])
	case "status":
		return status(args[1:])
	case "events":
		return remoteEvents(args[1:])
	case "cordon":
		return cordon(args[1:])
	case "standby":
		return standby(args[1:])
	case "version":
		return showVersion(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// signalContext cancels on interrupt or SIGTERM so a daemon shuts down cleanly
// rather than leaving node connections half-open or a dispatch half-applied.
// Both daemons use it, because both run under a service manager that stops them
// with SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func usage() {
	fmt.Fprintln(os.Stderr, `a4s - agentic infrastructure control-plane spike

Usage:
  a4s validate --file scenario.json
  a4s simulate --file scenario.json [--json] [--event-log /path] [--max-rounds N]
  a4s node --node-id ID --key-id ID --public-key /path [runtime flags]
           [--seccomp] [--apparmor PROFILE] [--run-as UID[:GID]]
           [--read-only-root] [--user-namespace]
           [--dns 127.0.0.1:53] [--keyset /path/keyset.json]
           [--nft /usr/sbin/nft] [--nft-dry-run]
           [--gateway-admin http://127.0.0.1:2019 --acme-email you@example.com]
  a4s server --event-log /path [--file scenario.json] [--status]
             [--listen host:port --signing-key /path --node-keys /dir]
             [--api host:port --operator-keys /dir] [--require-encryption]
             [--require-attestation] [--anchor /path/anchor.jsonl]
             [--git-remote URL --git-mirror /path [--git-ref main]
              [--git-path goals] [--git-interval 30s] [--git-ssh-key /path]]
             [--log-level info] [--log-format text|json]
  a4s keygen --out /path
  a4s keys init --keyset /path/keyset.json --key-id control-1 --out /path/key
  a4s keys rotate --keyset /path/keyset.json --key-id control-2 --out /path/key2
  a4s keys retire --keyset /path/keyset.json --key-id control-1
  a4s keys list --keyset /path/keyset.json [--json]
  a4s seal --secret NAME --version V --node ID --node-key /path --in /path --out /dir
  a4s attest --image REF --builder NAME --key /path --key-id ID
             [--lifetime 720h] [--out /path]
  a4s plan --file scenario.json [--event-log /path] [--json]
  a4s explain --event-log /path --target ID [--json]
  a4s diagnose --event-log /path --goal ID [--file scenario.json] [--json]
             [--deterministic] [--model ID]
  a4s approve --event-log /path --goal ID --scope SCOPE --operator NAME
              --key /path --key-id ID [--reason TEXT] [--lifetime 1h] [--revoke]
  a4s approve --event-log /path --goal ID --scope rollback --workload NAME
              --operator NAME --key /path --key-id ID [--from IMAGE] [--to IMAGE]
  a4s approve --scopes
  a4s history --event-log /path [--goal ID] [--target ID] [--kind KIND]
              [--since 1h] [--until 10m] [--limit N] [--json]
  a4s backup --event-log /path --out /path/backup.log [--json]
  a4s backup --verify /path/backup.log [--json]
  a4s restore --from /path/backup.log --event-log /path [--json]

Remote commands speak to a running server's operator API. Each request is
signed with the operator key, so authority originates with a person rather
than with possession of the network path:

  a4s submit --file scenario.json --server http://host:8443
             --key-id ID --operator-key /path [--operator NAME]
  a4s status --server http://host:8443 --key-id ID --operator-key /path [--json]
  a4s events --server http://host:8443 --key-id ID --operator-key /path
             [--goal ID] [--target ID] [--kind KIND] [--since 1h]
             [--until 10m] [--limit N]
  a4s cordon --node ID --server http://host:8443 --key-id ID
             --operator-key /path [--reason TEXT] [--undo] [--json]
  a4s standby --event-log /path --anchor /path --file scenario.json
              --server http://host:8443 --key-id ID --operator-key /path
              [--interval 5s] [--once]

  a4s version [--json] [--short]`)
}
