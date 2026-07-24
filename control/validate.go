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
	if err := validateSecrets(w.Secrets); err != nil {
		return err
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
	if w.KnownGood == nil {
		w.KnownGood = make(map[string]string)
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

// secretNamePattern keeps secret names to opaque handles. A name that looked
// like a path or carried arbitrary text would invite operators to smuggle
// material into a field that gets logged.
var secretNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)

// maxSecretVersionLength bounds a version string. A version is an identifier,
// and anything long enough to hold a key is not one.
const maxSecretVersionLength = 64

// validateSecrets enforces that only references, never material, reach a goal.
//
// The struct has no field for a value, so this guards the remaining risk: an
// operator encoding material into a name, a version, or a mount path, any of
// which are recorded in the durable log.
func validateSecrets(refs []SecretRef) error {
	seenNames := make(map[string]bool, len(refs))
	seenPaths := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if !secretNamePattern.MatchString(ref.Name) {
			return fmt.Errorf("secret name %q must be a short lowercase handle", ref.Name)
		}
		if ref.Version == "" || len(ref.Version) > maxSecretVersionLength {
			return fmt.Errorf("secret %q needs a version of at most %d characters",
				ref.Name, maxSecretVersionLength)
		}
		if strings.ContainsAny(ref.Version, "\n\r\t ") {
			return fmt.Errorf("secret %q version must not contain whitespace", ref.Name)
		}
		if !strings.HasPrefix(ref.MountPath, "/") {
			return fmt.Errorf("secret %q mount path must be absolute", ref.Name)
		}
		// A relative element could escape the mount directory and place secret
		// material somewhere the workload does not expect.
		if strings.Contains(ref.MountPath, "..") {
			return fmt.Errorf("secret %q mount path must not contain %q", ref.Name, "..")
		}
		if seenNames[ref.Name] {
			return fmt.Errorf("secret %q is referenced twice", ref.Name)
		}
		if seenPaths[ref.MountPath] {
			return fmt.Errorf("two secrets share mount path %q", ref.MountPath)
		}
		seenNames[ref.Name] = true
		seenPaths[ref.MountPath] = true
	}
	return nil
}
