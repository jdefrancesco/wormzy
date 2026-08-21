package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	defaultRelayWaitingLimit      = 1024
	defaultRelayWaitingPerIPLimit = 8
	defaultRelayLiveLimit         = 2048
	defaultRelayLivePerIPLimit    = 16
	relayRegistrationTimeout      = 5 * time.Second
	relayReadyTimeout             = 5 * time.Second
	relayPairingWaitTimeout       = 90 * time.Second
	defaultRelayPairIdleTimeout   = 5 * time.Minute
	defaultRelayPairLifetime      = 12 * time.Hour
)

var (
	errRelayCapacity     = errors.New("relay waiting capacity reached")
	errRelayPeerMismatch = errors.New("relay peer registration mismatch")
)

// RelayServer relays QUIC streams between paired clients that share a code+token.
// Payloads stay Noise-encrypted end-to-end; the server only forwards bytes.
type RelayServer struct {
	Addr      string
	Logger    *slog.Logger
	Telemetry *ServiceTelemetry
	// PairIdleTimeout closes paired peers that relay no application bytes.
	PairIdleTimeout time.Duration
	// PairLifetime sets an absolute lifetime even when application data continues.
	PairLifetime time.Duration

	mu      sync.Mutex
	waiting map[string]*relayServerClient
	live    map[string]int
	liveN   int

	waitingLimit      int
	waitingPerIPLimit int
	liveLimit         int
	livePerIPLimit    int
	pairingWait       time.Duration
}

type relayServerClient struct {
	conn     *quic.Conn
	hello    relayHello
	sourceIP string
	paired   chan struct{}
}

func (s *RelayServer) ListenAndServe(ctx context.Context) error {
	addr := s.Addr
	if addr == "" {
		addr = fmt.Sprintf(":%d", defaultRelayUDPPort)
	}
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return err
	}
	udpConn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return err
	}
	return s.serve(ctx, udpConn)
}

// serve accepts relay clients on an already-bound UDP socket. Separating the
// listener setup keeps startup ownership explicit and permits race-free tests.
func (s *RelayServer) serve(ctx context.Context, udpConn *net.UDPConn) error {
	defer udpConn.Close()

	tlsConf, err := selfSignedTLS()
	if err != nil {
		return err
	}
	tlsConf.NextProtos = []string{relayALPN}
	quicConf := &quic.Config{
		KeepAlivePeriod:       15 * time.Second,
		MaxIdleTimeout:        2 * time.Minute,
		MaxIncomingStreams:    16,
		MaxIncomingUniStreams: 16,
	}
	transport := &quic.Transport{Conn: udpConn}
	ln, err := transport.Listen(tlsConf, quicConf)
	if err != nil {
		return err
	}
	defer ln.Close()
	s.log().Info("relay listening", "addr", udpConn.LocalAddr())

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log().Warn("relay accept", "err", err)
			s.Telemetry.RecordError(err)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *RelayServer) handleConn(ctx context.Context, conn *quic.Conn) {
	s.Telemetry.ConnectionOpened()
	defer s.Telemetry.ConnectionClosed()
	sourceIP := relaySourceIP(conn.RemoteAddr())
	if !s.admitConnection(sourceIP) {
		err := errRelayCapacity
		s.Telemetry.RecordError(err)
		_ = conn.CloseWithError(0, "relay capacity reached")
		return
	}
	defer s.releaseConnection(sourceIP)
	registrationCtx, cancelRegistration := context.WithTimeout(ctx, relayRegistrationTimeout)
	defer cancelRegistration()
	stream, err := conn.AcceptStream(registrationCtx)
	if err != nil {
		s.Telemetry.RecordError(err)
		_ = conn.CloseWithError(0, "")
		return
	}
	if err := stream.SetReadDeadline(time.Now().Add(relayRegistrationTimeout)); err != nil {
		s.Telemetry.RecordError(err)
		_ = conn.CloseWithError(0, "")
		return
	}
	hello, err := decodeRelayHello(stream)
	if err != nil {
		s.log().Warn("relay hello decode", "err", err)
		s.Telemetry.RecordError(err)
		_ = conn.CloseWithError(0, "")
		return
	}
	_ = stream.Close()

	client := &relayServerClient{conn: conn, hello: hello, sourceIP: sourceIP, paired: make(chan struct{})}
	pair, err := s.register(client)
	if err != nil {
		s.log().Warn("relay registration rejected", "err", err)
		s.Telemetry.RecordError(err)
		_ = conn.CloseWithError(0, "relay registration rejected")
		return
	}
	if pair == nil {
		// Keep the first peer connected until either:
		//  1) a partner arrives and later closes, or
		//  2) the peer disconnects / server shuts down.
		waitTimer := time.NewTimer(s.effectivePairingWait())
		defer waitTimer.Stop()
		pairingExpired := false
		select {
		case <-ctx.Done():
		case <-conn.Context().Done():
		case <-waitTimer.C:
			pairingExpired = true
		case <-client.paired:
			select {
			case <-ctx.Done():
			case <-conn.Context().Done():
			}
		}
		if pairingExpired && !s.unregisterWaiting(client) {
			// A second peer won the lock at the timeout boundary. Keep this
			// connection alive for the pair owner instead of tearing it down.
			select {
			case <-ctx.Done():
			case <-conn.Context().Done():
			}
		} else if !pairingExpired {
			s.unregisterWaiting(client)
		}
		_ = conn.CloseWithError(0, "")
		return
	}
	s.Telemetry.PairStarted()
	defer s.Telemetry.PairFinished()
	defer pair.a.conn.CloseWithError(0, "")
	defer pair.b.conn.CloseWithError(0, "")

	ctxPair, cancel := context.WithTimeout(ctx, s.effectivePairLifetime())
	defer cancel()
	go func() {
		select {
		case <-pair.a.conn.Context().Done():
		case <-pair.b.conn.Context().Done():
		case <-ctxPair.Done():
		}
		cancel()
	}()

	// Notify both sides concurrently so one stalled peer cannot block the other.
	readyCtx, cancelReady := context.WithTimeout(ctxPair, relayReadyTimeout)
	readyResults := make(chan error, 2)
	go func() { readyResults <- sendRelayReady(readyCtx, pair.a.conn) }()
	go func() { readyResults <- sendRelayReady(readyCtx, pair.b.conn) }()
	for i := 0; i < 2; i++ {
		if err := <-readyResults; err != nil {
			cancelReady()
			s.Telemetry.RecordError(err)
			_ = pair.a.conn.CloseWithError(0, "relay readiness failed")
			_ = pair.b.conn.CloseWithError(0, "relay readiness failed")
			return
		}
	}
	cancelReady()

	activity := make(chan struct{}, 1)
	signalRelayActivity(activity)
	go mirrorBidi(ctxPair, pair.a.conn, pair.b.conn, s.Telemetry, activity)
	go mirrorBidi(ctxPair, pair.b.conn, pair.a.conn, s.Telemetry, activity)
	go mirrorUni(ctxPair, pair.a.conn, pair.b.conn, s.Telemetry, activity)
	go mirrorUni(ctxPair, pair.b.conn, pair.a.conn, s.Telemetry, activity)
	idleTimer := time.NewTimer(s.effectivePairIdleTimeout())
	defer idleTimer.Stop()

	for {
		select {
		case <-ctxPair.Done():
			_ = pair.a.conn.CloseWithError(0, "relay session expired")
			_ = pair.b.conn.CloseWithError(0, "relay session expired")
			return
		case <-pair.a.conn.Context().Done():
			_ = pair.b.conn.CloseWithError(0, "relay peer closed")
			return
		case <-pair.b.conn.Context().Done():
			_ = pair.a.conn.CloseWithError(0, "relay peer closed")
			return
		case <-activity:
			resetRelayIdleTimer(idleTimer, s.effectivePairIdleTimeout())
		case <-idleTimer.C:
			_ = pair.a.conn.CloseWithError(0, "relay application idle timeout")
			_ = pair.b.conn.CloseWithError(0, "relay application idle timeout")
			return
		}
	}
}

// admitConnection reserves capacity for one live relay peer, including peers
// that immediately self-pair instead of remaining in the waiting map.
func (s *RelayServer) admitConnection(sourceIP string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil {
		s.live = make(map[string]int)
	}
	if s.liveN >= s.effectiveLiveLimit() || s.live[sourceIP] >= s.effectiveLivePerIPLimit() {
		return false
	}
	s.live[sourceIP]++
	s.liveN++
	return true
}

// releaseConnection returns one live relay peer's admission capacity.
func (s *RelayServer) releaseConnection(sourceIP string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live[sourceIP] > 1 {
		s.live[sourceIP]--
	} else {
		delete(s.live, sourceIP)
	}
	if s.liveN > 0 {
		s.liveN--
	}
}

type relayPair struct {
	a *relayServerClient
	b *relayServerClient
}

// register retains a bounded first peer or returns a mutually authenticated relay pair.
func (s *RelayServer) register(client *relayServerClient) (*relayPair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiting == nil {
		s.waiting = make(map[string]*relayServerClient)
	}
	if other, ok := s.waiting[client.hello.Session]; ok {
		alias := relaySessionAlias(client.hello.Session)
		if other.hello.Role == client.hello.Role || !relayTokensEqual(other.hello.Token, client.hello.Token) {
			s.log().Warn("relay peer mismatch", "session", alias)
			return nil, errRelayPeerMismatch
		}
		delete(s.waiting, client.hello.Session)
		if other.paired != nil {
			close(other.paired)
		}
		s.Telemetry.SetWaitingPeers(len(s.waiting))
		s.log().Info("relay paired", "session", alias)
		return &relayPair{a: other, b: client}, nil
	}
	if len(s.waiting) >= s.effectiveWaitingLimit() || s.waitingForSource(client.sourceIP) >= s.effectiveWaitingPerIPLimit() {
		return nil, errRelayCapacity
	}
	s.waiting[client.hello.Session] = client
	s.Telemetry.SetWaitingPeers(len(s.waiting))
	s.log().Info("relay waiting", "session", relaySessionAlias(client.hello.Session))
	return nil, nil
}

// effectiveWaitingLimit returns the configured global waiting-peer limit.
func (s *RelayServer) effectiveWaitingLimit() int {
	if s.waitingLimit > 0 {
		return s.waitingLimit
	}
	return defaultRelayWaitingLimit
}

// effectiveWaitingPerIPLimit returns the configured waiting-peer limit for one source IP.
func (s *RelayServer) effectiveWaitingPerIPLimit() int {
	if s.waitingPerIPLimit > 0 {
		return s.waitingPerIPLimit
	}
	return defaultRelayWaitingPerIPLimit
}

// effectiveLiveLimit returns the configured global live-peer limit.
func (s *RelayServer) effectiveLiveLimit() int {
	if s.liveLimit > 0 {
		return s.liveLimit
	}
	return defaultRelayLiveLimit
}

// effectiveLivePerIPLimit returns the configured live-peer limit for one source IP.
func (s *RelayServer) effectiveLivePerIPLimit() int {
	if s.livePerIPLimit > 0 {
		return s.livePerIPLimit
	}
	return defaultRelayLivePerIPLimit
}

// effectivePairingWait returns the absolute time a first peer may occupy a waiting slot.
func (s *RelayServer) effectivePairingWait() time.Duration {
	if s.pairingWait > 0 {
		return s.pairingWait
	}
	return relayPairingWaitTimeout
}

// effectivePairIdleTimeout returns the maximum interval without relayed bytes.
func (s *RelayServer) effectivePairIdleTimeout() time.Duration {
	if s.PairIdleTimeout > 0 {
		return s.PairIdleTimeout
	}
	return defaultRelayPairIdleTimeout
}

// effectivePairLifetime returns the absolute lifetime of one paired session.
func (s *RelayServer) effectivePairLifetime() time.Duration {
	if s.PairLifetime > 0 {
		return s.PairLifetime
	}
	return defaultRelayPairLifetime
}

// waitingForSource counts retained peers from one source while s.mu is held.
func (s *RelayServer) waitingForSource(sourceIP string) int {
	count := 0
	for _, waiting := range s.waiting {
		if waiting.sourceIP == sourceIP {
			count++
		}
	}
	return count
}

// relaySourceIP extracts a stable source IP without trusting forwarded metadata.
func relaySourceIP(addr net.Addr) string {
	if addr == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "unknown"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "unknown"
	}
	return ip.String()
}

// unregisterWaiting removes client only if it still owns its session's waiting slot.
func (s *RelayServer) unregisterWaiting(client *relayServerClient) bool {
	if client == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.waiting[client.hello.Session]
	if ok && existing == client {
		delete(s.waiting, client.hello.Session)
		s.Telemetry.SetWaitingPeers(len(s.waiting))
		return true
	}
	return false
}

// sendRelayReady opens one bounded stream carrying the pair-ready notification.
func sendRelayReady(ctx context.Context, conn *quic.Conn) error {
	us, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}
	return writeRelayReady(ctx, us)
}

// writeRelayReady sends and closes one context-bounded readiness stream.
func writeRelayReady(ctx context.Context, stream relayWriteStream) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetWriteDeadline(deadline); err != nil {
			return err
		}
	}
	stopCancel := context.AfterFunc(ctx, func() {
		stream.CancelWrite(0)
	})
	defer stopCancel()
	if err := json.NewEncoder(stream).Encode(map[string]string{"status": "ready"}); err != nil {
		_ = stream.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return stream.Close()
}

// mirrorBidi forwards bidirectional streams and reports application activity.
func mirrorBidi(ctx context.Context, src, dst *quic.Conn, telemetry *ServiceTelemetry, activity chan<- struct{}) {
	for {
		stream, err := src.AcceptStream(ctx)
		if err != nil {
			return
		}
		signalRelayActivity(activity)
		target, err := dst.OpenStreamSync(ctx)
		if err != nil {
			stream.CancelRead(0)
			return
		}
		go proxyStream(stream, target, telemetry, activity)
	}
}

// mirrorUni forwards unidirectional streams and reports application activity.
func mirrorUni(ctx context.Context, src, dst *quic.Conn, telemetry *ServiceTelemetry, activity chan<- struct{}) {
	for {
		us, err := src.AcceptUniStream(ctx)
		if err != nil {
			return
		}
		ds, err := dst.OpenUniStreamSync(ctx)
		if err != nil {
			us.CancelRead(0)
			return
		}
		signalRelayActivity(activity)
		go func(r *quic.ReceiveStream, w *quic.SendStream) {
			n, _ := io.Copy(relayActivityWriter{writer: w, activity: activity}, r)
			telemetry.AddRelayBytes(n)
			_ = w.Close()
		}(us, ds)
	}
}

// proxyStream copies both halves of one bidirectional stream.
func proxyStream(a *quic.Stream, b *quic.Stream, telemetry *ServiceTelemetry, activity chan<- struct{}) {
	go func() {
		n, _ := io.Copy(relayActivityWriter{writer: b, activity: activity}, a)
		telemetry.AddRelayBytes(n)
		_ = b.Close()
	}()
	n, _ := io.Copy(relayActivityWriter{writer: a, activity: activity}, b)
	telemetry.AddRelayBytes(n)
	_ = a.Close()
}

type relayActivityWriter struct {
	writer   io.Writer
	activity chan<- struct{}
}

// Write forwards bytes and signals only successful application traffic.
func (w relayActivityWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		signalRelayActivity(w.activity)
	}
	return n, err
}

// signalRelayActivity coalesces relay activity without blocking stream copies.
func signalRelayActivity(activity chan<- struct{}) {
	if activity == nil {
		return
	}
	select {
	case activity <- struct{}{}:
	default:
	}
}

// resetRelayIdleTimer safely restarts an owned idle timer.
func resetRelayIdleTimer(timer *time.Timer, timeout time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(timeout)
}

func (s *RelayServer) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
