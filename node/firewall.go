package node

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// DefaultFirewallTimeout bounds an nft invocation so a wedged call cannot stall
// the action stream.
const DefaultFirewallTimeout = 10 * time.Second

// FirewallConfig describes how the node applies network policy.
type FirewallConfig struct {
	// Command is the nft binary. Overridable so a test can substitute a
	// recorder without a kernel.
	Command string
	Timeout time.Duration
	// DryRun compiles and records the ruleset without installing it, which
	// lets an operator inspect what would be applied on a host they are not
	// ready to change.
	DryRun bool
}

// Firewall installs compiled nftables policy on this node.
//
// It applies a complete ruleset atomically through `nft -f -`, replacing the
// a4s table wholesale. Applying rule by rule would leave the host in a
// partially-enforced state whenever a single rule failed, which for a firewall
// means either an unintended opening or an unintended outage.
type Firewall struct {
	config FirewallConfig
	mu     sync.Mutex
	// applied is the fingerprint of the ruleset currently installed, so an
	// unchanged policy is not reinstalled on every reconciliation.
	applied string
}

func NewFirewall(config FirewallConfig) *Firewall {
	if config.Command == "" {
		config.Command = "nft"
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultFirewallTimeout
	}
	return &Firewall{config: config}
}

// Execute applies a compiled policy.
func (f *Firewall) Execute(ctx context.Context, action control.Action) (control.Evidence, error) {
	if action.Kind != control.ActionApplyPolicy {
		return control.Evidence{}, fmt.Errorf("firewall does not support action kind %q", action.Kind)
	}
	if action.Policy == nil {
		return control.Evidence{}, fmt.Errorf("apply policy requires a compiled ruleset")
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	fingerprint := action.Policy.Fingerprint()
	if fingerprint == f.applied {
		// Reapplying an identical ruleset would briefly flush the table, which
		// drops in-flight connections for no benefit.
		return control.Evidence{
			Kind: control.EvidencePolicyApplied, Target: action.Node,
			Observed: map[string]string{
				"fingerprint": fingerprint,
				"rules":       fmt.Sprint(len(action.Policy.Rules)),
				"changed":     "false",
			},
		}, nil
	}

	if !f.config.DryRun {
		if err := f.apply(ctx, action.Policy.Script()); err != nil {
			return control.Evidence{}, err
		}
	}
	f.applied = fingerprint

	return control.Evidence{
		Kind: control.EvidencePolicyApplied, Target: action.Node,
		Observed: map[string]string{
			"fingerprint": fingerprint,
			"rules":       fmt.Sprint(len(action.Policy.Rules)),
			"changed":     "true",
			"dry_run":     fmt.Sprint(f.config.DryRun),
		},
	}, nil
}

// apply feeds the ruleset to nft on stdin.
func (f *Firewall) apply(ctx context.Context, script string) error {
	ctx, cancel := context.WithTimeout(ctx, f.config.Timeout)
	defer cancel()

	command := exec.CommandContext(ctx, f.config.Command, "-f", "-")
	command.Stdin = bytes.NewReader([]byte(script))
	var stderr bytes.Buffer
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		// nft reports the offending line on stderr, which is the only thing
		// that makes a rejected ruleset debuggable.
		return fmt.Errorf("apply nftables policy: %w: %s", err, stderr.String())
	}
	return nil
}

func (f *Firewall) Close() error { return nil }
