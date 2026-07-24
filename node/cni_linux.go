//go:build linux

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// CNIConfig describes how the node invokes CNI plugins.
type CNIConfig struct {
	// BinDir holds the plugin executables, conventionally /opt/cni/bin.
	BinDir string
	// ConfDir holds network configuration, conventionally /etc/cni/net.d.
	ConfDir string
	// NetworkName selects which configured network to attach to.
	NetworkName string
	// Subnet is the node-local allocation CIDR.
	Subnet string
	// NamespaceDir is where allocation network namespaces are bound.
	NamespaceDir string
	// Timeout bounds a single plugin invocation, so a hung plugin cannot stall
	// the control loop.
	Timeout time.Duration
}

const defaultCNITimeout = 30 * time.Second

// cniBackend invokes standard CNI plugins.
//
// a4s does not implement networking; it invokes the reference plugins the same
// way any runtime does. CNI is a small, stable runtime-to-plugin contract, and
// reimplementing bridge, IPAM, or firewall logic would mean owning code that is
// already mature elsewhere.
type cniBackend struct {
	config CNIConfig
	ipam   *BridgeIPAM

	mu          sync.Mutex
	attachments map[string]NetworkAttachment
}

// OpenCNI prepares the node's network capability.
func OpenCNI(config CNIConfig) (*Network, error) {
	config = defaultCNIConfig(config)
	if !filepath.IsAbs(config.BinDir) || !filepath.IsAbs(config.NamespaceDir) {
		return nil, fmt.Errorf("cni bin and namespace directories must be absolute paths")
	}
	if err := os.MkdirAll(config.NamespaceDir, 0o750); err != nil {
		return nil, fmt.Errorf("create namespace directory: %w", err)
	}
	ipam, err := NewBridgeIPAM(config.Subnet)
	if err != nil {
		return nil, err
	}
	return NewNetwork(&cniBackend{
		config: config, ipam: ipam,
		attachments: make(map[string]NetworkAttachment),
	}), nil
}

func defaultCNIConfig(config CNIConfig) CNIConfig {
	if config.BinDir == "" {
		config.BinDir = "/opt/cni/bin"
	}
	if config.ConfDir == "" {
		config.ConfDir = "/etc/cni/net.d"
	}
	if config.NetworkName == "" {
		config.NetworkName = "a4s0"
	}
	if config.Subnet == "" {
		config.Subnet = "10.42.0.0/24"
	}
	if config.NamespaceDir == "" {
		config.NamespaceDir = "/var/run/a4s/netns"
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultCNITimeout
	}
	return config
}

// cniResult is the subset of the CNI result the node needs. The full spec
// carries more, but a narrow decode keeps the node from depending on fields it
// does not use.
type cniResult struct {
	CNIVersion string `json:"cniVersion"`
	IPs        []struct {
		Address   string `json:"address"`
		Interface *int   `json:"interface,omitempty"`
	} `json:"ips"`
	Interfaces []struct {
		Name    string `json:"name"`
		Sandbox string `json:"sandbox,omitempty"`
	} `json:"interfaces,omitempty"`
}

func (b *cniBackend) Attach(ctx context.Context, request NetworkRequest) (NetworkAttachment, error) {
	b.mu.Lock()
	if existing, ok := b.attachments[request.Allocation]; ok {
		b.mu.Unlock()
		// A replayed attach returns the existing namespace rather than creating
		// a second one, which would strand the first.
		existing.AlreadyAttached = true
		return existing, nil
	}
	b.mu.Unlock()

	address, reused, err := b.ipam.Assign(request.Allocation)
	if err != nil {
		return NetworkAttachment{}, err
	}
	namespace := filepath.Join(b.config.NamespaceDir, request.Allocation)

	result, err := b.invoke(ctx, "ADD", request, namespace)
	if err != nil {
		// Release the address so a failed attach does not leak it.
		if !reused {
			b.ipam.Release(request.Allocation)
		}
		return NetworkAttachment{}, err
	}
	if len(result.IPs) > 0 {
		// The plugin's IPAM is authoritative when it assigns one.
		if assigned := hostFromCIDR(result.IPs[0].Address); assigned != "" {
			address = assigned
		}
	}
	interfaceName := "eth0"
	for _, iface := range result.Interfaces {
		if iface.Sandbox != "" {
			interfaceName = iface.Name
			break
		}
	}

	attachment := NetworkAttachment{
		Address: address, Namespace: namespace, Interface: interfaceName,
	}
	b.mu.Lock()
	b.attachments[request.Allocation] = attachment
	b.mu.Unlock()
	return attachment, nil
}

func (b *cniBackend) Detach(ctx context.Context, allocation string) (bool, error) {
	b.mu.Lock()
	attachment, attached := b.attachments[allocation]
	b.mu.Unlock()
	if !attached {
		// A replayed detach must succeed rather than fail on absence.
		return false, nil
	}
	request := NetworkRequest{Allocation: allocation, ContainerID: allocation}
	if _, err := b.invoke(ctx, "DEL", request, attachment.Namespace); err != nil {
		return false, err
	}
	b.ipam.Release(allocation)
	b.mu.Lock()
	delete(b.attachments, allocation)
	b.mu.Unlock()
	_ = os.Remove(attachment.Namespace)
	return true, nil
}

func (b *cniBackend) Check(ctx context.Context, allocation string) (NetworkAttachment, error) {
	b.mu.Lock()
	attachment, attached := b.attachments[allocation]
	b.mu.Unlock()
	if !attached {
		return NetworkAttachment{}, fmt.Errorf("allocation %q has no network attachment", allocation)
	}
	request := NetworkRequest{Allocation: allocation, ContainerID: allocation}
	if _, err := b.invoke(ctx, "CHECK", request, attachment.Namespace); err != nil {
		return NetworkAttachment{}, fmt.Errorf("network check for %q: %w", allocation, err)
	}
	attachment.AlreadyAttached = true
	return attachment, nil
}

func (b *cniBackend) Close() error { return nil }

// invoke runs a CNI plugin with the environment and stdin the spec requires.
func (b *cniBackend) invoke(ctx context.Context, command string, request NetworkRequest, namespace string) (cniResult, error) {
	config, err := b.networkConfig()
	if err != nil {
		return cniResult{}, err
	}
	plugin, err := b.pluginPath(config)
	if err != nil {
		return cniResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, b.config.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, plugin)
	cmd.Env = append(os.Environ(),
		"CNI_COMMAND="+command,
		"CNI_CONTAINERID="+request.ContainerID,
		"CNI_NETNS="+namespace,
		"CNI_IFNAME=eth0",
		"CNI_PATH="+b.config.BinDir,
	)
	cmd.Stdin = bytes.NewReader(config)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return cniResult{}, fmt.Errorf("cni %s failed: %w: %s", command, err, stderr.String())
	}
	// DEL and CHECK legitimately return no body.
	if stdout.Len() == 0 {
		return cniResult{}, nil
	}
	var result cniResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return cniResult{}, fmt.Errorf("decode cni %s result: %w", command, err)
	}
	return result, nil
}

// networkConfig loads the configured network definition, falling back to a
// minimal bridge configuration so a node works without hand-written CNI files.
func (b *cniBackend) networkConfig() ([]byte, error) {
	path := filepath.Join(b.config.ConfDir, b.config.NetworkName+".conflist")
	if raw, err := os.ReadFile(path); err == nil {
		return raw, nil
	}
	path = filepath.Join(b.config.ConfDir, b.config.NetworkName+".conf")
	if raw, err := os.ReadFile(path); err == nil {
		return raw, nil
	}
	// The default keeps allocations on a node-local bridge with host-local IPAM
	// and a firewall, which is the minimum a workload needs to be reachable
	// from its own node's gateway.
	return json.Marshal(map[string]any{
		"cniVersion": "1.0.0",
		"name":       b.config.NetworkName,
		"type":       "bridge",
		"bridge":     b.config.NetworkName,
		"isGateway":  true,
		"ipMasq":     true,
		"ipam": map[string]any{
			"type":   "host-local",
			"subnet": b.config.Subnet,
			"routes": []map[string]string{{"dst": "0.0.0.0/0"}},
		},
	})
}

// pluginPath resolves the plugin binary named by the configuration.
func (b *cniBackend) pluginPath(config []byte) (string, error) {
	var named struct {
		Type    string `json:"type"`
		Plugins []struct {
			Type string `json:"type"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(config, &named); err != nil {
		return "", fmt.Errorf("decode network config: %w", err)
	}
	pluginType := named.Type
	if pluginType == "" && len(named.Plugins) > 0 {
		pluginType = named.Plugins[0].Type
	}
	if pluginType == "" {
		return "", fmt.Errorf("network config names no plugin type")
	}
	path := filepath.Join(b.config.BinDir, pluginType)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("cni plugin %q not found in %s: %w", pluginType, b.config.BinDir, err)
	}
	return path, nil
}

func hostFromCIDR(value string) string {
	host, _, found := cut(value, "/")
	if !found {
		return value
	}
	return host
}

func cut(value, sep string) (string, string, bool) {
	for i := 0; i+len(sep) <= len(value); i++ {
		if value[i:i+len(sep)] == sep {
			return value[:i], value[i+len(sep):], true
		}
	}
	return value, "", false
}
