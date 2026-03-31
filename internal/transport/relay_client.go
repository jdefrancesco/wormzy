package transport

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/zeebo/blake3"
)

const relayPrologue = "wormzy-relay-v1"

type relayHello struct {
	Code  string `json:"code"`
	Token string `json:"token"`
	Role  string `json:"role"`
}

func deriveRelayToken(psk []byte) [32]byte {
	return blake3.Sum256(append([]byte(relayPrologue), psk...))
}

func dialRelay(ctx context.Context, addr string, cfg Config) (*quic.Conn, *quic.Transport, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, nil, err
	}
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, nil, err
	}
	transport := &quic.Transport{Conn: udpConn}
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}}
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

func registerRelay(ctx context.Context, conn *quic.Conn, code, role string, psk []byte) error {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	token := deriveRelayToken(psk)
	hello := relayHello{Code: code, Token: hex.EncodeToString(token[:]), Role: role}
	if err := json.NewEncoder(stream).Encode(&hello); err != nil {
		return err
	}
	return waitRelayReady(ctx, conn)
}

func waitRelayReady(ctx context.Context, conn *quic.Conn) error {
	type readyMsg struct {
		Status string `json:"status"`
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		us, err := conn.AcceptUniStream(ctx)
		if err != nil {
			return err
		}
		var msg readyMsg
		if err := json.NewDecoder(us).Decode(&msg); err != nil {
			return err
		}
		us.CancelRead(0)
		if msg.Status == "ready" {
			return nil
		}
		if msg.Status == "" {
			return fmt.Errorf("relay response missing status")
		}
	}
}
