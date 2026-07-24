package node

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// DesiredAllocation is what the node believes should be running locally. It is
// recorded when the server authorizes a start and cleared when the server
// authorizes a stop or delete.
type DesiredAllocation struct {
	ID        string            `json:"id"`
	Workload  string            `json:"workload"`
	Image     string            `json:"image"`
	Resources control.Resources `json:"resources"`
	// Running is the last server-authorized intent for this allocation. A node
	// restarts a crashed task only while this is true.
	Running bool                `json:"running"`
	Probe   control.ProbeTarget `json:"probe"`
	// Volumes records the storage the server authorized for this allocation,
	// so a restarted node mounts exactly what it was told to.
	Volumes  []control.VolumeRef `json:"volumes,omitempty"`
	Restarts int                 `json:"restarts"`
	// UpdatedAt records when intent last changed, for operator diagnosis.
	UpdatedAt time.Time `json:"updated_at"`
}

// DesiredState is the node's durable record of server-authorized intent.
//
// It exists so the node can keep workloads running while the server is
// unreachable. Without it a node is purely reactive: a container that dies
// during a control-plane outage stays dead until the server returns. The node
// never invents intent, it only remembers what it was last told, which keeps
// authority with the control plane while making the data plane survivable.
type DesiredState struct {
	mu      sync.Mutex
	path    string
	entries map[string]DesiredAllocation
}

func OpenDesiredState(path string) (*DesiredState, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("desired state path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create desired state directory: %w", err)
	}
	state := &DesiredState{path: path, entries: make(map[string]DesiredAllocation)}
	if err := state.load(); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *DesiredState) load() error {
	file, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open desired state: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var entry DesiredAllocation
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return fmt.Errorf("decode desired state line %d: %w", line, err)
		}
		if entry.ID == "" {
			return fmt.Errorf("desired state line %d has no allocation id", line)
		}
		s.entries[entry.ID] = entry
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("read desired state: %w", err)
	}
	return nil
}

// persist rewrites the whole file atomically. Desired state is small and
// rewriting it keeps recovery trivial: the file is either the old state or the
// new one, never a partial mixture.
func (s *DesiredState) persist() error {
	temporary := s.path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open desired state temp: %w", err)
	}
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	writer := bufio.NewWriter(file)
	for _, id := range ids {
		record, err := json.Marshal(s.entries[id])
		if err != nil {
			file.Close()
			return fmt.Errorf("encode desired state: %w", err)
		}
		if _, err := writer.Write(append(record, '\n')); err != nil {
			file.Close()
			return fmt.Errorf("write desired state: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		file.Close()
		return fmt.Errorf("flush desired state: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync desired state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close desired state: %w", err)
	}
	return os.Rename(temporary, s.path)
}

// Record stores server-authorized intent for an allocation.
func (s *DesiredState) Record(entry DesiredAllocation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.ID == "" {
		return fmt.Errorf("desired allocation requires an id")
	}
	if existing, ok := s.entries[entry.ID]; ok {
		entry.Restarts = existing.Restarts
	}
	s.entries[entry.ID] = entry
	return s.persist()
}

// Forget removes an allocation from desired state, which stops supervision.
func (s *DesiredState) Forget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return nil
	}
	delete(s.entries, id)
	return s.persist()
}

// SetRunning updates only the running intent, leaving the rest of the record.
func (s *DesiredState) SetRunning(id string, running bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[id]
	if !ok {
		return nil
	}
	entry.Running = running
	entry.UpdatedAt = time.Now().UTC()
	s.entries[id] = entry
	return s.persist()
}

// AddVolume records that the server authorized a volume for an allocation.
func (s *DesiredState) AddVolume(id string, ref control.VolumeRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[id]
	if !ok {
		return nil
	}
	for _, existing := range entry.Volumes {
		if existing.Name == ref.Name {
			return nil
		}
	}
	entry.Volumes = append(entry.Volumes, ref)
	entry.UpdatedAt = time.Now().UTC()
	s.entries[id] = entry
	return s.persist()
}

func (s *DesiredState) recordRestart(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[id]
	if !ok {
		return nil
	}
	entry.Restarts++
	entry.UpdatedAt = time.Now().UTC()
	s.entries[id] = entry
	return s.persist()
}

// List returns desired allocations in stable order.
func (s *DesiredState) List() []DesiredAllocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	entries := make([]DesiredAllocation, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, s.entries[id])
	}
	return entries
}

func (s *DesiredState) Get(id string) (DesiredAllocation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[id]
	return entry, ok
}
