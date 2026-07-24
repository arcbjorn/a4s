package reason

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/arcbjorn/a4s/control"
)

// DefaultModel is the model a diagnosis uses unless one is configured.
//
// It is pinned rather than an alias for the same reason a workload image is
// pinned by digest: an explanation should be attributable to an exact model,
// and a provider repointing an alias would silently change how the control
// plane explains itself.
const DefaultModel = "claude-opus-5"

// anthropicEndpoint is the Messages API.
const anthropicEndpoint = "https://api.anthropic.com/v1/messages"

// anthropicVersion is the API version header every request carries.
const anthropicVersion = "2023-06-01"

// maxDiagnosisTokens bounds one response. A diagnosis is a short list of
// causes; a larger ceiling would only buy a longer refusal to be concise.
const maxDiagnosisTokens = 2048

// Anthropic is a minimal Messages API client.
//
// It is deliberately hand-rolled against the HTTP API rather than built on the
// provider SDK. The project takes on dependencies only where the alternative is
// reimplementing something substantial — containerd's client earns its place;
// a single JSON POST does not. This also keeps a model provider out of the
// dependency graph of a control plane that must build and run without one.
type Anthropic struct {
	// APIKey authenticates to the provider. Empty means the client is
	// unconfigured and every diagnosis falls back.
	APIKey string
	// Model pins the exact model.
	Model string
	// Endpoint allows redirecting to a proxy or a test server.
	Endpoint string
	// Client performs the request.
	Client *http.Client
}

// NewAnthropic builds a client from the environment.
//
// A missing key is not an error. A node with no model configured must still
// start and reconcile, so an unconfigured client reports itself as unavailable
// and the deterministic path takes over.
func NewAnthropic() *Anthropic {
	return &Anthropic{
		APIKey:   strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		Model:    DefaultModel,
		Endpoint: anthropicEndpoint,
		Client:   &http.Client{Timeout: DefaultTimeout},
	}
}

// Configured reports whether the client can reach a provider.
func (a *Anthropic) Configured() bool {
	return a != nil && a.APIKey != ""
}

// anthropicRequest is the Messages API request body.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	// Thinking is left unset. A diagnosis is a short structured judgement over
	// supplied facts, not a reasoning-heavy task, and the extra latency would
	// be spent on an explanation an operator is waiting for.
	OutputConfig *anthropicOutput `json:"output_config,omitempty"`
}

type anthropicOutput struct {
	// Effort bounds how much the model spends. Low is deliberate: the context
	// is already reduced to the relevant facts, so depth buys little here.
	Effort string `json:"effort,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse is the subset of the response this package reads.
type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Model      string `json:"model"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements Completer against the Messages API.
func (a *Anthropic) Complete(ctx context.Context, prompt string) (string, error) {
	if !a.Configured() {
		return "", fmt.Errorf("no ANTHROPIC_API_KEY configured")
	}
	body, err := json.Marshal(anthropicRequest{
		Model:     a.model(),
		MaxTokens: maxDiagnosisTokens,
		System:    control.ModelDiagnosisInstructions,
		Messages: []anthropicMessage{
			{Role: "user", Content: prompt},
		},
		OutputConfig: &anthropicOutput{Effort: "low"},
	})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("anthropic-version", anthropicVersion)
	request.Header.Set("x-api-key", a.APIKey)

	response, err := a.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("call provider: %w", err)
	}
	defer response.Body.Close()

	var decoded anthropicResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		if decoded.Error != nil {
			return "", fmt.Errorf("provider returned %d: %s", response.StatusCode, decoded.Error.Message)
		}
		return "", fmt.Errorf("provider returned %d", response.StatusCode)
	}
	// A refusal is a definite answer, not a transport failure. It means the
	// model declined rather than that the provider is unreachable, and either
	// way the deterministic diagnosis is what an operator gets.
	if decoded.StopReason == "refusal" {
		return "", fmt.Errorf("model declined to answer")
	}

	var text strings.Builder
	for _, block := range decoded.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return "", fmt.Errorf("model returned no text")
	}
	return text.String(), nil
}

func (a *Anthropic) model() string {
	if a.Model != "" {
		return a.Model
	}
	return DefaultModel
}

func (a *Anthropic) endpoint() string {
	if a.Endpoint != "" {
		return a.Endpoint
	}
	return anthropicEndpoint
}

func (a *Anthropic) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: DefaultTimeout}
}

// verify Anthropic satisfies the interface the diagnoser needs.
var _ Completer = (*Anthropic)(nil)
