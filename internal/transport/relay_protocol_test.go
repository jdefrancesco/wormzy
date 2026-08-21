package transport

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

type blockingRelayWriteStream struct {
	canceled chan struct{}
	once     sync.Once
	deadline time.Time
}

type blockingRelayResolver struct{}

// LookupIPAddr blocks until cancellation to model a stalled DNS resolver.
func (blockingRelayResolver) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestResolveRelayUDPAddrHonorsContext verifies DNS cannot extend the relay handshake timeout.
func TestResolveRelayUDPAddrHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := resolveRelayUDPAddr(ctx, blockingRelayResolver{}, "relay.invalid:3478")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolve error = %v; want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled resolution took %s", elapsed)
	}
}

// Write blocks until cancellation to model a peer that withholds stream credit.
func (s *blockingRelayWriteStream) Write([]byte) (int, error) {
	<-s.canceled
	return 0, context.Canceled
}

// Close satisfies relayWriteStream without changing the cancellation signal.
func (s *blockingRelayWriteStream) Close() error { return nil }

// SetWriteDeadline records the context deadline applied by the protocol helper.
func (s *blockingRelayWriteStream) SetWriteDeadline(deadline time.Time) error {
	s.deadline = deadline
	return nil
}

// CancelWrite releases a blocked write exactly once.
func (s *blockingRelayWriteStream) CancelWrite(quic.StreamErrorCode) {
	s.once.Do(func() { close(s.canceled) })
}

// TestRelayControlWritesHonorContext verifies registration and ready writes cannot stall indefinitely.
func TestRelayControlWritesHonorContext(t *testing.T) {
	hello := relayHello{
		Session: strings.Repeat("1", relayIdentifierLength*2),
		Token:   strings.Repeat("2", relayIdentifierLength*2),
		Role:    "send",
	}
	tests := []struct {
		name string
		run  func(context.Context, relayWriteStream) error
	}{
		{name: "hello", run: func(ctx context.Context, stream relayWriteStream) error {
			return writeRelayHelloContext(ctx, stream, hello)
		}},
		{name: "ready", run: writeRelayReady},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := &blockingRelayWriteStream{canceled: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := test.run(ctx, stream); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("context-bound write error = %v; want deadline exceeded", err)
			}
			if stream.deadline.IsZero() {
				t.Fatal("write deadline was not set")
			}
		})
	}
}

// TestRelayIdentifiersAreOpaqueAndDomainSeparated verifies the relay receives
// neither the pairing code nor a mailbox-correlatable identifier.
func TestRelayIdentifiersAreOpaqueAndDomainSeparated(t *testing.T) {
	psk := []byte("relay protocol test PAKE key")
	session := deriveRelaySessionID(psk)
	token := deriveRelayToken(psk)
	if bytes.Equal(session[:], token[:]) {
		t.Fatal("relay session ID and token are not domain-separated")
	}
	hello := relayHello{
		Session: hex.EncodeToString(session[:]),
		Token:   hex.EncodeToString(token[:]),
		Role:    "send",
	}
	wire, err := json.Marshal(hello)
	if err != nil {
		t.Fatalf("encode relay hello: %v", err)
	}
	if bytes.Contains(wire, []byte(testPairingCode)) {
		t.Fatalf("relay hello exposes pairing code: %s", wire)
	}
	decoded, err := decodeRelayHello(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("decode relay hello: %v", err)
	}
	if decoded != hello {
		t.Fatalf("decoded hello = %#v; want %#v", decoded, hello)
	}
}

// TestWriteRelayHelloClosesRegistrationStream verifies strict server decoding
// receives EOF before either client waits for the relay-ready notification.
func TestWriteRelayHelloClosesRegistrationStream(t *testing.T) {
	client, server := net.Pipe()
	hello := relayHello{
		Session: strings.Repeat("1", relayIdentifierLength*2),
		Token:   strings.Repeat("2", relayIdentifierLength*2),
		Role:    "send",
	}
	decoded := make(chan relayHello, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := decodeRelayHello(server)
		if err != nil {
			errs <- err
			return
		}
		decoded <- got
	}()
	if err := writeRelayHello(client, hello); err != nil {
		t.Fatalf("write relay hello: %v", err)
	}
	select {
	case err := <-errs:
		t.Fatalf("decode relay hello: %v", err)
	case got := <-decoded:
		if got != hello {
			t.Fatalf("decoded hello = %#v; want %#v", got, hello)
		}
	case <-time.After(time.Second):
		t.Fatal("relay hello decoder remained blocked waiting for EOF")
	}
	_ = server.Close()
}

// TestDecodeRelayHelloRejectsMalformedInput verifies untrusted QUIC
// registrations are bounded and strictly validated.
func TestDecodeRelayHelloRejectsMalformedInput(t *testing.T) {
	validSession := strings.Repeat("0", relayIdentifierLength*2)
	validToken := strings.Repeat("1", relayIdentifierLength*2)
	tests := map[string]string{
		"invalid role":    `{"session":"` + validSession + `","token":"` + validToken + `","role":"other"}`,
		"short session":   `{"session":"00","token":"` + validToken + `","role":"send"}`,
		"unknown field":   `{"session":"` + validSession + `","token":"` + validToken + `","role":"send","extra":true}`,
		"trailing object": `{"session":"` + validSession + `","token":"` + validToken + `","role":"send"}{}`,
		"oversized":       strings.Repeat("x", maxRelayHelloSize+1),
	}
	for name, wire := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRelayHello(strings.NewReader(wire)); err == nil {
				t.Fatal("accepted malformed relay hello")
			}
		})
	}
}

// TestDecodeRelayReadyRequiresOneBoundedReadyMessage verifies a custom relay
// cannot allocate or keep feeding arbitrary pre-ready responses.
func TestDecodeRelayReadyRequiresOneBoundedReadyMessage(t *testing.T) {
	if err := decodeRelayReady(strings.NewReader(`{"status":"ready"}`)); err != nil {
		t.Fatalf("decode valid relay ready: %v", err)
	}
	for name, wire := range map[string]string{
		"wrong status":  `{"status":"waiting"}`,
		"unknown field": `{"status":"ready","extra":true}`,
		"trailing":      `{"status":"ready"}{}`,
		"oversized":     `{"status":"` + strings.Repeat("x", maxRelayReadySize) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := decodeRelayReady(strings.NewReader(wire)); err == nil {
				t.Fatal("accepted invalid relay ready response")
			}
		})
	}
}

// TestRelayV2UsesDedicatedALPN verifies old relay servers cannot accidentally
// accept the incompatible opaque-session registration format.
func TestRelayV2UsesDedicatedALPN(t *testing.T) {
	if relayALPN == alpn {
		t.Fatal("relay v2 must not reuse the direct transport ALPN")
	}
	if !strings.Contains(relayALPN, "relay-2") {
		t.Fatalf("relay ALPN %q does not identify protocol v2", relayALPN)
	}
}

// TestRelayRegisterRejectsMismatchesWithoutEvictingWaiter verifies an attacker
// cannot remove a legitimate first peer by guessing its opaque session ID.
func TestRelayRegisterRejectsMismatchesWithoutEvictingWaiter(t *testing.T) {
	session := strings.Repeat("a", relayIdentifierLength*2)
	token := strings.Repeat("b", relayIdentifierLength*2)
	server := &RelayServer{}
	waiter := &relayServerClient{
		hello:    relayHello{Session: session, Token: token, Role: "send"},
		sourceIP: "192.0.2.1",
	}
	if pair, err := server.register(waiter); err != nil || pair != nil {
		t.Fatalf("register first peer = (%v, %v); want waiting", pair, err)
	}
	attacker := &relayServerClient{
		hello:    relayHello{Session: session, Token: strings.Repeat("c", relayIdentifierLength*2), Role: "recv"},
		sourceIP: "198.51.100.2",
	}
	if pair, err := server.register(attacker); !errors.Is(err, errRelayPeerMismatch) || pair != nil {
		t.Fatalf("register mismatched peer = (%v, %v); want mismatch", pair, err)
	}
	if server.waiting[session] != waiter {
		t.Fatal("mismatched peer evicted the legitimate waiter")
	}
	peer := &relayServerClient{
		hello:    relayHello{Session: session, Token: token, Role: "recv"},
		sourceIP: "203.0.113.3",
	}
	pair, err := server.register(peer)
	if err != nil || pair == nil || pair.a != waiter || pair.b != peer {
		t.Fatalf("register matching peer = (%v, %v); want legitimate pair", pair, err)
	}
}

// TestRelayRegisterBoundsWaitingPeers verifies both global and per-source
// admission limits fail closed before retaining another connection.
func TestRelayRegisterBoundsWaitingPeers(t *testing.T) {
	newClient := func(sessionByte, sourceIP string) *relayServerClient {
		return &relayServerClient{
			hello: relayHello{
				Session: strings.Repeat(sessionByte, relayIdentifierLength*2),
				Token:   strings.Repeat("f", relayIdentifierLength*2),
				Role:    "send",
			},
			sourceIP: sourceIP,
		}
	}

	server := &RelayServer{waitingLimit: 2, waitingPerIPLimit: 1}
	if _, err := server.register(newClient("1", "192.0.2.1")); err != nil {
		t.Fatalf("register first peer: %v", err)
	}
	if _, err := server.register(newClient("2", "192.0.2.1")); !errors.Is(err, errRelayCapacity) {
		t.Fatalf("same-source register error = %v; want capacity", err)
	}
	if _, err := server.register(newClient("2", "192.0.2.2")); err != nil {
		t.Fatalf("register second source: %v", err)
	}
	if _, err := server.register(newClient("3", "192.0.2.3")); !errors.Is(err, errRelayCapacity) {
		t.Fatalf("global-cap register error = %v; want capacity", err)
	}
	if len(server.waiting) != 2 {
		t.Fatalf("waiting peers = %d; want 2", len(server.waiting))
	}
}

// TestRelayConnectionAdmissionBoundsAllLivePeers verifies self-pairing cannot
// bypass the global or per-source limits that cover waiting and active peers.
func TestRelayConnectionAdmissionBoundsAllLivePeers(t *testing.T) {
	server := &RelayServer{liveLimit: 3, livePerIPLimit: 2}
	if !server.admitConnection("192.0.2.1") || !server.admitConnection("192.0.2.1") {
		t.Fatal("rejected connections below the per-source limit")
	}
	if server.admitConnection("192.0.2.1") {
		t.Fatal("accepted connection above the per-source limit")
	}
	if !server.admitConnection("192.0.2.2") {
		t.Fatal("rejected connection below the global limit")
	}
	if server.admitConnection("192.0.2.3") {
		t.Fatal("accepted connection above the global limit")
	}
	server.releaseConnection("192.0.2.1")
	if !server.admitConnection("192.0.2.3") {
		t.Fatal("did not release live-connection capacity")
	}
}

// TestRelayUnregisterReconcilesPairingRace verifies an expired first-peer
// timer cannot close a connection that was paired at the same instant.
func TestRelayUnregisterReconcilesPairingRace(t *testing.T) {
	session := strings.Repeat("d", relayIdentifierLength*2)
	token := strings.Repeat("e", relayIdentifierLength*2)
	server := &RelayServer{}
	waiter := &relayServerClient{
		hello:  relayHello{Session: session, Token: token, Role: "send"},
		paired: make(chan struct{}),
	}
	if _, err := server.register(waiter); err != nil {
		t.Fatalf("register waiter: %v", err)
	}
	peer := &relayServerClient{hello: relayHello{Session: session, Token: token, Role: "recv"}}
	if pair, err := server.register(peer); err != nil || pair == nil {
		t.Fatalf("register peer = (%v, %v); want pair", pair, err)
	}
	if server.unregisterWaiting(waiter) {
		t.Fatal("paired client was reported as an expired waiter")
	}

	unpaired := &relayServerClient{
		hello: relayHello{
			Session: strings.Repeat("f", relayIdentifierLength*2),
			Token:   token,
			Role:    "send",
		},
	}
	if _, err := server.register(unpaired); err != nil {
		t.Fatalf("register unpaired client: %v", err)
	}
	if !server.unregisterWaiting(unpaired) {
		t.Fatal("unpaired client was not removed")
	}
}

// TestRelayPairLifetimesHaveBoundedDefaults verifies operators can tune both
// the application-idle and absolute relay pair limits.
func TestRelayPairLifetimesHaveBoundedDefaults(t *testing.T) {
	server := &RelayServer{}
	if got := server.effectivePairIdleTimeout(); got != defaultRelayPairIdleTimeout {
		t.Fatalf("default pair idle timeout = %s; want %s", got, defaultRelayPairIdleTimeout)
	}
	if got := server.effectivePairLifetime(); got != defaultRelayPairLifetime {
		t.Fatalf("default pair lifetime = %s; want %s", got, defaultRelayPairLifetime)
	}
	server.PairIdleTimeout = time.Second
	server.PairLifetime = time.Minute
	if got := server.effectivePairIdleTimeout(); got != time.Second {
		t.Fatalf("custom pair idle timeout = %s; want %s", got, time.Second)
	}
	if got := server.effectivePairLifetime(); got != time.Minute {
		t.Fatalf("custom pair lifetime = %s; want %s", got, time.Minute)
	}
}

// TestRelayActivityWriterSignalsSuccessfulBytes verifies keepalive traffic
// cannot masquerade as relayed application activity.
func TestRelayActivityWriterSignalsSuccessfulBytes(t *testing.T) {
	var destination bytes.Buffer
	activity := make(chan struct{}, 1)
	writer := relayActivityWriter{writer: &destination, activity: activity}
	if _, err := writer.Write([]byte("payload")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if destination.String() != "payload" {
		t.Fatalf("destination = %q; want payload", destination.String())
	}
	select {
	case <-activity:
	default:
		t.Fatal("successful relay write did not signal activity")
	}
}
