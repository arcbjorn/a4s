package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/arcbjorn/agentic-git/pkg/gitcmd"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
	a4snode "github.com/arcbjorn/a4s/node"
	"github.com/arcbjorn/a4s/obs"
	"github.com/arcbjorn/a4s/reason"
	"github.com/arcbjorn/a4s/server"
	"github.com/arcbjorn/a4s/source"
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
	case "version":
		return showVersion(args[1:])
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
		control.RolloutAgent{}, control.PlacementAgent{}, control.NetworkAgent{})
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

// seal encrypts secret material to one node's identity.
//
// Material is read from a file rather than an argument, because a command line
// is visible in shell history and process listings. It is never echoed back.
func seal(args []string) error {
	flags := flag.NewFlagSet("seal", flag.ContinueOnError)
	name := flags.String("secret", "", "secret name")
	version := flags.String("version", "", "secret version")
	nodeID := flags.String("node", "", "node this secret is sealed for")
	nodeKeyPath := flags.String("node-key", "", "path to the node's base64 Ed25519 public key")
	in := flags.String("in", "", "file holding the secret material")
	outDir := flags.String("out", "", "directory to write the sealed secret into")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" || *version == "" || *nodeID == "" || *nodeKeyPath == "" || *in == "" || *outDir == "" {
		return fmt.Errorf("secret, version, node, node-key, in, and out are required")
	}
	nodeKey, err := loadPublicKey(*nodeKeyPath)
	if err != nil {
		return err
	}
	material, err := os.ReadFile(*in)
	if err != nil {
		return fmt.Errorf("read secret material: %w", err)
	}
	if len(material) == 0 {
		return fmt.Errorf("secret material is empty")
	}
	sealed, err := a4snode.Seal(*name, *version, *nodeID, nodeKey, material)
	if err != nil {
		return err
	}
	document, err := json.Marshal(sealed)
	if err != nil {
		return fmt.Errorf("encode sealed secret: %w", err)
	}
	if err := os.MkdirAll(*outDir, 0o750); err != nil {
		return fmt.Errorf("create sealed secret directory: %w", err)
	}
	path := filepath.Join(*outDir, *name+"."+*version+".sealed")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		return fmt.Errorf("write sealed secret: %w", err)
	}
	// Report only the reference. The material is never echoed.
	fmt.Printf("sealed %s version %s for node %s: %s\n", *name, *version, *nodeID, path)
	return nil
}

// keygen writes an Ed25519 keypair to disk. Keys are written to files with
// restrictive permissions and never printed, because a private key echoed to a
// terminal ends up in scrollback and shell history.
func keygen(args []string) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := flags.String("out", "", "path for the private key; the public key gets a .pub suffix")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("out is required")
	}
	public, private, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	privateText := base64.RawStdEncoding.EncodeToString(private)
	if err := os.WriteFile(*out, []byte(privateText+"\n"), 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	publicText := base64.RawStdEncoding.EncodeToString(public)
	if err := os.WriteFile(*out+".pub", []byte(publicText+"\n"), 0o644); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	fmt.Printf("wrote %s and %s.pub\n", *out, *out)
	return nil
}

func runServer(args []string) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	eventLog := flags.String("event-log", "", "absolute path to the durable event log")
	file := flags.String("file", "", "optional scenario supplying node inventory and approvals")
	statusOnly := flags.Bool("status", false, "report recovered state and exit")
	listen := flags.String("listen", "", "address to accept enrolled node connections on")
	keyID := flags.String("key-id", "control-1", "identifier for this server signing key")
	signingKeyPath := flags.String("signing-key", "", "path to the base64 Ed25519 private signing key")
	nodeKeyDir := flags.String("node-keys", "", "directory of <node-id>.pub enrolled node keys")
	operatorKeyDir := flags.String("operator-keys", "", "directory of <key-id>.pub operator keys")
	requireEncryption := flags.Bool("require-encryption", false,
		"refuse nodes that do not negotiate an encrypted channel")
	requireAttestation := flags.Bool("require-attestation", false,
		"refuse evidence a node did not sign with its identity key")
	anchorPath := flags.String("anchor", "",
		"absolute path to the external chain-head anchor (empty disables it)")
	gitRemote := flags.String("git-remote", "",
		"repository to read goals from (empty disables the git source)")
	gitRef := flags.String("git-ref", "main", "branch or tag to track")
	gitPath := flags.String("git-path", "goals", "directory of goal documents in the repository")
	gitMirror := flags.String("git-mirror", "", "absolute path for the local bare mirror")
	gitInterval := flags.Duration("git-interval", source.DefaultPollInterval,
		"how often to poll the tracked ref")
	gitSSHKey := flags.String("git-ssh-key", "", "SSH identity for the repository")
	apiListen := flags.String("api", "", "address to serve the operator API on (empty disables it)")
	logLevel := flags.String("log-level", "info", "log verbosity: debug, info, warn, or error")
	logFormat := flags.String("log-format", "text", "log format: text or json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *eventLog == "" {
		return fmt.Errorf("event-log is required")
	}
	logger, err := obs.New(obs.Config{
		Level: *logLevel, Format: obs.Format(*logFormat), Component: "server",
	})
	if err != nil {
		return err
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

	// Operator keys are what let the server accept an approval or an API call.
	// Without them the control plane still reconciles, but no new authority can
	// enter it, which is the correct posture for a server nobody has told who
	// its operators are.
	operatorKeys := map[string]ed25519.PublicKey{}
	if *operatorKeyDir != "" {
		loaded, err := loadNodeKeys(*operatorKeyDir)
		if err != nil {
			return fmt.Errorf("load operator keys: %w", err)
		}
		operatorKeys = loaded
	}
	if *apiListen != "" && len(operatorKeys) == 0 {
		return fmt.Errorf("operator-keys is required to serve the API")
	}

	instance, openErr := server.Open(server.Config{
		EventLog: *eventLog, Base: base, OperatorKeys: operatorKeys,
		Anchor: *anchorPath,
	}, control.RolloutAgent{}, control.PlacementAgent{}, control.NetworkAgent{})
	if openErr != nil {
		return openErr
	}
	defer instance.Close()

	if goal != nil {
		if err := instance.Submit(*goal); err != nil {
			return err
		}
	}
	status := instance.Status()
	if *statusOnly {
		// --status is an operator query, so its answer goes to stdout where it
		// can be read or piped, not into the log stream.
		fmt.Printf("recovered revision %d from %d events: %d nodes, %d allocations, %d routes, %d goals\n",
			status.Revision, status.Events, status.Nodes, status.Allocations, status.Routes, status.Goals)
		return nil
	}
	logger.Info("recovered world from event log",
		slog.Uint64("revision", status.Revision),
		slog.Uint64("events", status.Events),
		slog.Int("nodes", status.Nodes),
		slog.Int("allocations", status.Allocations),
		slog.Int("routes", status.Routes),
		slog.Int("goals", status.Goals))
	if *listen == "" {
		return fmt.Errorf("listen is required to serve; use --status to inspect recovered state")
	}
	if *signingKeyPath == "" || *nodeKeyDir == "" {
		return fmt.Errorf("signing-key and node-keys are required to serve")
	}
	signingKey, err := loadPrivateKey(*signingKeyPath)
	if err != nil {
		return err
	}
	nodeKeys, err := loadNodeKeys(*nodeKeyDir)
	if err != nil {
		return err
	}

	registry := a4snode.NewRegistry()
	defer registry.CloseAll()
	listener, err := a4snode.Listen("tcp", *listen, registry, a4snode.ListenerConfig{
		NodeKeys: nodeKeys, ServerKeyID: *keyID,
		RequireEncryption: *requireEncryption,
		OnError: func(err error) {
			// A rejected enrollment is a security-relevant event, not noise:
			// it is what an unenrolled or misconfigured peer looks like.
			logger.Warn("enrollment rejected", slog.Any("error", err))
		},
	})
	if err != nil {
		return err
	}
	defer listener.Close()

	ctx, cancel := signalContext()
	defer cancel()
	go func() {
		if err := listener.Serve(ctx); err != nil {
			logger.Error("listener stopped", slog.Any("error", err))
		}
	}()
	logger.Info("accepting enrolled nodes",
		slog.String("addr", listener.Addr().String()),
		slog.String("key_id", *keyID))

	metrics := obs.NewMetrics()
	if *apiListen != "" {
		api := server.NewAPI(instance, server.APIConfig{Logger: logger, Metrics: metrics})
		httpServer := &http.Server{
			Addr:    *apiListen,
			Handler: api.Handler(),
			// Timeouts bound how long one caller can hold a connection, so a
			// slow or stalled client cannot accumulate against the control
			// plane until it runs out of file descriptors.
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go func() {
			logger.Info("serving operator API", slog.String("addr", *apiListen))
			if err := httpServer.ListenAndServe(); err != nil &&
				!errors.Is(err, http.ErrServerClosed) {
				logger.Error("operator API stopped", slog.Any("error", err))
			}
		}()
		defer func() {
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelShutdown()
			_ = httpServer.Shutdown(shutdownCtx)
		}()
	}

	// Goals may also arrive from a versioned repository. The adapter submits
	// through the same admission path as the operator API, so a repository is a
	// transport rather than a privileged channel.
	if *gitRemote != "" {
		mirror := *gitMirror
		if mirror == "" {
			return fmt.Errorf("git-mirror is required with git-remote")
		}
		git := &gitcmd.Runner{SSHKey: *gitSSHKey}
		if err := git.Available(); err != nil {
			return err
		}
		repository := source.NewRepository(
			gitcmd.NewSource(git, mirror, *gitRemote, *gitRef), *gitPath, instance)
		go watchGitSource(ctx, logger, repository, *gitInterval)
	}

	executor := a4snode.NewRegistryExecutor(registry, *keyID, signingKey)
	// Evidence is the only input that advances the world, so the executor
	// verifies each node's signature over what it reports rather than trusting
	// the authenticated channel to stand behind the claim.
	executor.NodeKeys = nodeKeys
	executor.RequireAttestation = *requireAttestation
	return reconcileLoop(ctx, logger, instance, executor, registry, metrics)
}

// watchGitSource tracks a repository, logging what each sync applied.
//
// A failed sync is logged and retried rather than fatal: an unreachable remote
// is a network problem, and a control plane that stopped tracking its repository
// over a blip would need manual intervention to resume.
func watchGitSource(ctx context.Context, logger *slog.Logger,
	repository *source.Repository, interval time.Duration) {

	_ = repository.Watch(ctx, interval, func(result source.Result, err error) {
		if err != nil {
			logger.Warn("git source sync failed", slog.Any("error", err))
			return
		}
		if !result.Changed {
			return
		}
		logger.Info("applied goals from git",
			slog.String("commit", result.Commit),
			slog.String("message", result.Message),
			slog.Int("goals", len(result.Goals)))
		for file, reason := range result.Rejected {
			// A rejected goal is the operator's problem to fix, so name the file.
			logger.Warn("git source rejected a goal",
				slog.String("file", file), slog.String("reason", reason))
		}
	})
}

// reconcileLoop drives accepted goals whenever nodes are connected. A goal that
// cannot converge is reported and retried rather than terminating the server,
// because the cause is usually a node that has not connected yet.
func reconcileLoop(ctx context.Context, logger *slog.Logger, instance *server.Server,
	executor *a4snode.RegistryExecutor, registry *a4snode.Registry,
	metrics *obs.Metrics) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return nil
		case <-ticker.C:
			metrics.SetGauge("a4s_connected_nodes", int64(len(registry.Nodes())))
			if len(registry.Nodes()) == 0 {
				continue
			}
			for _, goal := range instance.Goals() {
				metrics.Count("a4s_reconciliations_total")
				if err := instance.Reconcile(goal.ID, executor); err != nil {
					metrics.Count("a4s_reconcile_failures_total")
					logger.Warn("reconcile failed",
						slog.String("goal", goal.ID), slog.Any("error", err))
					continue
				}
				status := instance.Status()
				logger.Info("goal converged",
					slog.String("goal", goal.ID),
					slog.Uint64("revision", status.Revision),
					slog.Int("allocations", status.Allocations),
					slog.Int("routes", status.Routes))
			}
		}
	}
}

// signalContext cancels on interrupt or SIGTERM so a daemon shuts down cleanly
// rather than leaving node connections half-open or a dispatch half-applied.
// Both daemons use it, because both run under a service manager that stops them
// with SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// loadPrivateKey reads a base64 Ed25519 private key from a file. Keys are never
// accepted on the command line, where they would appear in shell history and
// process listings.
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	decoded, err := decodeBase64(strings.TrimSpace(string(raw)))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key must be a base64-encoded Ed25519 private key")
	}
	return ed25519.PrivateKey(decoded), nil
}

// loadNodeKeys reads enrolled node public keys from <node-id>.pub files. A node
// absent from this directory cannot enroll.
func loadNodeKeys(dir string) (map[string]ed25519.PublicKey, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read node key directory: %w", err)
	}
	keys := make(map[string]ed25519.PublicKey)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pub") {
			continue
		}
		nodeID := strings.TrimSuffix(entry.Name(), ".pub")
		key, err := loadPublicKey(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", nodeID, err)
		}
		keys[nodeID] = key
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no enrolled node keys found in %s", dir)
	}
	return keys, nil
}

func decodeBase64(encoded string) ([]byte, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return base64.StdEncoding.DecodeString(encoded)
	}
	return decoded, nil
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
	seccomp := flags.Bool("seccomp", true, "apply the runtime's default seccomp profile")
	apparmor := flags.String("apparmor", "", "AppArmor profile to confine containers with")
	runAsUser := flags.String("run-as", "", "run containers as uid[:gid] instead of the image default")
	readOnlyRoot := flags.Bool("read-only-root", false, "mount container root filesystems read-only")
	userNamespace := flags.Bool("user-namespace", false,
		"map container uids into an unprivileged host range")
	hostUIDBase := flags.Uint("host-uid-base", 100000, "first host uid of the mapped range")
	uidMapSize := flags.Uint("uid-map-size", 65536, "size of the mapped uid range")
	cniBinDir := flags.String("cni-bin", "/opt/cni/bin", "directory holding CNI plugin binaries")
	cniConfDir := flags.String("cni-conf", "/etc/cni/net.d", "directory holding CNI network configuration")
	cniNetwork := flags.String("cni-network", "a4s0", "CNI network name")
	allocationSubnet := flags.String("subnet", "10.42.0.0/24", "node-local allocation subnet")
	netnsDir := flags.String("netns-dir", "/var/run/a4s/netns", "allocation network namespace directory")
	volumeRoot := flags.String("volume-root", "/var/lib/a4s/volumes", "directory holding durable volumes")
	volumeState := flags.String("volume-state", "/var/lib/a4s/volume-state.jsonl", "durable volume ownership state")
	backupRoot := flags.String("backup-root", "", "off-host path for snapshot backups (empty disables backup)")
	secretDir := flags.String("secret-dir", "/var/lib/a4s/secrets", "directory of sealed secrets for this node")
	secretRoot := flags.String("secret-root", "/run/a4s/secrets", "tmpfs directory for decrypted material")
	gatewayAdmin := flags.String("gateway-admin", "", "Caddy admin API address (empty disables the gateway)")
	gatewayConfig := flags.String("gateway-config", "/var/lib/a4s/gateway.json", "path for the applied gateway config")
	acmeEmail := flags.String("acme-email", "", "contact address for certificate issuance")
	tlsInternal := flags.Bool("tls-internal", false, "issue internal certificates instead of using ACME")
	serverAddress := flags.String("server", "", "server address to connect to (empty reads stdin)")
	identityKeyPath := flags.String("identity-key", "", "path to this node's base64 Ed25519 private key")
	keysetPath := flags.String("keyset", "", "controller keyset to trust (supersedes --public-key)")
	dnsListen := flags.String("dns", "", "address to answer a4s service names on (empty disables it)")
	nftCommand := flags.String("nft", "", "nft binary for network policy (empty disables enforcement)")
	nftDryRun := flags.Bool("nft-dry-run", false, "compile policy without installing it")
	logLevel := flags.String("log-level", "info", "log verbosity: debug, info, warn, or error")
	logFormat := flags.String("log-format", "text", "log format: text or json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *nodeID == "" {
		return fmt.Errorf("node-id is required")
	}
	if *keysetPath == "" && (*keyID == "" || *publicKeyPath == "") {
		return fmt.Errorf("either keyset, or key-id and public-key, are required")
	}
	logger, err := obs.New(obs.Config{
		Level: *logLevel, Format: obs.Format(*logFormat), Component: "node",
	})
	if err != nil {
		return err
	}
	logger = logger.With(slog.String("node_id", *nodeID))
	// With a keyset the single public key is unused, so requiring it would make
	// the rotation-capable path harder to run than the one it replaces.
	var publicKey ed25519.PublicKey
	if *publicKeyPath != "" {
		loaded, err := loadPublicKey(*publicKeyPath)
		if err != nil {
			return err
		}
		publicKey = loaded
	}

	// The node runs as a supervised service, so SIGTERM must unwind the dispatch
	// loop and supervisor rather than killing the process mid-action. Workloads
	// keep running either way: stopping the node does not stop containerd.
	ctx, cancel := signalContext()
	defer cancel()

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

	// Confinement is host configuration, not workload configuration: an
	// authorized action cannot ask to be sandboxed less than this.
	runtime.Sandbox = a4snode.SandboxProfile{
		Seccomp:       *seccomp,
		AppArmor:      *apparmor,
		User:          *runAsUser,
		ReadOnlyRoot:  *readOnlyRoot,
		UserNamespace: *userNamespace,
		HostUIDBase:   uint32(*hostUIDBase),
		UIDMapSize:    uint32(*uidMapSize),
	}

	// Each allocation gets its own namespace and address, so replicas of one
	// workload can share this node without contending for a host port.
	network, err := a4snode.OpenCNI(a4snode.CNIConfig{
		BinDir: *cniBinDir, ConfDir: *cniConfDir, NetworkName: *cniNetwork,
		Subnet: *allocationSubnet, NamespaceDir: *netnsDir,
	})
	if err != nil {
		return err
	}
	defer network.Close()
	runtime.Namespaces = func(allocation string) string {
		attachment, err := network.Attachment(ctx, allocation)
		if err != nil {
			return ""
		}
		return attachment.Namespace
	}

	ledger, err := a4snode.OpenFileLedger(*ledgerPath)
	if err != nil {
		return err
	}
	defer ledger.Close()
	desired, err := a4snode.OpenDesiredState(*desiredPath)
	if err != nil {
		return err
	}
	// Volume ownership is durable, so a restarted node still knows which
	// allocation holds each volume and refuses a second writer.
	volumes, err := a4snode.OpenVolumes(*volumeRoot, *volumeState)
	if err != nil {
		return err
	}
	defer volumes.Close()
	if *backupRoot != "" {
		// The store must live outside the volume root, or a host loss takes the
		// data and its backups together.
		store, err := a4snode.NewDirectoryBackupStore(*backupRoot, *volumeRoot)
		if err != nil {
			return err
		}
		volumes.WithBackupStore(store)
	}
	runtime.VolumeMountsFor = func(allocation string) []a4snode.VolumeMountSpec {
		return volumes.Mounts(allocation, volumeRefsFor(allocation, desired))
	}

	// Secrets require the node identity key, which is also what proves identity
	// during enrollment. One key per node rather than two that can drift.
	var secrets *a4snode.Secrets
	if *identityKeyPath != "" {
		identity, err := loadPrivateKey(*identityKeyPath)
		if err != nil {
			return err
		}
		broker, err := a4snode.NewFileBroker(*secretDir, *nodeID, identity)
		if err != nil {
			return err
		}
		secrets, err = a4snode.NewSecrets(broker, *secretRoot)
		if err != nil {
			return err
		}
		defer secrets.Close()
		runtime.SecretMountsFor = func(allocation string) []a4snode.SecretMountSpec {
			mounts := secrets.Mounts(allocation)
			specs := make([]a4snode.SecretMountSpec, 0, len(mounts))
			for _, mount := range mounts {
				specs = append(specs, a4snode.SecretMountSpec{
					Source: mount.Path, Destination: mount.Path,
				})
			}
			return specs
		}
	}

	// The gateway is optional: only a node that fronts public traffic needs one.
	var router *a4snode.Router
	if *gatewayAdmin != "" {
		gateway, err := a4snode.NewCaddyGateway(a4snode.CaddyConfig{
			AdminAddress: *gatewayAdmin, ConfigPath: *gatewayConfig,
			ACMEEmail: *acmeEmail, TLSInternal: *tlsInternal,
		})
		if err != nil {
			return err
		}
		router = a4snode.NewRouter(gateway)
		// Routes resolve to allocations this node has attached and that a probe
		// measured serving. An endpoint is never asserted, only observed.
		router.Endpoints = func(workload string) []control.Endpoint {
			var endpoints []control.Endpoint
			for _, entry := range desired.List() {
				if entry.Workload != workload || !entry.Running {
					continue
				}
				attachment, err := network.Attachment(ctx, entry.ID)
				if err != nil || attachment.Address == "" {
					continue
				}
				endpoints = append(endpoints, control.Endpoint{
					Allocation: entry.ID, Node: *nodeID,
					Address: attachment.Address, Port: entry.Probe.Port,
				})
			}
			return endpoints
		}
	}

	// Policy enforcement is opt-in per node. A node without it runs unfiltered
	// rather than refusing work, which keeps a host the operator deliberately
	// left open from blocking convergence.
	var firewall *a4snode.Firewall
	if *nftCommand != "" {
		firewall = a4snode.NewFirewall(a4snode.FirewallConfig{
			Command: *nftCommand, DryRun: *nftDryRun,
		})
		logger.Info("network policy enforcement enabled",
			slog.String("nft", *nftCommand), slog.Bool("dry_run", *nftDryRun))
	}

	// The resolver is what makes a service name mean the same thing from this
	// node as from any other. It serves only the a4s zone and never forwards,
	// so a name it does not know fails rather than escaping to public DNS.
	var resolver *a4snode.Resolver
	if *dnsListen != "" {
		resolver = a4snode.NewResolver(*nodeID)
		go func() {
			logger.Info("serving service names", slog.String("addr", *dnsListen))
			if err := resolver.ListenAndServe(*dnsListen); err != nil {
				logger.Error("resolver stopped", slog.Any("error", err))
			}
		}()
		defer resolver.Close()
	}

	// A keyset lets this node trust several controller keys at once, which is
	// what makes rotation possible without restarting the fleet. The single
	// --public-key form remains supported for a one-key deployment.
	trustedKeys := map[string]ed25519.PublicKey{}
	if publicKey != nil {
		trustedKeys[*keyID] = publicKey
	}
	if *keysetPath != "" {
		set, err := readKeySet(*keysetPath)
		if err != nil {
			return err
		}
		trusted, err := set.TrustMap()
		if err != nil {
			return err
		}
		trustedKeys = trusted
		logger.Info("loaded controller keyset",
			slog.String("keyset", *keysetPath), slog.Int("trusted_keys", len(trusted)))
	}

	// The identity key is loaded before the dispatcher is built, because it signs
	// the evidence this node reports as well as proving identity at enrollment.
	var identityKey ed25519.PrivateKey
	if *identityKeyPath != "" {
		identityKey, err = loadPrivateKey(*identityKeyPath)
		if err != nil {
			return err
		}
	}

	dispatcher := a4snode.Dispatcher{
		NodeID: *nodeID,
		Keys:   trustedKeys,
		Runtime: &a4snode.CompositeRuntime{
			Containers: runtime,
			Networks:   network,
			Routes:     router,
			Secrets:    secrets,
			Volumes:    volumes,
			Resolver:   resolver,
			Firewall:   firewall,
		},
		Ledger:      ledger,
		Desired:     desired,
		Now:         time.Now,
		IdentityKey: identityKey,
	}

	// Supervision runs alongside the action stream so a crashed workload is
	// restarted even while the server is unreachable.
	supervisor := a4snode.NewSupervisor(runtime, desired)
	supervisor.NodeID = *nodeID
	supervisor.IdentityKey = identityKey
	supervisorCtx, stopSupervisor := context.WithCancel(ctx)
	defer stopSupervisor()
	go superviseLoop(supervisorCtx, logger, supervisor, *superviseInterval)

	if *serverAddress == "" {
		// Without a server address the node reads a local stream, which remains
		// useful for offline testing against a real containerd.
		return a4snode.Serve(ctx, &dispatcher, os.Stdin, os.Stdout)
	}
	if identityKey == nil {
		return fmt.Errorf("identity-key is required to connect to a server")
	}
	logger.Info("connecting to server", slog.String("addr", *serverAddress))
	return a4snode.DialServer(ctx, "tcp", *serverAddress, *nodeID, identityKey, &dispatcher, 0)
}

// volumeRefsFor reports the volumes an allocation was authorized to mount,
// taken from the node's record of server intent rather than invented locally.
func volumeRefsFor(allocation string, desired *a4snode.DesiredState) []control.VolumeRef {
	entry, ok := desired.Get(allocation)
	if !ok {
		return nil
	}
	return entry.Volumes
}

// superviseLoop periodically reconciles observed local state toward the node's
// last server-authorized desired state.
func superviseLoop(ctx context.Context, logger *slog.Logger,
	supervisor *a4snode.Supervisor, interval time.Duration) {
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
				logger.Warn("supervision failed", slog.Any("error", err))
				continue
			}
			for _, evidence := range observations {
				// Local supervision acts during a control-plane outage, so this
				// is often the only record that a workload was restarted.
				logger.Info("supervised allocation",
					slog.String("kind", string(evidence.Kind)),
					slog.String("target", evidence.Target),
					slog.Any("observed", evidence.Observed))
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
              [--since 1h] [--limit N] [--json]
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

  a4s version [--json] [--short]`)
}
