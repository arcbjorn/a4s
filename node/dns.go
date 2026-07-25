package node

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/arcbjorn/a4s/control"
)

// DNSPort is the conventional port a node's resolver listens on.
const DNSPort = 53

// maxDNSMessage bounds a UDP DNS message.
//
// 512 bytes is the classic limit; anything larger is refused rather than
// buffered, so a caller cannot make the resolver allocate arbitrary memory.
const maxDNSMessage = 512

// dnsTTL is how long a resolver answer may be cached, in seconds.
//
// It is deliberately short. Endpoints come from readiness evidence that itself
// expires, and a client caching an answer past the point where a4s stopped
// observing the instance as ready would send traffic somewhere the control
// plane no longer vouches for.
const dnsTTL = 5

// Resolver answers a4s service names for workloads on this node.
//
// It serves only the internal zone and refuses everything else rather than
// forwarding. A resolver that fell back to the public internet would turn a
// typo in a service name into a request to a stranger's server, which is a
// data-exfiltration path disguised as a convenience.
type Resolver struct {
	mu   sync.RWMutex
	zone control.ServiceZone
	conn net.PacketConn
}

// NewResolver builds a resolver holding an initially empty zone.
func NewResolver(nodeID string) *Resolver {
	return &Resolver{zone: control.ServiceZone{
		Node: nodeID, Records: map[string][]control.ResolvedEndpoint{},
	}}
}

// Apply replaces the served zone.
//
// Like the gateway, the resolver receives a complete zone and replaces its own;
// it never merges. A name the control plane did not publish cannot survive in
// the resolver through incremental drift.
func (r *Resolver) Apply(zone control.ServiceZone) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.zone = zone
}

// Zone returns the currently served zone.
func (r *Resolver) Zone() control.ServiceZone {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.zone
}

// Lookup resolves one name to the addresses a caller on this node should dial.
func (r *Resolver) Lookup(name string) ([]control.ResolvedEndpoint, bool) {
	workload, ok := control.ParseServiceName(name)
	if !ok {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	endpoints, known := r.zone.Records[control.ServiceName(workload)]
	return endpoints, known
}

// ListenAndServe answers queries on the given UDP address until closed.
func (r *Resolver) ListenAndServe(address string) error {
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		return fmt.Errorf("listen for dns queries: %w", err)
	}
	r.mu.Lock()
	r.conn = conn
	r.mu.Unlock()
	return r.serve(conn)
}

func (r *Resolver) serve(conn net.PacketConn) error {
	buffer := make([]byte, maxDNSMessage)
	for {
		read, from, err := conn.ReadFrom(buffer)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read dns query: %w", err)
		}
		response, err := r.answer(buffer[:read])
		if err != nil {
			// A malformed query is dropped rather than answered. There is no
			// useful reply to a message whose header could not be parsed.
			continue
		}
		if _, err := conn.WriteTo(response, from); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
		}
	}
}

// Close stops the resolver.
func (r *Resolver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return nil
	}
	err := r.conn.Close()
	r.conn = nil
	return err
}

// answer builds a response for one query message.
//
// Only A queries in the internal zone are answered. Anything else gets an
// authoritative name error, which is what keeps this resolver from being usable
// as a general forwarder.
func (r *Resolver) answer(query []byte) ([]byte, error) {
	header, question, name, qtype, err := parseQuery(query)
	if err != nil {
		return nil, err
	}

	// Header: same id, response bit, authoritative answer, recursion refused.
	response := make([]byte, 0, maxDNSMessage)
	response = append(response, header[0], header[1])
	flags := uint16(0x8400) // QR + AA
	if header[2]&0x01 != 0 {
		// Echo the recursion-desired bit but never set recursion-available:
		// this resolver does not recurse for anyone. header[2] is the first
		// flags byte, whose low bit is RD.
		flags |= 0x0100
	}

	var answers []control.ResolvedEndpoint
	if qtype == 1 { // A
		if endpoints, known := r.Lookup(name); known {
			answers = endpoints
		}
	}
	if len(answers) == 0 {
		flags |= 0x0003 // NXDOMAIN
	}

	response = binary.BigEndian.AppendUint16(response, flags)
	response = binary.BigEndian.AppendUint16(response, 1)                            // QDCOUNT
	response = binary.BigEndian.AppendUint16(response, uint16(len(ipv4Of(answers)))) // ANCOUNT
	response = binary.BigEndian.AppendUint16(response, 0)                            // NSCOUNT
	response = binary.BigEndian.AppendUint16(response, 0)                            // ARCOUNT
	response = append(response, question...)

	for _, address := range ipv4Of(answers) {
		// Name is a pointer back to the question, which keeps the response
		// inside the 512-byte limit for realistic endpoint counts.
		response = append(response, 0xC0, 0x0C)
		response = binary.BigEndian.AppendUint16(response, 1) // A
		response = binary.BigEndian.AppendUint16(response, 1) // IN
		response = binary.BigEndian.AppendUint32(response, dnsTTL)
		response = binary.BigEndian.AppendUint16(response, 4)
		response = append(response, address...)
		if len(response) > maxDNSMessage {
			// Truncate rather than emit an oversized UDP response. A client
			// that needs the full set can retry over TCP.
			return response[:maxDNSMessage], nil
		}
	}
	return response, nil
}

// ipv4Of keeps only endpoints with a usable IPv4 address, since an A record
// cannot carry anything else.
func ipv4Of(endpoints []control.ResolvedEndpoint) [][]byte {
	addresses := make([][]byte, 0, len(endpoints))
	for _, endpoint := range endpoints {
		parsed := net.ParseIP(endpoint.Address)
		if parsed == nil {
			continue
		}
		if four := parsed.To4(); four != nil {
			addresses = append(addresses, four)
		}
	}
	return addresses
}

// parseQuery extracts the header, raw question, name, and type from a query.
func parseQuery(query []byte) (header []byte, question []byte, name string, qtype uint16, err error) {
	if len(query) < 12 {
		return nil, nil, "", 0, fmt.Errorf("dns query is too short")
	}
	if binary.BigEndian.Uint16(query[4:6]) != 1 {
		// Exactly one question. Multi-question queries are not used in
		// practice and supporting them would complicate the parser for nothing.
		return nil, nil, "", 0, fmt.Errorf("dns query must carry exactly one question")
	}

	var labels []string
	offset := 12
	for offset < len(query) {
		length := int(query[offset])
		if length == 0 {
			offset++
			break
		}
		if length&0xC0 != 0 {
			// Compression pointers are not valid in a question section.
			return nil, nil, "", 0, fmt.Errorf("dns question uses compression")
		}
		offset++
		if offset+length > len(query) {
			return nil, nil, "", 0, fmt.Errorf("dns label runs past the message")
		}
		labels = append(labels, string(query[offset:offset+length]))
		offset += length
	}
	if offset+4 > len(query) {
		return nil, nil, "", 0, fmt.Errorf("dns question is truncated")
	}
	qtype = binary.BigEndian.Uint16(query[offset : offset+2])
	offset += 4

	return query[:4], query[12:offset], strings.Join(labels, "."), qtype, nil
}
