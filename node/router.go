package node

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/arcbjorn/a4s/control"
)

// RouteBackend applies an atomic route snapshot to a gateway. The real
// implementation writes a gateway configuration and reloads it; the control
// plane never speaks the gateway's own configuration language.
type RouteBackend interface {
	Apply(context.Context, []control.RouteSnapshot) error
}

// Router handles route actions on the node. It is separate from the container
// runtime because publishing a route and starting a container are different
// capabilities that should not share an implementation or a blast radius.
//
// The gateway consumes a whole snapshot rather than incremental edits, so a
// partially applied change cannot leave routing in a state no one authorized.
type Router struct {
	mu      sync.Mutex
	backend RouteBackend
	routes  map[string]control.Route
	// Endpoints resolves a workload to the instances currently observed
	// serving it. Without it a route is published with no upstream, which the
	// gateway answers honestly rather than silently dropping.
	Endpoints func(workload string) []control.Endpoint
}

func NewRouter(backend RouteBackend) *Router {
	return &Router{backend: backend, routes: make(map[string]control.Route)}
}

func (r *Router) Execute(ctx context.Context, action control.Action) (control.Evidence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch action.Kind {
	case control.ActionPublishRoute:
		if action.Target == "" || action.Workload == "" {
			return control.Evidence{}, fmt.Errorf("publish route requires target and workload")
		}
		previous, existed := r.routes[action.Target]
		r.routes[action.Target] = control.Route{
			Host: action.Target, Workload: action.Workload,
			Port: action.Port, Exposure: action.Exposure,
		}
		if err := r.apply(ctx); err != nil {
			// Restore the prior snapshot so a failed apply does not leave the
			// node believing in a route the gateway never accepted.
			if existed {
				r.routes[action.Target] = previous
			} else {
				delete(r.routes, action.Target)
			}
			return control.Evidence{}, fmt.Errorf("publish route %q: %w", action.Target, err)
		}
		return control.Evidence{
			Kind: control.EvidenceRouteReachable, Target: action.Target,
			Observed: map[string]string{
				"workload": action.Workload,
				"port":     fmt.Sprint(action.Port),
				"exposure": action.Exposure,
			},
		}, nil

	default:
		return control.Evidence{}, fmt.Errorf("router does not support action kind %q", action.Kind)
	}
}

func (r *Router) apply(ctx context.Context) error {
	if r.backend == nil {
		return nil
	}
	hosts := make([]string, 0, len(r.routes))
	for host := range r.routes {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	snapshot := make([]control.RouteSnapshot, 0, len(hosts))
	for _, host := range hosts {
		route := r.routes[host]
		entry := control.RouteSnapshot{
			Host: route.Host, Workload: route.Workload,
			Port: route.Port, Exposure: route.Exposure,
		}
		if r.Endpoints != nil {
			entry.Endpoints = r.Endpoints(route.Workload)
		}
		snapshot = append(snapshot, entry)
	}
	return r.backend.Apply(ctx, snapshot)
}

func (r *Router) Close() error { return nil }

// CompositeRuntime dispatches each action to the capability that owns it. The
// node exposes several narrow capabilities rather than one broad executor, so
// an action can only ever reach the subsystem authorized to perform it.
type CompositeRuntime struct {
	Containers *ContainerRuntime
	Routes     *Router
	Networks   *Network
}

func (c *CompositeRuntime) Execute(ctx context.Context, action control.Action) (control.Evidence, error) {
	switch action.Kind {
	case control.ActionPublishRoute:
		if c.Routes == nil {
			return control.Evidence{}, fmt.Errorf("node has no routing capability")
		}
		return c.Routes.Execute(ctx, action)
	case control.ActionAttachNetwork:
		if c.Networks == nil {
			return control.Evidence{}, fmt.Errorf("node has no network capability")
		}
		return c.Networks.Execute(ctx, action)
	case control.ActionDeleteAllocation:
		if c.Containers == nil {
			return control.Evidence{}, fmt.Errorf("node has no container capability")
		}
		evidence, err := c.Containers.Execute(ctx, action)
		if err != nil {
			return control.Evidence{}, err
		}
		// A deleted allocation must never leave a namespace or address behind,
		// so teardown is part of delete rather than a separately proposed step
		// an agent could forget.
		if c.Networks != nil {
			if _, detachErr := c.Networks.Detach(ctx, action.Target); detachErr != nil {
				return control.Evidence{}, detachErr
			}
		}
		return evidence, nil
	default:
		if c.Containers == nil {
			return control.Evidence{}, fmt.Errorf("node has no container capability")
		}
		return c.Containers.Execute(ctx, action)
	}
}

func (c *CompositeRuntime) Close() error {
	var err error
	if c.Containers != nil {
		err = c.Containers.Close()
	}
	if c.Routes != nil {
		if closeErr := c.Routes.Close(); err == nil {
			err = closeErr
		}
	}
	if c.Networks != nil {
		if closeErr := c.Networks.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}
