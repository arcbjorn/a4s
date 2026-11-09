package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Flags a serving run needs are checked before the event log is touched.
func TestServerValidatesFlagsBeforeOpeningTheLog(t *testing.T) {
	for name, args := range map[string][]string{
		"no listen":  {"--file", example("web-service.json")},
		"no keys":    {"--listen", "127.0.0.1:0"},
		"git mirror": {"--listen", "127.0.0.1:0", "--signing-key", "k", "--node-keys", "d", "--git-remote", "r"},
	} {
		t.Run(name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "events.db")
			_, _, err := runCLI(t, append([]string{"server", "--event-log", log}, args...)...)
			if err == nil {
				t.Fatal("a serving run with missing flags started")
			}
			if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
				t.Fatalf("the event log was created before the flags were checked (%v)", err)
			}
		})
	}
}

func TestServerStatusReportsRecoveredState(t *testing.T) {
	log := recordedLog(t)
	stdout := mustRun(t, "server", "--event-log", log, "--file", example("web-service.json"), "--status")
	if !strings.HasPrefix(stdout, "recovered revision ") || !strings.Contains(stdout, "2 nodes") {
		t.Fatalf("unexpected status output %q", stdout)
	}
}

// An operator API that cannot bind must fail startup, not leave a daemon
// running without one.
func TestServerFailsWhenTheAPICannotBind(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	signingKey := generatedKey(t, "control")
	nodeKeys := t.TempDir()
	operatorKeys := t.TempDir()
	node := generatedKey(t, "node")
	operator := generatedKey(t, "operator")
	for dir, source := range map[string]string{
		filepath.Join(nodeKeys, "base.pub"):     node + ".pub",
		filepath.Join(operatorKeys, "op-1.pub"): operator + ".pub",
	} {
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, string(raw))
	}

	_, _, err = runCLI(t, "server", "--event-log", filepath.Join(t.TempDir(), "events.db"),
		"--file", example("web-service.json"), "--listen", "127.0.0.1:0",
		"--signing-key", signingKey, "--node-keys", nodeKeys,
		"--operator-keys", operatorKeys, "--api", occupied.Addr().String(), "--log-level", "error")
	if err == nil || !strings.Contains(err.Error(), "operator API") {
		t.Fatalf("want the API bind failure reported, got %v", err)
	}
}

func TestShutdownCause(t *testing.T) {
	signals, stop := context.WithCancel(context.Background())
	ctx, fail := context.WithCancelCause(signals)
	defer fail(nil)
	stop()
	if err := shutdownCause(ctx); err != nil {
		t.Fatalf("an operator interrupt was reported as %v", err)
	}

	ctx, fail = context.WithCancelCause(context.Background())
	broken := errors.New("node listener stopped")
	fail(broken)
	if err := shutdownCause(ctx); !errors.Is(err, broken) {
		t.Fatalf("a component failure was reported as %v", err)
	}
}
