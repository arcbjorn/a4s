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
	Timeout   time.Duration
	Client    *http.Client
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

	default:
		return false, observed, fmt.Errorf("unsupported probe kind %q", target.Kind)
	}
}

// endpoint resolves where to reach an allocation. Until CNI assigns per
// allocation addresses, the node records the mapped host endpoint at start.
func (o *RuntimeObserver) endpoint(target control.ProbeTarget) (string, error) {
	if host, ok := o.Endpoints[target.Allocation]; ok {
		return host, nil
	}
	if target.Port <= 0 {
		return "", fmt.Errorf("probe for %q has no port", target.Allocation)
	}
	// Without an allocation network, the workload is reachable on loopback.
	// This is deliberately conservative and must change when CNI lands.
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(target.Port)), nil
}

func (o *RuntimeObserver) client() *http.Client {
	if o.Client == nil {
		return &http.Client{Timeout: DefaultProbeTimeout}
	}
	return o.Client
}
