package reason

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func textResponse(w http.ResponseWriter, text string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
}

// The key travels in a custom header, which Go forwards on redirect. A
// redirect must be refused, not followed to whatever host it names.
func TestAnthropicDoesNotForwardKeyOnRedirect(t *testing.T) {
	var leaked string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("x-api-key")
		textResponse(w, "ok")
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	// A caller-supplied client with default redirect handling is still safe.
	client := &Anthropic{APIKey: "sk-secret", Endpoint: redirector.URL, Client: &http.Client{}}
	if _, err := client.Complete(context.Background(), "prompt"); err == nil ||
		!strings.Contains(err.Error(), "307") {
		t.Fatalf("want the redirect reported as its status, got %v", err)
	}
	if leaked != "" {
		t.Fatal("the API key was forwarded to the redirect target")
	}
}

func TestAnthropicBoundsResponseSize(t *testing.T) {
	huge := strings.Repeat("a", maxProviderResponse+1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		textResponse(w, huge)
	}))
	defer provider.Close()
	client := &Anthropic{APIKey: "k", Endpoint: provider.URL, Client: provider.Client()}
	if _, err := client.Complete(context.Background(), "prompt"); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want an oversized response refused, got %v", err)
	}
}

// A proxy's HTML error page must surface as its status, not a JSON error.
func TestAnthropicReportsStatusOfNonJSONFailure(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer provider.Close()
	client := &Anthropic{APIKey: "k", Endpoint: provider.URL, Client: provider.Client()}
	_, err := client.Complete(context.Background(), "prompt")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want the 502 reported, got %v", err)
	}
}

func TestAnthropicReportsProviderErrorMessage(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": "rate_limit_error", "message": "slow down"},
		})
	}))
	defer provider.Close()
	client := &Anthropic{APIKey: "k", Endpoint: provider.URL, Client: provider.Client()}
	_, err := client.Complete(context.Background(), "prompt")
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("want the status and provider message, got %v", err)
	}
}

func TestNewAnthropicReadsTrimmedKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "  sk-test\n")
	client := NewAnthropic()
	if client.APIKey != "sk-test" || !client.Configured() {
		t.Fatalf("key = %q, configured = %t", client.APIKey, client.Configured())
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if NewAnthropic().Configured() {
		t.Fatal("a client with no key reports itself configured")
	}
}
