package main

import (
	"bytes"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/server"
)

// operatorClient signs and issues requests against a remote control plane.
//
// The client holds the operator's private key and the server holds only the
// public half, so authority always originates with a person and the server can
// verify a decision without being able to make one.
type operatorClient struct {
	address  string
	keyID    string
	issuedBy string
	key      ed25519.PrivateKey
	http     *http.Client
}

func newOperatorClient(address, keyID, issuedBy string, key ed25519.PrivateKey) *operatorClient {
	return &operatorClient{
		address: strings.TrimSuffix(address, "/"), keyID: keyID, issuedBy: issuedBy, key: key,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// do signs one request and returns its body. Each call gets a fresh nonce, so a
// captured request cannot be reused even against the same endpoint.
func (c *operatorClient) do(method, path string, body any) ([]byte, error) {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		payload = encoded
	}

	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	// The signed path excludes the query string, matching what the server
	// verifies against r.URL.Path.
	signedPath := path
	if index := strings.IndexByte(signedPath, '?'); index >= 0 {
		signedPath = signedPath[:index]
	}
	signed, err := server.SignRequest(server.RequestEnvelope{
		Nonce: nonce, Method: method, Path: signedPath,
		BodyDigest: server.BodyDigest(payload), IssuedBy: c.issuedBy,
		IssuedAt: now, ExpiresAt: now.Add(2 * time.Minute),
	}, c.keyID, c.key)
	if err != nil {
		return nil, err
	}
	header, err := json.Marshal(signed)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequest(method, c.address+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set(server.SignatureHeader, string(header))
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("contact server: %w", err)
	}
	defer response.Body.Close()

	// The response is bounded for the same reason the server bounds requests: a
	// hostile or broken peer must not be able to exhaust this process.
	answer, err := io.ReadAll(io.LimitReader(response.Body, server.MaxRequestBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("server refused %s %s: %s: %s",
			method, path, response.Status, strings.TrimSpace(string(answer)))
	}
	return answer, nil
}

func randomNonce() (string, error) {
	raw := make([]byte, 16)
	if _, err := cryptorand.Read(raw); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// clientFlags registers the connection flags shared by every remote command.
type clientFlags struct {
	address  *string
	keyID    *string
	issuedBy *string
	keyPath  *string
}

func registerClientFlags(flags *flag.FlagSet) clientFlags {
	return clientFlags{
		address:  flags.String("server", "http://127.0.0.1:8443", "operator API address"),
		keyID:    flags.String("key-id", "", "operator key id the server trusts"),
		issuedBy: flags.String("operator", "", "operator principal name"),
		keyPath:  flags.String("operator-key", "", "path to the base64 Ed25519 operator private key"),
	}
}

func (f clientFlags) client() (*operatorClient, error) {
	if *f.keyID == "" || *f.keyPath == "" {
		return nil, fmt.Errorf("key-id and operator-key are required")
	}
	key, err := loadPrivateKey(*f.keyPath)
	if err != nil {
		return nil, err
	}
	issuedBy := *f.issuedBy
	if issuedBy == "" {
		// The key id is a reasonable default principal: it already names who is
		// acting, and requiring a second name for the common case is friction
		// without added accountability.
		issuedBy = *f.keyID
	}
	return newOperatorClient(*f.address, *f.keyID, issuedBy, key), nil
}

// submit sends a goal to a running control plane.
func submit(args []string) error {
	flags := flag.NewFlagSet("submit", flag.ContinueOnError)
	connection := registerClientFlags(flags)
	file := flags.String("file", "", "scenario or goal document to submit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("file is required")
	}
	client, err := connection.client()
	if err != nil {
		return err
	}

	goal, err := loadGoal(*file)
	if err != nil {
		return err
	}
	if _, err := client.do(http.MethodPost, "/v1/goals", goal); err != nil {
		return err
	}
	fmt.Printf("submitted goal %s\n", goal.ID)
	return nil
}

// loadGoal reads a goal from either a full scenario document or a bare goal, so
// the same example files used for simulation can be submitted to a server.
func loadGoal(path string) (control.Goal, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return control.Goal{}, fmt.Errorf("read goal: %w", err)
	}
	var scenario struct {
		Goal *control.Goal `json:"goal"`
	}
	if err := json.Unmarshal(raw, &scenario); err == nil && scenario.Goal != nil {
		return *scenario.Goal, nil
	}
	var goal control.Goal
	if err := json.Unmarshal(raw, &goal); err != nil {
		return control.Goal{}, fmt.Errorf("decode goal: %w", err)
	}
	if goal.ID == "" {
		return control.Goal{}, fmt.Errorf("document contains neither a goal nor a scenario")
	}
	return goal, nil
}

// status reports what a running control plane currently holds.
func status(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	connection := registerClientFlags(flags)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	client, err := connection.client()
	if err != nil {
		return err
	}
	answer, err := client.do(http.MethodGet, "/v1/status", nil)
	if err != nil {
		return err
	}
	if *jsonOutput {
		fmt.Print(string(answer))
		return nil
	}
	var reported server.Status
	if err := json.Unmarshal(answer, &reported); err != nil {
		return fmt.Errorf("decode status: %w", err)
	}
	fmt.Printf("revision %d from %d events: %d goals, %d nodes, %d allocations, %d routes\n",
		reported.Revision, reported.Events, reported.Goals,
		reported.Nodes, reported.Allocations, reported.Routes)
	// Printed only when a safeguard is actually holding something back. A line
	// of zeroes on every healthy cluster would train an operator to skip the
	// one line that explains a stalled one.
	if reported.Cordoned > 0 || reported.BackingOff > 0 || reported.Disruptions > 0 {
		fmt.Printf("holding back: %d/%d nodes schedulable, %d targets in backoff, %d recent disruptions\n",
			reported.Schedulable, reported.Nodes, reported.BackingOff, reported.Disruptions)
	}
	return nil
}

// remoteEvents queries history from a running server.
func remoteEvents(args []string) error {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	connection := registerClientFlags(flags)
	goalID := flags.String("goal", "", "restrict to one goal")
	target := flags.String("target", "", "restrict to one allocation or route")
	kind := flags.String("kind", "", "restrict to one event type")
	since := flags.Duration("since", 0, "only events within this window")
	until := flags.Duration("until", 0, "only events older than this")
	limit := flags.Int("limit", 0, "return at most this many events")
	if err := flags.Parse(args); err != nil {
		return err
	}
	client, err := connection.client()
	if err != nil {
		return err
	}

	query := url.Values{}
	for name, value := range map[string]string{
		"goal": *goalID, "target": *target, "kind": *kind,
	} {
		if value != "" {
			query.Set(name, value)
		}
	}
	// The window is expressed as an age here and as a timestamp on the wire,
	// because an operator reaching for history thinks in "the last ten minutes"
	// rather than in RFC3339.
	now := time.Now().UTC()
	for name, age := range map[string]time.Duration{"since": *since, "until": *until} {
		if age > 0 {
			query.Set(name, now.Add(-age).Format(time.RFC3339Nano))
		}
	}
	if *limit > 0 {
		query.Set("limit", fmt.Sprint(*limit))
	}
	path := "/v1/events"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}

	answer, err := client.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	var events []control.Event
	if err := json.Unmarshal(answer, &events); err != nil {
		return fmt.Errorf("decode events: %w", err)
	}
	if len(events) == 0 {
		fmt.Println("no matching events")
		return nil
	}
	for _, event := range events {
		fmt.Printf("%02d  %-20s  %-16s  %s\n",
			event.Sequence, event.Type, event.Actor, event.Message)
	}
	return nil
}
