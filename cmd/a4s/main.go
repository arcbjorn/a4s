package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// cli is the process boundary every command reads and writes through.
//
// Commands take it rather than reaching for os.Stdout directly, so a test can
// drive the real command code with buffers and assert on exactly what an
// operator would see. Results go to stdout, where they can be piped; usage and
// diagnostics go to stderr, so they never contaminate a result.
type cli struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func main() {
	c := &cli{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	os.Exit(c.exitCode(c.run(os.Args[1:])))
}

// exitCode maps a command's outcome to a process status, reporting any error.
//
// A help request is a successful run: the flag package has already printed the
// command's usage, and exiting non-zero would make "a4s validate -h" look like
// a failure to every script and service manager that checks it.
func (c *cli) exitCode(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintln(c.stderr, "a4s:", err)
	return 1
}

// flagSet returns a flag set whose usage and parse errors go to this cli's
// stderr rather than the process's.
func (c *cli) flagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(c.stderr)
	return flags
}

func (c *cli) run(args []string) error {
	if len(args) == 0 {
		c.usage()
		return fmt.Errorf("command is required")
	}
	switch args[0] {
	case "validate":
		return c.validate(args[1:])
	case "simulate":
		return c.simulate(args[1:])
	case "node":
		return c.runNode(args[1:])
	case "server":
		return c.runServer(args[1:])
	case "keygen":
		return c.keygen(args[1:])
	case "keys":
		return c.keys(args[1:])
	case "seal":
		return c.seal(args[1:])
	case "attest":
		return c.attest(args[1:])
	case "explain":
		return c.explain(args[1:])
	case "plan":
		return c.plan(args[1:])
	case "diagnose":
		return c.diagnose(args[1:])
	case "approve":
		return c.approve(args[1:])
	case "history":
		return c.history(args[1:])
	case "backup":
		return c.backup(args[1:])
	case "restore":
		return c.restore(args[1:])
	case "submit":
		return c.submit(args[1:])
	case "status":
		return c.status(args[1:])
	case "events":
		return c.remoteEvents(args[1:])
	case "cordon":
		return c.cordon(args[1:])
	case "standby":
		return c.standby(args[1:])
	case "version":
		return c.showVersion(args[1:])
	case "help", "-h", "--help":
		c.usage()
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

func (c *cli) usage() {
	fmt.Fprintln(c.stderr, `a4s - agentic infrastructure control-plane spike

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
