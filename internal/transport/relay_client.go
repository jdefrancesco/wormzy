package transport

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	relayALPN             = "p2p-wormzy-relay-2"
	relayTokenLabel       = "wormzy-relay-token-v2" // #nosec G101 -- public HMAC domain-separation label, not a credential.
	relaySessionIDLabel   = "wormzy-relay-session-id-v2"
	maxRelayHelloSize     = 1024
	maxRelayReadySize     = 256
	relayIdentifierLength = sha256.Size
)

type relayHello struct {
	Session string `json:"session"`
	Token   string `json:"token"`
	Role    string `json:"role"`
}

type relayWriteStream interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
	CancelWrite(quic.StreamErrorCode)
}

type relayIPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// deriveRelayToken creates the proof that both relay clients share the PAKE key.
func deriveRelayToken(psk []byte) [32]byte {
	return deriveRelayIdentifier(relayTokenLabel, psk)
}

// deriveRelaySessionID creates an opaque relay lookup value that cannot reveal
// or be correlated directly with the pairing code or mailbox identifier.
func deriveRelaySessionID(psk []byte) [32]byte {
	return deriveRelayIdentifier(relaySessionIDLabel, psk)
}

// deriveRelayIdentifier computes one domain-separated identifier from the PAKE key.
func deriveRelayIdentifier(label string, psk []byte) [32]byte {
	mac := hmac.New(sha256.New, psk)
	_, _ = mac.Write([]byte(label))
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

// dialRelay opens the unauthenticated QUIC carrier later authenticated by the PAKE key.
func dialRelay(ctx context.Context, addr string, cfg Config) (*quic.Conn, *quic.Transport, error) {
	udpAddr, err := resolveRelayUDPAddr(ctx, net.DefaultResolver, addr)
	if err != nil {
		return nil, nil, err
	}
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, nil, err
	}
	transport := &quic.Transport{Conn: udpConn}
	// The custom relay is not trusted for peer identity; the PAKE-derived relay
	// registration and pre-race key confirmation authenticate the remote peer.
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{relayALPN}} // #nosec G402 -- end-to-end PAKE authentication follows.
	quicConf := &quic.Config{
		KeepAlivePeriod:      15 * time.Second,
		MaxIdleTimeout:       cfg.IdleTimeout,
		HandshakeIdleTimeout: cfg.HandshakeTimeout,
	}
	conn, err := transport.Dial(ctx, udpAddr, tlsConf, quicConf)
	if err != nil {
		_ = udpConn.Close()
		return nil, nil, err
	}
	return conn, transport, nil
}

// resolveRelayUDPAddr resolves a relay hostname with caller cancellation so a
// stalled DNS lookup cannot outlive the configured handshake timeout.
func resolveRelayUDPAddr(ctx context.Context, resolver relayIPResolver, addr string) (*net.UDPAddr, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("parse relay address: %w", err)
	}
	if host == "" {
		return nil, errors.New("relay address requires a host")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, errors.New("relay address requires a valid UDP port")
	}
	if literal := net.ParseIP(host); literal != nil {
		if ipv4 := literal.To4(); ipv4 != nil {
			return &net.UDPAddr{IP: ipv4, Port: int(port)}, nil
		}
		return nil, errors.New("relay address must resolve to IPv4")
	}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve relay address: %w", err)
	}
	for _, address := range addresses {
		if ipv4 := address.IP.To4(); ipv4 != nil {
			return &net.UDPAddr{IP: ipv4, Port: int(port)}, nil
		}
	}
	return nil, errors.New("relay address did not resolve to IPv4")
}

// registerRelay proves a role's opaque PAKE-derived relay identity and waits for its peer.
func registerRelay(ctx context.Context, conn *quic.Conn, role string, psk []byte) error {
	if validateMailboxRole(role) != nil {
		return fmt.Errorf("invalid relay role %q", role)
	}
	if len(psk) == 0 {
		return errors.New("relay registration requires PAKE key")
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	token := deriveRelayToken(psk)
	sessionID := deriveRelaySessionID(psk)
	hello := relayHello{
		Session: hex.EncodeToString(sessionID[:]),
		Token:   hex.EncodeToString(token[:]),
		Role:    role,
	}
	if err := writeRelayHelloContext(ctx, stream, hello); err != nil {
		return err
	}
	return waitRelayReady(ctx, conn)
}

// writeRelayHelloContext binds registration writes and stream closure to ctx.
func writeRelayHelloContext(ctx context.Context, stream relayWriteStream, hello relayHello) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetWriteDeadline(deadline); err != nil {
			return err
		}
	}
	stopCancel := context.AfterFunc(ctx, func() {
		stream.CancelWrite(0)
	})
	defer stopCancel()
	if err := writeRelayHello(stream, hello); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// writeRelayHello sends one registration and closes the stream's write side so
// the relay's strict decoder sees EOF before the client waits for readiness.
func writeRelayHello(stream io.WriteCloser, hello relayHello) error {
	if err := json.NewEncoder(stream).Encode(&hello); err != nil {
		_ = stream.Close()
		return err
	}
	return stream.Close()
}

// decodeRelayHello strictly decodes and validates one bounded registration message.
func decodeRelayHello(r io.Reader) (relayHello, error) {
	limited := &io.LimitedReader{R: r, N: maxRelayHelloSize + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var hello relayHello
	if err := decoder.Decode(&hello); err != nil {
		return relayHello{}, err
	}
	if limited.N == 0 {
		return relayHello{}, errors.New("relay hello exceeds size limit")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return relayHello{}, errors.New("relay hello contains trailing data")
	}
	if err := validateRelayHello(hello); err != nil {
		return relayHello{}, err
	}
	return hello, nil
}

// validateRelayHello enforces role and opaque-identifier formatting.
func validateRelayHello(hello relayHello) error {
	if validateMailboxRole(hello.Role) != nil {
		return fmt.Errorf("invalid relay role %q", hello.Role)
	}
	for name, encoded := range map[string]string{"session": hello.Session, "token": hello.Token} {
		raw, err := hex.DecodeString(encoded)
		if err != nil || len(raw) != relayIdentifierLength {
			return fmt.Errorf("invalid relay %s", name)
		}
	}
	return nil
}

// relayTokensEqual compares encoded relay proofs without data-dependent timing.
func relayTokensEqual(left, right string) bool {
	leftRaw, leftErr := hex.DecodeString(left)
	rightRaw, rightErr := hex.DecodeString(right)
	return leftErr == nil && rightErr == nil && len(leftRaw) == relayIdentifierLength &&
		len(rightRaw) == relayIdentifierLength && hmac.Equal(leftRaw, rightRaw)
}

// relaySessionAlias returns a short, non-secret label for server diagnostics.
func relaySessionAlias(session string) string {
	if len(session) < 8 {
		return "unknown"
	}
	return "r-" + session[:8]
}

// waitRelayReady waits for the bounded server notification that both peers are registered.
func waitRelayReady(ctx context.Context, conn *quic.Conn) error {
	us, err := conn.AcceptUniStream(ctx)
	if err != nil {
		return err
	}
	defer us.CancelRead(0)
	if deadline, ok := ctx.Deadline(); ok {
		if err := us.SetReadDeadline(deadline); err != nil {
			return err
		}
	}
	return decodeRelayReady(us)
}

// decodeRelayReady accepts exactly one bounded ready response from the custom relay.
func decodeRelayReady(r io.Reader) error {
	limited := &io.LimitedReader{R: r, N: maxRelayReadySize + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var message struct {
		Status string `json:"status"`
	}
	if err := decoder.Decode(&message); err != nil {
		return err
	}
	if limited.N == 0 {
		return errors.New("relay ready response exceeds size limit")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("relay ready response contains trailing data")
	}
	if message.Status != "ready" {
		return errors.New("relay response is not ready")
	}
	return nil
}
