package transport

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

type pathTransferResult struct {
	mode string
	res  *Result
	err  error
}

// TestICEQUICTransferLoopback exercises ICE, QUIC, PAKE confirmation, Noise,
// encrypted file transfer, integrity verification, and completion receipts.
func TestICEQUICTransferLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	srcData, srcPath, recvDir := preparePathTransferFiles(t, "ice.bin")
	toReceiver := make(chan mailboxMessage, 8)
	toSender := make(chan mailboxMessage, 8)
	mailboxes := map[string]mailbox{
		"send": &iceTestMailbox{outbound: toReceiver, inbound: toSender},
		"recv": &iceTestMailbox{outbound: toSender, inbound: toReceiver},
	}
	psk := bytes.Repeat([]byte{0x5a}, 32)
	peer := rendezvous.SelfInfo{Features: []string{featureICEv1, featureTransferCompletionV1}}
	results := make(chan pathTransferResult, 2)

	for _, mode := range []string{"send", "recv"} {
		mode := mode
		go func() {
			cfg := pathTransferConfig(mode, srcPath, recvDir)
			session, err := attemptICEQUICSession(
				ctx, cfg, mailboxes[mode], tReporter{t}, peer, testPairingCode, psk, nil,
			)
			if err != nil {
				results <- pathTransferResult{mode: mode, err: err}
				return
			}
			defer session.cleanup()
			stats := transferStats{Mode: mode, Transport: "p2p", Candidate: "ice-p2p", DirectOutcome: "won"}
			res, err := transferEstablishedQUICSession(
				ctx, cfg, tReporter{t}, session.conn, session.initiated, false,
				nil, rendezvous.SelfInfo{}, peer, testPairingCode, psk, &stats,
			)
			results <- pathTransferResult{mode: mode, res: res, err: err}
		}()
	}

	assertPathTransferResults(t, results, srcData, "ice-p2p", "p2p")
}

// TestRelayQUICTransferLoopback exercises a live RelayServer and the complete
// end-to-end encrypted transfer protocol over its mirrored QUIC streams.
func TestRelayQUICTransferLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen relay UDP: %v", err)
	}
	serverCtx, stopServer := context.WithCancel(ctx)
	defer stopServer()
	serverDone := make(chan error, 1)
	server := &RelayServer{PairIdleTimeout: 5 * time.Second, PairLifetime: time.Minute}
	go func() { serverDone <- server.serve(serverCtx, udpConn) }()

	srcData, srcPath, recvDir := preparePathTransferFiles(t, "relay.bin")
	psk := bytes.Repeat([]byte{0x7b}, 32)
	peer := rendezvous.SelfInfo{Features: []string{featureTransferCompletionV1}}
	results := make(chan pathTransferResult, 2)
	relayAddr := udpConn.LocalAddr().String()

	for _, mode := range []string{"send", "recv"} {
		mode := mode
		go func() {
			cfg := pathTransferConfig(mode, srcPath, recvDir)
			conn, transport, err := dialRelay(ctx, relayAddr, cfg)
			if err != nil {
				results <- pathTransferResult{mode: mode, err: err}
				return
			}
			defer transport.Conn.Close()
			defer conn.CloseWithError(0, "test complete")
			if err := registerRelay(ctx, conn, mode, psk); err != nil {
				results <- pathTransferResult{mode: mode, err: err}
				return
			}
			initiated := mode == "send"
			if err := confirmQUICPeer(ctx, conn, initiated, psk); err != nil {
				results <- pathTransferResult{mode: mode, err: err}
				return
			}
			stats := transferStats{Mode: mode, Transport: "relay", Candidate: "relay", DirectOutcome: "no-response"}
			res, err := transferEstablishedQUICSession(
				ctx, cfg, tReporter{t}, conn, initiated, true,
				nil, rendezvous.SelfInfo{}, peer, testPairingCode, psk, &stats,
			)
			results <- pathTransferResult{mode: mode, res: res, err: err}
		}()
	}

	assertPathTransferResults(t, results, srcData, "relay", "relay")
	stopServer()
	select {
	case err := <-serverDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("relay server shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay server did not stop")
	}
}

// preparePathTransferFiles creates one source and destination directory for a path test.
func preparePathTransferFiles(t *testing.T, filename string) ([]byte, string, string) {
	t.Helper()
	data := bytes.Repeat([]byte("wormzy-path-integration\n"), 128)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, filename)
	if err := os.WriteFile(srcPath, data, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	recvDir := filepath.Join(dir, "recv")
	if err := os.Mkdir(recvDir, 0o700); err != nil {
		t.Fatalf("create receive directory: %v", err)
	}
	return data, srcPath, recvDir
}

// pathTransferConfig returns deterministic local settings without public STUN or TURN traffic.
func pathTransferConfig(mode, srcPath, recvDir string) Config {
	return Config{
		Mode:             mode,
		FilePath:         srcPath,
		DownloadDir:      recvDir,
		Loopback:         true,
		STUNServers:      []string{" "},
		TURNServers:      []string{" "},
		HandshakeTimeout: 10 * time.Second,
		IdleTimeout:      10 * time.Second,
	}
}

// assertPathTransferResults checks both roles and verifies the received bytes on disk.
func assertPathTransferResults(
	t *testing.T,
	results <-chan pathTransferResult,
	wantData []byte,
	wantCandidate string,
	wantTransport string,
) {
	t.Helper()
	var recvResult *Result
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s transfer: %v", result.mode, result.err)
		}
		if result.res == nil {
			t.Fatalf("%s transfer returned no result", result.mode)
		}
		if result.res.Candidate != wantCandidate || result.res.Transport != wantTransport {
			t.Fatalf("%s path = %s/%s; want %s/%s", result.mode, result.res.Transport, result.res.Candidate, wantTransport, wantCandidate)
		}
		if result.mode == "recv" {
			recvResult = result.res
		}
	}
	if recvResult == nil {
		t.Fatal("missing receiver result")
	}
	got, err := os.ReadFile(recvResult.FilePath)
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if !bytes.Equal(got, wantData) {
		t.Fatal("received file content mismatch")
	}
}
