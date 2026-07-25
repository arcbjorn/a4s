package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// recordingNft writes whatever it receives on stdin to a file, so a test can
// assert exactly what would have reached the kernel.
func recordingNft(t *testing.T) (command, output string) {
	t.Helper()
	dir := t.TempDir()
	output = filepath.Join(dir, "applied.nft")
	command = filepath.Join(dir, "nft")

	script := "#!/bin/sh\ncat > " + output + "\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return command, output
}

func testRuleset(node string, rules ...string) *control.CompiledPolicy {
	return &control.CompiledPolicy{
		Node: node, Table: control.PolicyTable,
		Rules: append([]string{"add table inet a4s"}, rules...),
	}
}

// The node must feed the compiled script to nft verbatim, so what a test
// asserts is what a host receives.
func TestFirewallAppliesCompiledRuleset(t *testing.T) {
	command, output := recordingNft(t)
	firewall := NewFirewall(FirewallConfig{Command: command})

	policy := testRuleset("alpha", "add rule inet a4s input ip saddr 10.0.0.1/32 accept")
	evidence, err := firewall.Execute(context.Background(), control.Action{
		Kind: control.ActionApplyPolicy, Node: "alpha", Policy: policy,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if evidence.Kind != control.EvidencePolicyApplied {
		t.Fatalf("unexpected evidence kind %q", evidence.Kind)
	}
	if evidence.Observed["changed"] != "true" {
		t.Fatalf("first apply reported no change: %+v", evidence.Observed)
	}

	applied, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("nft received nothing: %v", err)
	}
	if string(applied) != policy.Script() {
		t.Fatalf("nft received:\n%s\nwant:\n%s", applied, policy.Script())
	}
}

// Reapplying an identical ruleset flushes the table for no reason, dropping
// in-flight connections. The fingerprint must suppress it.
func TestUnchangedPolicyIsNotReapplied(t *testing.T) {
	command, output := recordingNft(t)
	firewall := NewFirewall(FirewallConfig{Command: command})
	policy := testRuleset("alpha", "add rule inet a4s input accept")
	action := control.Action{Kind: control.ActionApplyPolicy, Node: "alpha", Policy: policy}

	if _, err := firewall.Execute(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}

	evidence, err := firewall.Execute(context.Background(), action)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if evidence.Observed["changed"] != "false" {
		t.Fatalf("an unchanged policy reported a change: %+v", evidence.Observed)
	}
	if _, err := os.Stat(output); err == nil {
		t.Fatal("nft was invoked for an unchanged ruleset")
	}
}

// A changed ruleset must actually be reinstalled.
func TestChangedPolicyIsReapplied(t *testing.T) {
	command, output := recordingNft(t)
	firewall := NewFirewall(FirewallConfig{Command: command})

	if _, err := firewall.Execute(context.Background(), control.Action{
		Kind: control.ActionApplyPolicy, Node: "alpha",
		Policy: testRuleset("alpha", "add rule inet a4s input accept"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := firewall.Execute(context.Background(), control.Action{
		Kind: control.ActionApplyPolicy, Node: "alpha",
		Policy: testRuleset("alpha", "add rule inet a4s input drop"),
	}); err != nil {
		t.Fatal(err)
	}

	applied, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(applied), "drop") {
		t.Fatalf("the changed ruleset was not applied:\n%s", applied)
	}
}

// A dry run must report what it would install without touching the host.
func TestFirewallDryRunDoesNotInvokeNft(t *testing.T) {
	command, output := recordingNft(t)
	firewall := NewFirewall(FirewallConfig{Command: command, DryRun: true})

	evidence, err := firewall.Execute(context.Background(), control.Action{
		Kind: control.ActionApplyPolicy, Node: "alpha",
		Policy: testRuleset("alpha", "add rule inet a4s input accept"),
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if evidence.Observed["dry_run"] != "true" {
		t.Fatalf("evidence does not record the dry run: %+v", evidence.Observed)
	}
	if _, err := os.Stat(output); err == nil {
		t.Fatal("a dry run invoked nft")
	}
}

// A rejected ruleset must surface nft's own error, which is the only thing
// that makes a bad rule debuggable.
func TestFirewallReportsNftFailure(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "nft")
	script := "#!/bin/sh\ncat > /dev/null\necho 'Error: syntax error, unexpected junk' >&2\nexit 1\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	firewall := NewFirewall(FirewallConfig{Command: command})
	_, err := firewall.Execute(context.Background(), control.Action{
		Kind: control.ActionApplyPolicy, Node: "alpha",
		Policy: testRuleset("alpha", "add rule inet a4s input junk"),
	})
	if err == nil {
		t.Fatal("expected a rejected ruleset to fail")
	}
	if !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("error does not carry nft's own message: %v", err)
	}
}

// A failed apply must not record the fingerprint, or the next round would
// believe the host is enforcing a ruleset it rejected.
func TestFailedApplyDoesNotRecordFingerprint(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "nft")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	firewall := NewFirewall(FirewallConfig{Command: command})
	action := control.Action{
		Kind: control.ActionApplyPolicy, Node: "alpha",
		Policy: testRuleset("alpha", "add rule inet a4s input accept"),
	}
	if _, err := firewall.Execute(context.Background(), action); err == nil {
		t.Fatal("expected the apply to fail")
	}
	if firewall.applied != "" {
		t.Fatal("a failed apply recorded its fingerprint as installed")
	}
}

func TestFirewallRefusesActionWithoutRuleset(t *testing.T) {
	firewall := NewFirewall(FirewallConfig{Command: "/bin/true"})
	if _, err := firewall.Execute(context.Background(), control.Action{
		Kind: control.ActionApplyPolicy, Node: "alpha",
	}); err == nil {
		t.Fatal("expected an action with no ruleset to be refused")
	}
}
