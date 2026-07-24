package node

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// DefaultProbeTimeout bounds a single readiness measurement so a hung service
// cannot stall the control loop.
const DefaultProbeTimeout = 3 * time.Second

// CompositeObserver routes each probe kind to the capability that owns it.
//
// Readiness means something different per workload kind, and only the owning
// subsystem can establish it. A database is ready when its engine accepts a
// query; an agent when it can reach its provider with budget left. Routing here
// mirrors CompositeRuntime: the node exposes narrow capabilities rather than
// one observer that pretends to understand every workload.
type CompositeObserver struct {
	// Runtime measures process, TCP, and HTTP readiness.
	Runtime *RuntimeObserver
	// Databases measures engine readiness by connection.
	Databases *DatabaseManager
	// Agents measures provider reachability and remaining budget.
	Agents *Agents
}

func (c *CompositeObserver) ObserveReadiness(target control.ProbeTarget) (bool, map[string]string, error) {
	switch target.Kind {
	case control.ProbeDatabase:
		if c.Databases == nil {
			return false, nil, fmt.Errorf("node has no database capability for probe %q", target.Allocation)
		}
		return c.Databases.ObserveReadiness(target)
	case control.ProbeAgent:
		if c.Agents == nil {
			return false, nil, fmt.Errorf("node has no agent capability for probe %q", target.Allocation)
		}
		// Provider reach and remaining budget are necessary but not sufficient:
		// an agent whose container died satisfies both while running nothing.
		// Every other probe kind establishes liveness first, and an agent must
		// not be the exception.
		if c.Runtime != nil {
			alive, observed, err := c.Runtime.ObserveReadiness(control.ProbeTarget{
				Allocation: target.Allocation, Kind: control.ProbeProcess,
			})
			if err != nil {
				return false, observed, err
			}
			if !alive {
				return false, observed, nil
			}
		}
		return c.Agents.ObserveReadiness(target)
	default:
		if c.Runtime == nil {
			return false, nil, fmt.Errorf("node has no runtime observer for probe %q", target.Allocation)
		}
		return c.Runtime.ObserveReadiness(target)
	}
}

// RuntimeObserver measures readiness on the node where the allocation runs.
// It is the real replacement for assuming that a started container is serving.
//
// It reports readiness only from a measurement it actually performed. A failed
// measurement returns an error rather than a negative result, because "could
// not measure" and "measured as unhealthy" are different facts and only the
// latter should look like a definite answer.
type RuntimeObserver struct {
	Runtime   *ContainerRuntime
	Endpoints map[string]string
	// Network resolves an allocation's CNI address when the probe target does
	// not already carry one.
	Network *Network
	Timeout time.Duration
	Client  *http.Client
}

func NewRuntimeObserver(runtime *ContainerRuntime) *RuntimeObserver {
	timeout := DefaultProbeTimeout
	return &RuntimeObserver{
		Runtime: runtime, Endpoints: map[string]string{}, Timeout: timeout,
		Client: &http.Client{Timeout: timeout},
	}
}

func (o *RuntimeObserver) ObserveReadiness(target control.ProbeTarget) (bool, map[string]string, error) {
	if o == nil || o.Runtime == nil {
		return false, nil, fmt.Errorf("runtime observer is not initialized")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Every probe kind first requires the process to actually be running. A TCP
	// or HTTP success against a dead container would mean something else is
	// listening on that address.
	state, err := o.Runtime.Inspect(ctx, target.Allocation)
	if err != nil {
		return false, nil, fmt.Errorf("inspect %q: %w", target.Allocation, err)
	}
	observed := map[string]string{
		"exists":  strconv.FormatBool(state.Exists),
		"running": strconv.FormatBool(state.Running),
	}
	if state.Exists {
		observed["pid"] = strconv.FormatUint(uint64(state.PID), 10)
	}
	if !state.Exists || !state.Running {
		observed["reason"] = "task is not running"
		return false, observed, nil
	}

	switch target.Kind {
	case control.ProbeProcess, "":
		return true, observed, nil

	case control.ProbeTCP:
		address, err := o.endpoint(target)
		if err != nil {
			return false, observed, err
		}
		observed["address"] = address
		dialer := net.Dialer{Timeout: timeout}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			observed["reason"] = err.Error()
			return false, observed, nil
		}
		_ = conn.Close()
		return true, observed, nil

	case control.ProbeHTTP:
		address, err := o.endpoint(target)
		if err != nil {
			return false, observed, err
		}
		path := target.Path
		if path == "" {
			path = "/"
		}
		url := "http://" + address + path
		observed["url"] = url
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, observed, err
		}
		response, err := o.client().Do(request)
		if err != nil {
			observed["reason"] = err.Error()
			return false, observed, nil
		}
		defer response.Body.Close()
		observed["status"] = strconv.Itoa(response.StatusCode)
		ready := response.StatusCode >= 200 && response.StatusCode < 400
		if !ready {
			observed["reason"] = "unhealthy status"
		}
		return ready, observed, nil

	case control.ProbeDatabase, control.ProbeAgent:
		// These kinds are owned by the database and agent capabilities. Reaching
		// here means the observer was not composed, and reporting "unsupported"
		// would read as a missing feature rather than a wiring mistake.
		return false, observed, fmt.Errorf(
			"probe kind %q must be routed to its capability, not the runtime observer", target.Kind)

	default:
		return false, observed, fmt.Errorf("unsupported probe kind %q", target.Kind)
	}
}

// endpoint resolves where to reach an allocation.
//
// The allocation's own CNI address is authoritative. Probing loopback would be
// wrong in two ways: it cannot distinguish replicas sharing a node, and it can
// succeed against an unrelated process that happens to hold the port.
func (o *RuntimeObserver) endpoint(target control.ProbeTarget) (string, error) {
	if host, ok := o.Endpoints[target.Allocation]; ok {
		return host, nil
	}
	if target.Port <= 0 {
		return "", fmt.Errorf("probe for %q has no port", target.Allocation)
	}
	if target.Address != "" {
		return net.JoinHostPort(target.Address, strconv.Itoa(target.Port)), nil
	}
	if o.Network != nil {
		attachment, err := o.Network.Attachment(context.Background(), target.Allocation)
		if err == nil && attachment.Address != "" {
			return net.JoinHostPort(attachment.Address, strconv.Itoa(target.Port)), nil
		}
	}
	// Refusing is safer than guessing. A probe against the wrong address either
	// reports a dead workload healthy or a healthy one dead.
	return "", fmt.Errorf("allocation %q has no known address to probe", target.Allocation)
}

func (o *RuntimeObserver) client() *http.Client {
	if o.Client == nil {
		return &http.Client{Timeout: DefaultProbeTimeout}
	}
	return o.Client
}
