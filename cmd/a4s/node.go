package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"time"

	"github.com/arcbjorn/a4s/control"
	a4snode "github.com/arcbjorn/a4s/node"
	"github.com/arcbjorn/a4s/obs"
)

func (c *cli) runNode(args []string) error {
	flags := c.flagSet("node")
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
		// Readiness is measured here because here is where the workload runs.
		// The observer routes each probe kind to the capability that owns it,
		// so a database is asked whether it accepts connections rather than
		// whether its port is open.
		Observer: &a4snode.CompositeObserver{
			Runtime: a4snode.NewRuntimeObserver(runtime),
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
		return a4snode.Serve(ctx, &dispatcher, c.stdin, c.stdout)
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
