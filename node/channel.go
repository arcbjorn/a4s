package node

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

// maxRecordSize bounds one encrypted record.
//
// A length prefix a peer controls is a memory-exhaustion vector: without a
// ceiling, one frame claiming four gigabytes would allocate it. The limit is
// well above any real action envelope.
const maxRecordSize = 4 << 20 // 4 MiB

// channelKeyLength is the symmetric key size for ChaCha20-Poly1305.
const channelKeyLength = chacha20poly1305.KeySize

// secureConn wraps a connection in authenticated encryption.
//
// Enrollment proves who the peer is; this proves nobody else can read or edit
// what they say. The two are separate concerns and are deliberately layered:
// the signed action envelope inside remains the authority boundary, so a
// decryption failure is a transport problem and a signature failure is an
// authorization problem.
type secureConn struct {
	net.Conn

	readMu  sync.Mutex
	writeMu sync.Mutex
	// source is where ciphertext is read from: the socket, or the socket
	// preceded by bytes the handshake reader had already consumed.
	source   io.Reader
	sendAEAD cipher.AEAD
	recvAEAD cipher.AEAD
	// Sequence numbers form the nonce, so an attacker cannot replay or reorder
	// records within a session. They never wrap: the session ends first.
	sendSeq uint64
	recvSeq uint64
	// pending holds plaintext not yet consumed by a short Read.
	pending []byte
}

// deriveChannelKeys turns the shared secret into two directional keys.
//
// Separate keys per direction mean a record the server sent can never be
// replayed back to it as though the node had sent it. The handshake transcript
// is the salt, which binds the derived keys to the exact exchange that produced
// them: a tampered handshake yields different keys and the session fails rather
// than proceeding under an attacker's parameters.
func deriveChannelKeys(shared []byte, transcript []byte) (clientToServer, serverToClient []byte, err error) {
	clientToServer, err = hkdf.Key(sha256.New, shared, transcript,
		"a4s node channel v1 client-to-server", channelKeyLength)
	if err != nil {
		return nil, nil, fmt.Errorf("derive channel key: %w", err)
	}
	serverToClient, err = hkdf.Key(sha256.New, shared, transcript,
		"a4s node channel v1 server-to-client", channelKeyLength)
	if err != nil {
		return nil, nil, fmt.Errorf("derive channel key: %w", err)
	}
	return clientToServer, serverToClient, nil
}

// newSecureConn builds an encrypted connection from directional keys.
//
// carried holds ciphertext the handshake reader already consumed; it is read
// before the socket so the peer's first record is not lost.
func newSecureConn(conn net.Conn, sendKey, receiveKey []byte, carried ...[]byte) (*secureConn, error) {
	sendAEAD, err := chacha20poly1305.New(sendKey)
	if err != nil {
		return nil, fmt.Errorf("build send cipher: %w", err)
	}
	recvAEAD, err := chacha20poly1305.New(receiveKey)
	if err != nil {
		return nil, fmt.Errorf("build receive cipher: %w", err)
	}
	secure := &secureConn{Conn: conn, sendAEAD: sendAEAD, recvAEAD: recvAEAD}
	var prefix []byte
	for _, chunk := range carried {
		prefix = append(prefix, chunk...)
	}
	if len(prefix) > 0 {
		secure.source = io.MultiReader(bytes.NewReader(prefix), conn)
	} else {
		secure.source = conn
	}
	return secure, nil
}

// nonceFor builds a deterministic nonce from a sequence number.
//
// ChaCha20-Poly1305 requires a nonce never repeat under one key. A counter
// guarantees that within a session, and each session derives fresh keys from a
// fresh ephemeral exchange, so counters restarting at zero is safe.
func nonceFor(sequence uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(nonce[chacha20poly1305.NonceSize-8:], sequence)
	return nonce
}

// Write encrypts one record and frames it with a length prefix.
func (c *secureConn) Write(plaintext []byte) (int, error) {
	if len(plaintext) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	written := 0
	for len(plaintext) > 0 {
		// Records are chunked so a large payload cannot exceed the frame limit
		// the peer enforces on read.
		chunk := plaintext
		if len(chunk) > maxRecordSize-chacha20poly1305.Overhead {
			chunk = chunk[:maxRecordSize-chacha20poly1305.Overhead]
		}
		sealed := c.sendAEAD.Seal(nil, nonceFor(c.sendSeq), chunk, nil)
		c.sendSeq++

		frame := make([]byte, 4+len(sealed))
		binary.BigEndian.PutUint32(frame[:4], uint32(len(sealed)))
		copy(frame[4:], sealed)
		if _, err := c.Conn.Write(frame); err != nil {
			return written, err
		}
		written += len(chunk)
		plaintext = plaintext[len(chunk):]
	}
	return written, nil
}

// Read returns decrypted plaintext, buffering across short reads.
func (c *secureConn) Read(out []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if len(c.pending) == 0 {
		plaintext, err := c.readRecord()
		if err != nil {
			return 0, err
		}
		c.pending = plaintext
	}
	copied := copy(out, c.pending)
	c.pending = c.pending[copied:]
	return copied, nil
}

func (c *secureConn) readRecord() ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(c.source, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 {
		return nil, errors.New("channel received an empty record")
	}
	if length > maxRecordSize {
		// Refuse before allocating. A peer-controlled length is only safe with
		// a ceiling checked first.
		return nil, fmt.Errorf("channel record of %d bytes exceeds the %d byte limit",
			length, maxRecordSize)
	}
	sealed := make([]byte, length)
	if _, err := io.ReadFull(c.source, sealed); err != nil {
		return nil, err
	}
	plaintext, err := c.recvAEAD.Open(nil, nonceFor(c.recvSeq), sealed, nil)
	if err != nil {
		// An authentication failure means the record was edited, reordered, or
		// replayed. None of those are recoverable, so the session ends.
		return nil, fmt.Errorf("channel record failed authentication: the stream was tampered with")
	}
	c.recvSeq++
	return plaintext, nil
}

// generateEphemeral produces a fresh X25519 keypair for one session.
//
// The keys are ephemeral rather than derived from node identity so that
// recording a session today and compromising the identity key tomorrow does not
// reveal what was said. That is the forward secrecy the sealed-secret path,
// which intentionally uses long-lived identity keys, cannot provide.
func generateEphemeral() (private, public []byte, err error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	return key.Bytes(), key.PublicKey().Bytes(), nil
}

// decodeEphemeral parses a peer's X25519 share. An absent share is not an
// error: it means the peer predates channel encryption.
func decodeEphemeral(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("ephemeral key is not valid base64")
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("ephemeral key is not a valid X25519 point")
	}
	return raw, nil
}

// sharedSecret completes the X25519 exchange.
func sharedSecret(privateBytes, peerPublic []byte) ([]byte, error) {
	private, err := ecdh.X25519().NewPrivateKey(privateBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid ephemeral private key: %w", err)
	}
	peer, err := ecdh.X25519().NewPublicKey(peerPublic)
	if err != nil {
		return nil, fmt.Errorf("peer ephemeral key is invalid: %w", err)
	}
	// ECDH rejects low-order points, so a peer cannot force a predictable
	// shared secret.
	shared, err := private.ECDH(peer)
	if err != nil {
		return nil, fmt.Errorf("key agreement failed: %w", err)
	}
	return shared, nil
}
