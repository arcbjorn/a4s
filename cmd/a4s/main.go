package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
	a4snode "github.com/arcbjorn/a4s/node"
	"github.com/arcbjorn/a4s/reason"
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
	case "keygen":
		return keygen(args[1:])
	case "seal":
		return seal(args[1:])
	case "explain":
		return explain(args[1:])
	case "plan":
		return plan(args[1:])
	case "diagnose":
		return diagnose(args[1:])
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
		OnError: func(err error) { fmt.Fprintln(os.Stderr, "a4s: enrollment:", err) },
	})
	if err != nil {
		return err
	}
	defer listener.Close()

	ctx, cancel := signalContext()
	defer cancel()
	go func() {
		if err := listener.Serve(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "a4s: listener:", err)
		}
	}()
	fmt.Printf("accepting enrolled nodes on %s as key %q\n", listener.Addr(), *keyID)

	executor := a4snode.NewRegistryExecutor(registry, *keyID, signingKey)
	return reconcileLoop(ctx, instance, executor, registry)
}

// reconcileLoop drives accepted goals whenever nodes are connected. A goal that
// cannot converge is reported and retried rather than terminating the server,
// because the cause is usually a node that has not connected yet.
func reconcileLoop(ctx context.Context, instance *server.Server,
	executor *a4snode.RegistryExecutor, registry *a4snode.Registry) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("shutting down")
			return nil
		case <-ticker.C:
			if len(registry.Nodes()) == 0 {
				continue
			}
			for _, goal := range instance.Goals() {
				if err := instance.Reconcile(goal.ID, executor); err != nil {
					fmt.Fprintf(os.Stderr, "a4s: reconcile %s: %v\n", goal.ID, err)
					continue
				}
				status := instance.Status()
				fmt.Printf("goal %s converged at revision %d: %d allocations, %d routes\n",
					goal.ID, status.Revision, status.Allocations, status.Routes)
			}
		}
	}
}

// signalContext cancels on interrupt so the server shuts down cleanly rather
// than leaving node connections half-open.
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

	dispatcher := a4snode.Dispatcher{
		NodeID: *nodeID,
		Keys:   map[string]ed25519.PublicKey{*keyID: publicKey},
		Runtime: &a4snode.CompositeRuntime{
			Containers: runtime,
			Networks:   network,
			Routes:     router,
			Secrets:    secrets,
			Volumes:    volumes,
		},
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

	if *serverAddress == "" {
		// Without a server address the node reads a local stream, which remains
		// useful for offline testing against a real containerd.
		return a4snode.Serve(ctx, &dispatcher, os.Stdin, os.Stdout)
	}
	if *identityKeyPath == "" {
		return fmt.Errorf("identity-key is required to connect to a server")
	}
	identityKey, err := loadPrivateKey(*identityKeyPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "a4s: connecting to %s as node %q\n", *serverAddress, *nodeID)
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
           [--gateway-admin http://127.0.0.1:2019 --acme-email you@example.com]
  a4s server --event-log /path [--file scenario.json] [--status]
             [--listen host:port --signing-key /path --node-keys /dir]
  a4s keygen --out /path
  a4s seal --secret NAME --version V --node ID --node-key /path --in /path --out /dir
  a4s plan --file scenario.json [--event-log /path] [--json]
  a4s explain --event-log /path --target ID [--json]
  a4s diagnose --event-log /path --goal ID [--file scenario.json] [--json]
  a4s version`)
}
