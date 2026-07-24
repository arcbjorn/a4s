package node

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/arcbjorn/a4s/control"
)

// NetworkAttachment is the observed result of giving an allocation its own
// network namespace and address.
type NetworkAttachment struct {
	// Address is the allocation's own IP. Every allocation gets one, which is
	// what lets replicas of a workload share a node without contending for a
	// host port.
	Address string
	// Namespace is the network namespace path the runtime must join.
	Namespace string
	// Interface is the name of the interface inside the namespace.
	Interface string
	// AlreadyAttached reports an idempotent repeat rather than fresh work.
	AlreadyAttached bool
}

// NetworkBackend is the narrow contract between the node and a network
// implementation. It is deliberately smaller than the CNI spec: the control
// plane never speaks CNI directly, and a backend cannot be asked to do anything
// beyond attaching, detaching, and checking one allocation.
type NetworkBackend interface {
	// Attach creates the allocation's namespace and address. It must be safe to
	// repeat: a replayed attach returns the existing attachment.
	Attach(context.Context, NetworkRequest) (NetworkAttachment, error)
	// Detach releases the namespace and address. It must succeed when the
	// allocation is already gone so a replayed teardown stays idempotent.
	Detach(context.Context, string) (bool, error)
	// Check verifies the attachment still exists as described. CNI supports
	// this explicitly, and it is how the node detects a namespace that was
	// torn down underneath it.
	Check(context.Context, string) (NetworkAttachment, error)
	Close() error
}

// NetworkRequest describes one allocation's networking needs.
type NetworkRequest struct {
	Allocation string
	Workload   string
	// Port is the port the workload listens on inside its namespace.
	Port int
	// ContainerID ties the attachment to the container, which CNI requires so
	// a stale namespace can be attributed and cleaned up.
	ContainerID string
}

// Network handles network actions on the node. It is a separate capability from
// the container runtime because attaching a namespace and starting a process
// are different authorities that should not share a blast radius.
type Network struct {
	backend NetworkBackend
}

func NewNetwork(backend NetworkBackend) *Network {
	return &Network{backend: backend}
}

func (n *Network) Execute(ctx context.Context, action control.Action) (control.Evidence, error) {
	if n == nil || n.backend == nil {
		return control.Evidence{}, fmt.Errorf("node has no network capability")
	}
	switch action.Kind {
	case control.ActionAttachNetwork:
		if action.Target == "" || action.Workload == "" {
			return control.Evidence{}, fmt.Errorf("attach network requires target and workload")
		}
		attachment, err := n.backend.Attach(ctx, NetworkRequest{
			Allocation: action.Target, Workload: action.Workload,
			Port: action.Port, ContainerID: action.Target,
		})
		if err != nil {
			return control.Evidence{}, fmt.Errorf("attach network for %q: %w", action.Target, err)
		}
		if attachment.Address == "" {
			return control.Evidence{}, fmt.Errorf("network backend returned no address for %q", action.Target)
		}
		if net.ParseIP(attachment.Address) == nil {
			return control.Evidence{}, fmt.Errorf("network backend returned invalid address %q for %q",
				attachment.Address, action.Target)
		}
		return control.Evidence{
			Kind: control.EvidenceNetworkAttached, Target: action.Target,
			Observed: map[string]string{
				"address":          attachment.Address,
				"namespace":        attachment.Namespace,
				"interface":        attachment.Interface,
				"already_attached": fmt.Sprintf("%t", attachment.AlreadyAttached),
			},
		}, nil

	default:
		return control.Evidence{}, fmt.Errorf("network does not support action kind %q", action.Kind)
	}
}

// Detach releases an allocation's network. It is called during delete rather
// than proposed as its own action, because a deleted allocation must never
// leave a namespace behind.
func (n *Network) Detach(ctx context.Context, allocation string) (control.Evidence, error) {
	if n == nil || n.backend == nil {
		return control.Evidence{}, nil
	}
	released, err := n.backend.Detach(ctx, allocation)
	if err != nil {
		return control.Evidence{}, fmt.Errorf("detach network for %q: %w", allocation, err)
	}
	return control.Evidence{
		Kind: control.EvidenceNetworkDetached, Target: allocation,
		Observed: map[string]string{"released": fmt.Sprintf("%t", released)},
	}, nil
}

// Attachment reports the current attachment for an allocation, which the probe
// path uses to find where a workload actually listens.
func (n *Network) Attachment(ctx context.Context, allocation string) (NetworkAttachment, error) {
	if n == nil || n.backend == nil {
		return NetworkAttachment{}, fmt.Errorf("node has no network capability")
	}
	return n.backend.Check(ctx, allocation)
}

func (n *Network) Close() error {
	if n == nil || n.backend == nil {
		return nil
	}
	return n.backend.Close()
}

// BridgeIPAM assigns addresses from a node-local subnet.
//
// Each node owns its own allocation subnet and hands out addresses without
// coordinating with any other node. That is deliberate: cross-node traffic goes
// through service gateways rather than depending on cluster-wide pod-IP
// transparency, so address assignment never needs consensus.
type BridgeIPAM struct {
	mu       sync.Mutex
	subnet   *net.IPNet
	next     uint32
	assigned map[string]string
	released map[string]bool
}

// NewBridgeIPAM creates an allocator over a node-local CIDR.
func NewBridgeIPAM(cidr string) (*BridgeIPAM, error) {
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse allocation subnet: %w", err)
	}
	if subnet.IP.To4() == nil {
		return nil, fmt.Errorf("allocation subnet must be IPv4")
	}
	return &BridgeIPAM{
		subnet: subnet, next: 2, // .1 is the bridge itself.
		assigned: make(map[string]string), released: make(map[string]bool),
	}, nil
}

// Assign returns the allocation's address, creating one if it has none.
// Assignment is idempotent so a replayed attach yields the same address.
func (a *BridgeIPAM) Assign(allocation string) (string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if address, ok := a.assigned[allocation]; ok {
		return address, true, nil
	}
	base := binaryIPv4(a.subnet.IP)
	ones, bits := a.subnet.Mask.Size()
	capacity := uint32(1) << uint(bits-ones)
	for offset := a.next; offset < capacity-1; offset++ {
		candidate := ipv4From(base + offset)
		if a.inUse(candidate) {
			continue
		}
		a.assigned[allocation] = candidate
		a.next = offset + 1
		return candidate, false, nil
	}
	return "", false, fmt.Errorf("allocation subnet %s is exhausted", a.subnet)
}

func (a *BridgeIPAM) inUse(address string) bool {
	for _, existing := range a.assigned {
		if existing == address {
			return true
		}
	}
	return false
}

// Release returns an address to the pool.
func (a *BridgeIPAM) Release(allocation string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.assigned[allocation]; !ok {
		return false
	}
	delete(a.assigned, allocation)
	return true
}

// Address reports the current assignment without creating one.
func (a *BridgeIPAM) Address(allocation string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	address, ok := a.assigned[allocation]
	return address, ok
}

func binaryIPv4(ip net.IP) uint32 {
	v4 := ip.To4()
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
}

func ipv4From(value uint32) string {
	return net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value)).String()
}

// MemoryNetwork is an in-process network backend for tests and for the
// non-Linux build. It assigns real distinct addresses from a node-local subnet,
// so replica isolation is exercised without requiring namespaces.
type MemoryNetwork struct {
	mu          sync.Mutex
	ipam        *BridgeIPAM
	attachments map[string]NetworkAttachment
	// FailAttach makes the next attach fail, for testing the failure path.
	FailAttach error
}

func NewMemoryNetwork(cidr string) (*MemoryNetwork, error) {
	ipam, err := NewBridgeIPAM(cidr)
	if err != nil {
		return nil, err
	}
	return &MemoryNetwork{ipam: ipam, attachments: make(map[string]NetworkAttachment)}, nil
}

func (m *MemoryNetwork) Attach(_ context.Context, request NetworkRequest) (NetworkAttachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailAttach != nil {
		return NetworkAttachment{}, m.FailAttach
	}
	if existing, ok := m.attachments[request.Allocation]; ok {
		existing.AlreadyAttached = true
		return existing, nil
	}
	address, _, err := m.ipam.Assign(request.Allocation)
	if err != nil {
		return NetworkAttachment{}, err
	}
	attachment := NetworkAttachment{
		Address:   address,
		Namespace: "/var/run/a4s/netns/" + request.Allocation,
		Interface: "eth0",
	}
	m.attachments[request.Allocation] = attachment
	return attachment, nil
}

func (m *MemoryNetwork) Detach(_ context.Context, allocation string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.attachments[allocation]; !ok {
		return false, nil
	}
	delete(m.attachments, allocation)
	m.ipam.Release(allocation)
	return true, nil
}

func (m *MemoryNetwork) Check(_ context.Context, allocation string) (NetworkAttachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	attachment, ok := m.attachments[allocation]
	if !ok {
		return NetworkAttachment{}, fmt.Errorf("allocation %q has no network attachment", allocation)
	}
	attachment.AlreadyAttached = true
	return attachment, nil
}

func (m *MemoryNetwork) Close() error { return nil }
