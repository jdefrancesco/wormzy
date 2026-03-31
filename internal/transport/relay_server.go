package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// RelayServer relays QUIC streams between paired clients that share a code+token.
// Payloads stay Noise-encrypted end-to-end; the server only forwards bytes.
type RelayServer struct {
	Addr   string
	Logger *slog.Logger

	mu      sync.Mutex
	waiting map[string]*relayServerClient
}

type relayServerClient struct {
	conn  *quic.Conn
	hello relayHello
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
	defer udpConn.Close()

	tlsConf, err := selfSignedTLS()
	if err != nil {
		return err
	}
	tlsConf.NextProtos = []string{alpn}
	quicConf := &quic.Config{
		KeepAlivePeriod: 15 * time.Second,
		MaxIdleTimeout:  2 * time.Minute,
	}
	transport := &quic.Transport{Conn: udpConn}
	ln, err := transport.Listen(tlsConf, quicConf)
	if err != nil {
		return err
	}
	s.log().Info("relay listening", "addr", addr)

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log().Warn("relay accept", "err", err)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *RelayServer) handleConn(ctx context.Context, conn *quic.Conn) {
	defer conn.CloseWithError(0, "")
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return
	}
	var hello relayHello
	if err := json.NewDecoder(stream).Decode(&hello); err != nil {
		s.log().Warn("relay hello decode", "err", err)
		return
	}

	client := &relayServerClient{conn: conn, hello: hello}
	pair := s.register(client)
	if pair == nil {
		return
	}

	ctxPair, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-pair.a.conn.Context().Done():
		case <-pair.b.conn.Context().Done():
		case <-ctxPair.Done():
		}
		cancel()
	}()

	// Notify both sides that the relay tunnel is ready.
	_ = sendRelayReady(ctxPair, pair.a.conn)
	_ = sendRelayReady(ctxPair, pair.b.conn)

	go mirrorBidi(ctxPair, pair.a.conn, pair.b.conn)
	go mirrorBidi(ctxPair, pair.b.conn, pair.a.conn)
	go mirrorUni(ctxPair, pair.a.conn, pair.b.conn)
	go mirrorUni(ctxPair, pair.b.conn, pair.a.conn)

	<-ctxPair.Done()
}

type relayPair struct {
	a *relayServerClient
	b *relayServerClient
}

func (s *RelayServer) register(client *relayServerClient) *relayPair {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiting == nil {
		s.waiting = make(map[string]*relayServerClient)
	}
	if other, ok := s.waiting[client.hello.Code]; ok {
		if other.hello.Token != client.hello.Token {
			_ = other.conn.CloseWithError(0, "relay token mismatch")
			_ = client.conn.CloseWithError(0, "relay token mismatch")
			delete(s.waiting, client.hello.Code)
			return nil
		}
		delete(s.waiting, client.hello.Code)
		s.log().Info("relay paired", "code", client.hello.Code)
		return &relayPair{a: other, b: client}
	}
	s.waiting[client.hello.Code] = client
	s.log().Info("relay waiting", "code", client.hello.Code)
	return nil
}

func sendRelayReady(ctx context.Context, conn *quic.Conn) error {
	us, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}
	defer us.Close()
	return json.NewEncoder(us).Encode(map[string]string{"status": "ready"})
}

func mirrorBidi(ctx context.Context, src, dst *quic.Conn) {
	for {
		stream, err := src.AcceptStream(ctx)
		if err != nil {
			return
		}
		target, err := dst.OpenStreamSync(ctx)
		if err != nil {
			stream.CancelRead(0)
			return
		}
		go proxyStream(stream, target)
	}
}

func mirrorUni(ctx context.Context, src, dst *quic.Conn) {
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
		go func(r *quic.ReceiveStream, w *quic.SendStream) {
			_, _ = io.Copy(w, r)
			_ = w.Close()
		}(us, ds)
	}
}

func proxyStream(a *quic.Stream, b *quic.Stream) {
	go func() {
		_, _ = io.Copy(b, a)
		_ = b.Close()
	}()
	_, _ = io.Copy(a, b)
	_ = a.Close()
}

func (s *RelayServer) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
