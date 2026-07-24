package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/arcbjorn/a4s/control"
)

// RuntimeAPIVersion is the contract an agent image implements.
//
// It matches the runtime name a goal declares. A workload naming a version the
// node does not serve is refused at validation rather than discovered when the
// agent's first call fails.
const RuntimeAPIVersion = "a4s.agent/v1"

// RuntimeAPI is the workload-facing surface of the node.
//
// Every other node surface talks to the control plane, which is authenticated
// and trusted. This one talks to the agent, which is neither. The whole design
// follows from that: the caller proves identity with a node-issued token, names
// nothing about itself, and can only reach operations that were already bounded
// by the kernel.
//
// It listens on a Unix socket rather than a port. An agent's authority is
// per-instance, and a TCP listener would be reachable by every workload on the
// node and potentially off it, which would turn a per-instance credential into a
// cluster-wide attack surface.
type RuntimeAPI struct {
	Agents *Agents
	// TokenRoot is where per-allocation credentials are written, one directory
	// per allocation. Empty disables file provisioning, for tests and for nodes
	// that deliver the token another way.
	TokenRoot string

	// mu guards the routing tables below. They are written by the allocation
	// lifecycle and read by every in-flight agent request, which are different
	// goroutines.
	mu sync.RWMutex
	// brokers maps a queue name to its broker. An agent pulls only from the
	// queue its workload declared, which the node resolves rather than accepting
	// as a parameter.
	brokers map[string]*QueueBroker
	// queues maps an allocation to the queue it may pull from, established when
	// the allocation was authorized.
	queues map[string]string

	socket   string
	listener net.Listener
	server   *http.Server
}

// ServeRuntimeAPI starts the workload-facing API on a Unix socket.
func ServeRuntimeAPI(socket string, agents *Agents) (*RuntimeAPI, error) {
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("runtime socket path must be absolute")
	}
	if agents == nil {
		return nil, fmt.Errorf("runtime api requires the agent capability")
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		return nil, fmt.Errorf("create runtime socket directory: %w", err)
	}
	// A leftover socket from a crashed node would block the listener. Removing
	// it is safe because a live node holds the file open.
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clear runtime socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen on runtime socket: %w", err)
	}
	api := &RuntimeAPI{
		Agents: agents, brokers: map[string]*QueueBroker{},
		queues: map[string]string{}, socket: socket, listener: listener,
	}
	api.server = &http.Server{Handler: api.routes()}
	go func() { _ = api.server.Serve(listener) }()
	return api, nil
}

func (a *RuntimeAPI) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/task/claim", a.authenticated(a.claim))
	mux.HandleFunc("/v1/task/ack", a.authenticated(a.ack))
	mux.HandleFunc("/v1/task/requeue", a.authenticated(a.requeue))
	mux.HandleFunc("/v1/spend", a.authenticated(a.spend))
	mux.HandleFunc("/v1/tool/authorize", a.authenticated(a.authorizeTool))
	mux.HandleFunc("/v1/identity", a.authenticated(a.identity))
	return mux
}

// runtimeHandler is a request already attributed to an allocation.
type runtimeHandler func(http.ResponseWriter, *http.Request, string)

// authenticated resolves the caller's token to an allocation before the handler
// runs.
//
// No handler ever reads an allocation id from the request body. That is the
// single rule that makes the node's gates hold: an agent cannot address another
// instance's budget, envelope, or task slot, because it has no way to name one.
func (a *RuntimeAPI) authenticated(handler runtimeHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			writeRuntimeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		token := bearerToken(r)
		allocation, err := a.Agents.Resolve(token)
		if err != nil {
			// A revoked or forged token is indistinguishable from an unknown one,
			// and saying which would confirm valid identifiers to a caller
			// probing for them.
			writeRuntimeError(w, http.StatusUnauthorized, "unrecognized runtime token")
			return
		}
		handler(w, r, allocation)
	}
}

// identity lets a runtime discover which allocation it is, so an image needs no
// configuration beyond its token.
func (a *RuntimeAPI) identity(w http.ResponseWriter, _ *http.Request, allocation string) {
	spent, _ := a.Agents.Spent(allocation)
	writeRuntimeJSON(w, http.StatusOK, map[string]any{
		"allocation": allocation,
		"version":    RuntimeAPIVersion,
		"queue":      a.queueFor(allocation),
		"draining":   a.Agents.Draining(allocation),
		"spent":      spent,
		"tools":      a.Agents.Tools(allocation),
	})
}

// claimRequest carries no allocation: identity comes from the token.
type claimResponse struct {
	Claimed bool   `json:"claimed"`
	TaskID  string `json:"task_id,omitempty"`
	Payload string `json:"payload,omitempty"`
	// Reason explains an empty claim, so a runtime can distinguish an idle queue
	// from being refused work.
	Reason string `json:"reason,omitempty"`
}

func (a *RuntimeAPI) claim(w http.ResponseWriter, _ *http.Request, allocation string) {
	broker, err := a.broker(allocation)
	if err != nil {
		writeRuntimeError(w, http.StatusBadRequest, err.Error())
		return
	}
	task, claimed, err := broker.Claim(allocation)
	if err != nil {
		// Draining and exhaustion are ordinary lifecycle states, not faults. A
		// runtime should stop asking rather than retry, so they are reported as
		// a refusal with a reason instead of an error.
		if errors.Is(err, ErrDraining) || errors.Is(err, ErrBudgetExhausted) {
			writeRuntimeJSON(w, http.StatusOK, claimResponse{Reason: err.Error()})
			return
		}
		writeRuntimeError(w, http.StatusConflict, err.Error())
		return
	}
	if !claimed {
		writeRuntimeJSON(w, http.StatusOK, claimResponse{Reason: "no work available"})
		return
	}
	writeRuntimeJSON(w, http.StatusOK, claimResponse{
		Claimed: true, TaskID: task.ID, Payload: task.Payload,
	})
}

type taskRequest struct {
	TaskID string `json:"task_id"`
}

func (a *RuntimeAPI) ack(w http.ResponseWriter, r *http.Request, allocation string) {
	a.completeTask(w, r, allocation, func(broker *QueueBroker, id string) error {
		return broker.Ack(allocation, id)
	})
}

func (a *RuntimeAPI) requeue(w http.ResponseWriter, r *http.Request, allocation string) {
	a.completeTask(w, r, allocation, func(broker *QueueBroker, id string) error {
		return broker.Requeue(allocation, id)
	})
}

func (a *RuntimeAPI) completeTask(w http.ResponseWriter, r *http.Request, allocation string,
	complete func(*QueueBroker, string) error) {
	var request taskRequest
	if err := decodeRuntimeJSON(r, &request); err != nil {
		writeRuntimeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.TaskID == "" {
		writeRuntimeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	broker, err := a.broker(allocation)
	if err != nil {
		writeRuntimeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The broker checks that this allocation actually holds the task, so an
	// agent cannot complete work it was never given.
	if err := complete(broker, request.TaskID); err != nil {
		writeRuntimeError(w, http.StatusConflict, err.Error())
		return
	}
	writeRuntimeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// spendRequest reports consumption since the last report.
type spendRequest struct {
	Tokens      int `json:"tokens"`
	CostMillis  int `json:"cost_millis"`
	WallSeconds int `json:"wall_seconds"`
	ToolCalls   int `json:"tool_calls"`
}

type spendResponse struct {
	// Continue tells the runtime whether it may keep working. A well-behaved
	// agent stops here; the node does not depend on that, since the tool gate
	// refuses an exhausted instance regardless.
	Continue bool           `json:"continue"`
	Spent    control.Budget `json:"spent"`
}

func (a *RuntimeAPI) spend(w http.ResponseWriter, r *http.Request, allocation string) {
	var request spendRequest
	if err := decodeRuntimeJSON(r, &request); err != nil {
		writeRuntimeError(w, http.StatusBadRequest, err.Error())
		return
	}
	spent, within := a.Agents.Spend(allocation, control.Budget{
		Tokens: request.Tokens, CostMillis: request.CostMillis,
		WallSeconds: request.WallSeconds, ToolCalls: request.ToolCalls,
	})
	writeRuntimeJSON(w, http.StatusOK, spendResponse{Continue: within, Spent: spent})
}

type toolRequest struct {
	Tool  string `json:"tool"`
	Scope string `json:"scope"`
}

func (a *RuntimeAPI) authorizeTool(w http.ResponseWriter, r *http.Request, allocation string) {
	var request toolRequest
	if err := decodeRuntimeJSON(r, &request); err != nil {
		writeRuntimeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.Agents.AuthorizeToolCall(allocation, request.Tool, request.Scope); err != nil {
		// A refusal is a definite answer, not a server fault. The runtime is
		// expected to handle it and continue with what it may do.
		writeRuntimeJSON(w, http.StatusForbidden, map[string]any{
			"authorized": false, "reason": err.Error(),
		})
		return
	}
	writeRuntimeJSON(w, http.StatusOK, map[string]any{"authorized": true})
}

// broker resolves the queue an allocation may pull from.
//
// The queue comes from what the control plane authorized, never from the
// request. An agent naming its own queue could drain another workload's backlog.
func (a *RuntimeAPI) broker(allocation string) (*QueueBroker, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	name, ok := a.queues[allocation]
	if !ok || name == "" {
		return nil, fmt.Errorf("allocation %q is not bound to a queue", allocation)
	}
	broker, ok := a.brokers[name]
	if !ok || broker == nil {
		return nil, fmt.Errorf("queue %q is not served by this node", name)
	}
	return broker, nil
}

// queueFor reports the queue an allocation is bound to, for identity responses.
func (a *RuntimeAPI) queueFor(allocation string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.queues[allocation]
}

// Bind records which queue an allocation pulls from.
func (a *RuntimeAPI) Bind(allocation, queue string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.queues == nil {
		a.queues = map[string]string{}
	}
	a.queues[allocation] = queue
}

// Unbind forgets an allocation's queue binding.
func (a *RuntimeAPI) Unbind(allocation string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.queues, allocation)
}

// Serve registers a queue broker under its queue name.
func (a *RuntimeAPI) Serve(queue string, broker *QueueBroker) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.brokers == nil {
		a.brokers = map[string]*QueueBroker{}
	}
	a.brokers[queue] = broker
}

// Provision writes an allocation's credential where only that container can
// read it.
//
// The token is material, so it is treated like secret material: one file per
// allocation, owner-readable, under a directory the node controls. It is
// deliberately not passed as an environment variable, which would put a live
// credential into process listings and crash dumps.
func (a *RuntimeAPI) Provision(allocation, token string) error {
	if a.TokenRoot == "" {
		return nil
	}
	directory := filepath.Join(a.TokenRoot, allocation)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create runtime token directory: %w", err)
	}
	path := filepath.Join(directory, "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("write runtime token: %w", err)
	}
	return nil
}

// Revoke removes an allocation's credential and queue binding.
//
// The in-memory token is invalidated by Agents.Release; this clears the file so
// a credential does not outlive the workload that held it.
func (a *RuntimeAPI) Revoke(allocation string) error {
	a.Unbind(allocation)
	if a.TokenRoot == "" {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(a.TokenRoot, allocation)); err != nil {
		return fmt.Errorf("remove runtime token: %w", err)
	}
	return nil
}

// TokenPath reports where an allocation's credential lives, which is what the
// container mounts.
func (a *RuntimeAPI) TokenPath(allocation string) string {
	if a.TokenRoot == "" {
		return ""
	}
	return filepath.Join(a.TokenRoot, allocation, "token")
}

// Socket reports the path agents connect to.
func (a *RuntimeAPI) Socket() string { return a.socket }

func (a *RuntimeAPI) Close() error {
	if a.server == nil {
		return nil
	}
	err := a.server.Close()
	_ = os.Remove(a.socket)
	return err
}

// bearerToken extracts the credential from an Authorization header.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}

// maxRuntimeRequest bounds a request body. An agent is untrusted, and an
// unbounded body would let one exhaust node memory without spending a token.
const maxRuntimeRequest = 1 << 20

func decodeRuntimeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRuntimeRequest))
	// Unknown fields are refused so a runtime built against a later contract
	// fails loudly here rather than silently having its intent dropped.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}

func writeRuntimeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeRuntimeError(w http.ResponseWriter, status int, message string) {
	writeRuntimeJSON(w, status, map[string]string{"error": message})
}
