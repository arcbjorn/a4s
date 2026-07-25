package node

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

func servedResolver(t *testing.T, records map[string][]control.ResolvedEndpoint) string {
	t.Helper()
	resolver := NewResolver("alpha")
	resolver.Apply(control.ServiceZone{Node: "alpha", Records: records})

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	resolver.conn = conn
	go func() { _ = resolver.serve(conn) }()
	t.Cleanup(func() { _ = resolver.Close() })
	return conn.LocalAddr().String()
}

// Go's own resolver parses the response, which is an independent check on the
// hand-written wire encoding rather than a test of my parser against itself.
func TestResolverAnswersRealDNSQuery(t *testing.T) {
	address := servedResolver(t, map[string][]control.ResolvedEndpoint{
		"api.a4s.internal": {
			{Address: "10.42.0.5", Port: 8000, Allocation: "api-0", Node: "alpha", Local: true},
		},
	})

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", address)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addresses, err := resolver.LookupHost(ctx, "api.a4s.internal")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(addresses) != 1 || addresses[0] != "10.42.0.5" {
		t.Fatalf("resolved to %v, want the allocation address", addresses)
	}
}

func TestResolverReturnsEveryEndpoint(t *testing.T) {
	address := servedResolver(t, map[string][]control.ResolvedEndpoint{
		"api.a4s.internal": {
			{Address: "10.42.0.5", Port: 8000, Allocation: "api-0", Node: "alpha", Local: true},
			{Address: "100.64.0.2", Port: 8080, Allocation: "api-1", Node: "beta"},
		},
	})

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", address)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addresses, err := resolver.LookupHost(ctx, "api.a4s.internal")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(addresses) != 2 {
		t.Fatalf("resolved to %v, want both endpoints", addresses)
	}
}

// An unknown name must be an authoritative NXDOMAIN, not a hang or a forward.
func TestResolverReturnsNameErrorForUnknownService(t *testing.T) {
	address := servedResolver(t, map[string][]control.ResolvedEndpoint{})

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", address)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := resolver.LookupHost(ctx, "ghost.a4s.internal"); err == nil {
		t.Fatal("expected an unknown service to fail resolution")
	}
}

// The resolver must refuse names outside its zone rather than forwarding them.
// A resolver that forwarded would turn a typo into a request to a stranger.
func TestResolverRefusesForeignNames(t *testing.T) {
	address := servedResolver(t, map[string][]control.ResolvedEndpoint{
		"api.a4s.internal": {
			{Address: "10.42.0.5", Port: 8000, Allocation: "api-0", Node: "alpha", Local: true},
		},
	})

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", address)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := resolver.LookupHost(ctx, "example.com"); err == nil {
		t.Fatal("the resolver answered for a name outside its zone")
	}
}

func TestLookupResolvesBareAndQualifiedNames(t *testing.T) {
	resolver := NewResolver("alpha")
	resolver.Apply(control.ServiceZone{
		Node: "alpha",
		Records: map[string][]control.ResolvedEndpoint{
			"api.a4s.internal": {{Address: "10.42.0.5", Port: 8000, Allocation: "api-0"}},
		},
	})

	for _, name := range []string{"api", "api.a4s.internal", "API.a4s.internal."} {
		if _, known := resolver.Lookup(name); !known {
			t.Fatalf("lookup %q did not resolve", name)
		}
	}
	if _, known := resolver.Lookup("example.com"); known {
		t.Fatal("resolved a name outside the zone")
	}
}

// Applying a zone replaces rather than merges, so a name the control plane
// stopped publishing cannot survive in the resolver.
func TestApplyReplacesTheZone(t *testing.T) {
	resolver := NewResolver("alpha")
	resolver.Apply(control.ServiceZone{
		Node: "alpha",
		Records: map[string][]control.ResolvedEndpoint{
			"old.a4s.internal": {{Address: "10.42.0.5", Port: 8000}},
		},
	})
	resolver.Apply(control.ServiceZone{
		Node: "alpha",
		Records: map[string][]control.ResolvedEndpoint{
			"new.a4s.internal": {{Address: "10.42.0.6", Port: 8000}},
		},
	})

	if _, known := resolver.Lookup("old"); known {
		t.Fatal("a withdrawn name survived a zone replacement")
	}
	if _, known := resolver.Lookup("new"); !known {
		t.Fatal("the replacement zone was not applied")
	}
}

// A malformed query must be dropped rather than crashing the resolver.
func TestMalformedQueryIsRefused(t *testing.T) {
	resolver := NewResolver("alpha")
	for _, query := range [][]byte{
		{}, {0x00}, make([]byte, 11),
		// Claims two questions.
		{0, 1, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0},
		// Label length runs past the message.
		{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0x40, 'a'},
	} {
		if _, err := resolver.answer(query); err == nil {
			t.Fatalf("malformed query %v was accepted", query)
		}
	}
}

// A compression pointer in the question section is malformed and must not be
// followed, since following one is how a parser gets driven into a loop.
func TestCompressedQuestionIsRefused(t *testing.T) {
	resolver := NewResolver("alpha")
	query := []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xC0, 0x0C}
	if _, err := resolver.answer(query); err == nil {
		t.Fatal("a compressed question was accepted")
	}
}

// Only A queries are answered; anything else gets a name error rather than a
// malformed record.
func TestNonAddressQueryIsNotAnswered(t *testing.T) {
	resolver := NewResolver("alpha")
	resolver.Apply(control.ServiceZone{
		Node: "alpha",
		Records: map[string][]control.ResolvedEndpoint{
			"api.a4s.internal": {{Address: "10.42.0.5", Port: 8000}},
		},
	})
	// A TXT query (type 16) for a name that does exist.
	query := buildQuery(t, "api.a4s.internal", 16)
	response, err := resolver.answer(query)
	if err != nil {
		t.Fatal(err)
	}
	if answerCount(response) != 0 {
		t.Fatal("a non-address query was answered with records")
	}
}

// buildQuery encodes a minimal DNS question.
func buildQuery(t *testing.T, name string, qtype byte) []byte {
	t.Helper()
	query := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(name, ".") {
		query = append(query, byte(len(label)))
		query = append(query, label...)
	}
	query = append(query, 0, 0, qtype, 0, 1)
	return query
}

func answerCount(response []byte) int {
	if len(response) < 8 {
		return 0
	}
	return int(response[6])<<8 | int(response[7])
}
