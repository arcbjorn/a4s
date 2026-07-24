package control

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	digestPattern = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
)

func (s *Scenario) NormalizeAndValidate() error {
	s.World.normalize()
	if s.Goal.APIVersion != APIVersion {
		return fmt.Errorf("api_version must be %q", APIVersion)
	}
	if !namePattern.MatchString(s.Goal.ID) {
		return fmt.Errorf("goal id must be lowercase DNS-style text")
	}
	if strings.TrimSpace(s.Goal.Objective) == "" {
		return fmt.Errorf("goal objective is required")
	}
	w := s.Goal.Workload
	if !namePattern.MatchString(w.Name) {
		return fmt.Errorf("workload name must be lowercase DNS-style text")
	}
	if !digestPattern.MatchString(w.Image) {
		return fmt.Errorf("workload image must be pinned by sha256 digest")
	}
	if w.Replicas < 1 {
		return fmt.Errorf("workload replicas must be positive")
	}
	if w.Port < 1 || w.Port > 65535 {
		return fmt.Errorf("workload port must be between 1 and 65535")
	}
	if w.Resources.CPUMillis < 1 || w.Resources.MemoryMB < 1 {
		return fmt.Errorf("workload resources must be positive")
	}
	if w.Privileged {
		return fmt.Errorf("privileged workloads are outside the v1alpha1 safety policy")
	}
	if w.Stateful {
		return fmt.Errorf("stateful workloads require the future volume ownership protocol")
	}
	if s.Goal.Route != nil {
		if strings.TrimSpace(s.Goal.Route.Host) == "" {
			return fmt.Errorf("route host is required")
		}
		if s.Goal.Route.Port < 1 || s.Goal.Route.Port > 65535 {
			return fmt.Errorf("route port must be between 1 and 65535")
		}
		switch s.Goal.Route.Exposure {
		case "tailnet", "public":
		default:
			return fmt.Errorf("route exposure must be tailnet or public")
		}
	}
	if len(s.World.Nodes) == 0 {
		return fmt.Errorf("world must contain at least one node")
	}
	for id, node := range s.World.Nodes {
		if node == nil || node.ID != id || !namePattern.MatchString(id) {
			return fmt.Errorf("node map key %q must match a valid node id", id)
		}
		if node.Capacity.CPUMillis < 1 || node.Capacity.MemoryMB < 1 {
			return fmt.Errorf("node %q capacity must be positive", id)
		}
	}
	for id, approval := range s.World.Approvals {
		if approval == nil || approval.ID != id || approval.GoalID != s.Goal.ID || approval.Scope == "" || approval.IssuedBy == "" {
			return fmt.Errorf("approval %q is malformed or belongs to another goal", id)
		}
	}
	return nil
}

func (w *World) normalize() {
	if w.Nodes == nil {
		w.Nodes = make(map[string]*Node)
	}
	if w.Allocations == nil {
		w.Allocations = make(map[string]*Allocation)
	}
	if w.Routes == nil {
		w.Routes = make(map[string]*Route)
	}
	if w.Approvals == nil {
		w.Approvals = make(map[string]*Approval)
	}
	for _, node := range w.Nodes {
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		if node.Images == nil {
			node.Images = make(map[string]bool)
		}
	}
}

func hasApproval(world World, goalID, scope string) bool {
	for _, approval := range world.Approvals {
		if approval.GoalID == goalID && approval.Scope == scope && approval.Granted {
			return true
		}
	}
	return false
}
