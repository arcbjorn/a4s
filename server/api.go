package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/obs"
)

// MaxRequestBody bounds how much an operator request may carry.
//
// A decoder without a limit lets one caller consume memory until the control
// plane dies, which would make an availability failure reachable from an
// unauthenticated socket. The limit is applied before authentication for
// exactly that reason: verifying a signature over an unbounded body would
// already have done the damage.
const MaxRequestBody = 1 << 20 // 1 MiB

// MaxSignatureHeader bounds the signed envelope carried in the request header.
const MaxSignatureHeader = 8 << 10 // 8 KiB

// SignatureHeader carries the base64 signed request envelope.
//
// The envelope travels in a header rather than the body so that the body stays
// exactly the operator's payload, and so a body digest is a meaningful binding
// rather than a self-reference.
const SignatureHeader = "A4s-Signature"

// APIConfig configures the operator HTTP API.
type APIConfig struct {
	// Logger receives request outcomes. Authentication failures are logged at
	// warn: they are the signal that someone is probing the control plane.
	Logger *slog.Logger
	// Now supplies the clock, so tests can drive expiry deterministically.
	Now func() time.Time
	// Metrics records request outcomes. Optional.
	Metrics *obs.Metrics
}

// API serves the operator surface over HTTP.
//
// Every mutating endpoint requires a signed request from a known operator key.
// The API holds no operator private key and cannot manufacture authority: it
// verifies statements operators made and refuses everything else.
type API struct {
	server *Server
	config APIConfig
	nonces *nonceLedger
}

// NewAPI builds the operator API for a server.
func NewAPI(server *Server, config APIConfig) *API {
	if config.Logger == nil {
		config.Logger = obs.Discard()
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &API{server: server, config: config, nonces: newNonceLedger()}
}

// Handler returns the HTTP handler for the operator API.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Reads. These are authenticated too: the world projection, history, and
	// diagnoses describe the whole cluster, which is not public information.
	mux.HandleFunc("GET /v1/status", a.authenticated(a.status))
	mux.HandleFunc("GET /v1/world", a.authenticated(a.world))
	mux.HandleFunc("GET /v1/goals", a.authenticated(a.listGoals))
	mux.HandleFunc("GET /v1/events", a.authenticated(a.events))
	mux.HandleFunc("GET /v1/approvals", a.authenticated(a.approvals))
	mux.HandleFunc("GET /v1/directory", a.authenticated(a.directory))
	mux.HandleFunc("GET /v1/routes", a.authenticated(a.routes))
	mux.HandleFunc("GET /v1/explain/{target}", a.authenticated(a.explain))
	mux.HandleFunc("GET /v1/plan/{goal}", a.authenticated(a.plan))
	mux.HandleFunc("GET /v1/diagnose/{goal}", a.authenticated(a.diagnose))

	// Writes.
	mux.HandleFunc("POST /v1/goals", a.authenticated(a.submitGoal))
	mux.HandleFunc("POST /v1/approvals", a.authenticated(a.approve))
	mux.HandleFunc("POST /v1/approvals/revoke", a.authenticated(a.revoke))

	// Health is unauthenticated on purpose: a load balancer or supervisor must
	// be able to ask whether the process is alive without holding an operator
	// key. It reports liveness only and discloses no cluster state.
	mux.HandleFunc("GET /v1/health", a.health)

	// Metrics are authenticated. Counts of allocations, nodes, and refused
	// requests describe the cluster's shape and are not public information.
	mux.HandleFunc("GET /v1/metrics", a.authenticated(a.metrics))

	return mux
}

func (a *API) metrics(writer http.ResponseWriter, _ *http.Request, _ RequestEnvelope) {
	if a.config.Metrics == nil {
		http.Error(writer, "metrics are not enabled", http.StatusNotFound)
		return
	}
	// Refresh the gauges that describe current world shape, so a scrape
	// reports what is true now rather than at the last reconciliation.
	status := a.server.Status()
	a.config.Metrics.SetGauge("a4s_world_revision", int64(status.Revision))
	a.config.Metrics.SetGauge("a4s_goals", int64(status.Goals))
	a.config.Metrics.SetGauge("a4s_nodes", int64(status.Nodes))
	a.config.Metrics.SetGauge("a4s_allocations", int64(status.Allocations))
	a.config.Metrics.SetGauge("a4s_routes", int64(status.Routes))
	a.config.Metrics.SetGauge("a4s_events", int64(status.Events))

	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, a.config.Metrics.Text())
}

// authenticated wraps a handler with body limiting, signature verification, and
// replay rejection. Every operator endpoint goes through here.
func (a *API) authenticated(handler func(http.ResponseWriter, *http.Request, RequestEnvelope)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		// Limit before reading. An unbounded read is a denial-of-service vector
		// that does not require a valid signature to exploit.
		request.Body = http.MaxBytesReader(writer, request.Body, MaxRequestBody)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				a.deny(writer, request, http.StatusRequestEntityTooLarge, "request body exceeds limit")
				return
			}
			a.deny(writer, request, http.StatusBadRequest, "request body could not be read")
			return
		}

		header := request.Header.Get(SignatureHeader)
		if header == "" {
			a.deny(writer, request, http.StatusUnauthorized, "request is not signed")
			return
		}
		if len(header) > MaxSignatureHeader {
			a.deny(writer, request, http.StatusRequestEntityTooLarge, "signature header exceeds limit")
			return
		}
		var signed SignedRequest
		if err := json.Unmarshal([]byte(header), &signed); err != nil {
			a.deny(writer, request, http.StatusUnauthorized, "signature header is malformed")
			return
		}

		now := a.config.Now()
		envelope, err := VerifyRequest(signed, a.server.OperatorKeys(),
			request.Method, request.URL.Path, body, now)
		if err != nil {
			// The specific reason goes to the log, not the caller: an attacker
			// tuning a forgery should not learn which check rejected it.
			a.config.Logger.Warn("operator request refused",
				slog.String("path", request.URL.Path),
				slog.String("method", request.Method),
				slog.Any("error", err))
			a.record("denied")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !a.nonces.observe(envelope.KeyID, envelope.Nonce, envelope.ExpiresAt, now) {
			a.config.Logger.Warn("operator request replayed",
				slog.String("path", request.URL.Path),
				slog.String("operator", envelope.IssuedBy),
				slog.String("nonce", envelope.Nonce))
			a.record("replayed")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Handlers that decode a body read it from here rather than the request,
		// because the body was already consumed to verify its digest.
		request = request.WithContext(withBody(request.Context(), body))
		a.config.Logger.Info("operator request",
			slog.String("method", request.Method),
			slog.String("path", request.URL.Path),
			slog.String("operator", envelope.IssuedBy),
			slog.String("key_id", envelope.KeyID))
		a.record("accepted")
		handler(writer, request, envelope)
	}
}

func (a *API) record(outcome string) {
	if a.config.Metrics != nil {
		a.config.Metrics.CountRequest(outcome)
	}
}

func (a *API) deny(writer http.ResponseWriter, request *http.Request, status int, reason string) {
	a.config.Logger.Warn("operator request refused",
		slog.String("path", request.URL.Path),
		slog.String("method", request.Method),
		slog.String("reason", reason))
	a.record("denied")
	http.Error(writer, reason, status)
}

func (a *API) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) status(writer http.ResponseWriter, _ *http.Request, _ RequestEnvelope) {
	writeJSON(writer, http.StatusOK, a.server.Status())
}

func (a *API) world(writer http.ResponseWriter, _ *http.Request, _ RequestEnvelope) {
	writeJSON(writer, http.StatusOK, a.server.World())
}

func (a *API) listGoals(writer http.ResponseWriter, _ *http.Request, _ RequestEnvelope) {
	writeJSON(writer, http.StatusOK, a.server.Goals())
}

func (a *API) directory(writer http.ResponseWriter, _ *http.Request, _ RequestEnvelope) {
	writeJSON(writer, http.StatusOK, a.server.Directory())
}

func (a *API) routes(writer http.ResponseWriter, _ *http.Request, _ RequestEnvelope) {
	writeJSON(writer, http.StatusOK, a.server.RouteSnapshots())
}

func (a *API) approvals(writer http.ResponseWriter, _ *http.Request, _ RequestEnvelope) {
	writeJSON(writer, http.StatusOK, a.server.Approvals())
}

// events answers a history query. The filters mirror the `a4s history` command,
// so the CLI and API cannot drift into answering differently.
func (a *API) events(writer http.ResponseWriter, request *http.Request, _ RequestEnvelope) {
	query := HistoryQuery{
		GoalID: request.URL.Query().Get("goal"),
		Target: request.URL.Query().Get("target"),
		Kind:   request.URL.Query().Get("kind"),
	}
	if raw := request.URL.Query().Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			http.Error(writer, "since must be an RFC3339 timestamp", http.StatusBadRequest)
			return
		}
		query.Since = since
	}
	if raw := request.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 {
			http.Error(writer, "limit must be a non-negative integer", http.StatusBadRequest)
			return
		}
		query.Limit = limit
	}
	writeJSON(writer, http.StatusOK, a.server.Query(query))
}

func (a *API) explain(writer http.ResponseWriter, request *http.Request, _ RequestEnvelope) {
	target := request.PathValue("target")
	explanation := a.server.Explain(target)
	if !explanation.Found {
		writeJSON(writer, http.StatusNotFound, explanation)
		return
	}
	writeJSON(writer, http.StatusOK, explanation)
}

func (a *API) plan(writer http.ResponseWriter, request *http.Request, _ RequestEnvelope) {
	result, err := a.server.Plan(request.PathValue("goal"))
	if err != nil {
		http.Error(writer, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (a *API) diagnose(writer http.ResponseWriter, request *http.Request, _ RequestEnvelope) {
	// The API uses the deterministic diagnoser. A model-backed explanation is
	// available through the CLI, where the operator chooses to consult a model
	// and can see the provenance of the answer.
	writeJSON(writer, http.StatusOK,
		a.server.Diagnose(request.PathValue("goal"), control.LogDiagnoser{}))
}

// submitGoal accepts an operator goal. Validation happens inside Submit, so an
// unsatisfiable goal is refused at this boundary rather than failing later.
func (a *API) submitGoal(writer http.ResponseWriter, request *http.Request, envelope RequestEnvelope) {
	var goal control.Goal
	if err := decodeBody(request, &goal); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.server.Submit(goal); err != nil {
		http.Error(writer, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	a.config.Logger.Info("goal accepted",
		slog.String("goal", goal.ID), slog.String("operator", envelope.IssuedBy))
	writeJSON(writer, http.StatusCreated, map[string]string{"goal": goal.ID})
}

// approve records an operator-signed approval grant.
//
// The grant carries its own operator signature, separate from the request
// signature. That is deliberate: the request signature proves who is calling,
// while the grant signature is the durable authenticated record of the decision
// itself, which outlives the connection and is what the kernel later checks.
func (a *API) approve(writer http.ResponseWriter, request *http.Request, _ RequestEnvelope) {
	var signed control.SignedApproval
	if err := decodeBody(request, &signed); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	grant, err := a.server.Approve(signed)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	a.config.Logger.Info("approval recorded",
		slog.String("approval", grant.ID),
		slog.String("scope", grant.Scope),
		slog.String("goal", grant.GoalID),
		slog.String("issued_by", grant.IssuedBy))
	writeJSON(writer, http.StatusCreated, grant)
}

func (a *API) revoke(writer http.ResponseWriter, request *http.Request, _ RequestEnvelope) {
	var signed control.SignedApproval
	if err := decodeBody(request, &signed); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.server.Revoke(signed); err != nil {
		http.Error(writer, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "revoked"})
}

// decodeBody strictly decodes the already-read request body. Unknown fields are
// refused so a payload written against different semantics cannot be silently
// accepted with the parts this build understands.
func decodeBody(request *http.Request, target any) error {
	body := bodyFrom(request.Context())
	if len(body) == 0 {
		return fmt.Errorf("request body is required")
	}
	decoder := json.NewDecoder(newReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("malformed request body")
	}
	if decoder.More() {
		return fmt.Errorf("request body has trailing content")
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
