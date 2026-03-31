package transport

import (
	"bytes"
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	mrand "math/rand"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	cpace "filippo.io/cpace"
	"github.com/flynn/noise"
	"github.com/jdefrancesco/wormzy/internal/rendezvous"
	"github.com/jdefrancesco/wormzy/internal/stun"
	"github.com/quic-go/quic-go"
	"github.com/zeebo/blake3"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	alpn = "p2p-wormzy-1"
	// defaultRelay is the baked-in rendezvous/mailbox endpoint. Users can override
	// via CLI flag or environment (WORMZY_RELAY_URL / WORMZY_RELAY).
	defaultRelay          = "https://relay.wormzy.io"
	defaultRelayUDPPort   = 3478
	defaultHandshakeTO    = 90 * time.Second
	defaultTransferIdleTO = 5 * time.Minute
	relayFallbackDelay    = 4 * time.Second

	// Wire-format sizing limits.
	maxUint16PayloadLen = (1 << 16) - 1

	// File header layout: uint16(nameLen) + uint64(fileSize) + name bytes.
	fileHeaderNameLenSize = 2
	fileHeaderSizeSize    = 8
	fileHeaderFixedLen    = fileHeaderNameLenSize + fileHeaderSizeSize
)

// Config controls how a Wormzy transfer session behaves.
type Config struct {
	Mode             string
	FilePath         string
	Code             string
	RelayAddr        string
	RelayPin         string
	STUNServers      []string
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	Loopback         bool
	DownloadDir      string
}

// Result reports information about the established session.
type Result struct {
	Code      string
	Peer      rendezvous.SelfInfo
	Mode      string
	FilePath  string
	FileSize  int64
	FileHash  string
	Transport string
	Candidate string
}

// Reporter receives human-readable log lines describing progress.
type Stage string

const (
	StageSTUN       Stage = "stun"
	StageRendezvous Stage = "rendezvous"
	StageQUIC       Stage = "quic"
	StageNoise      Stage = "noise"
	StageTransfer   Stage = "transfer"
)

type StageState int

const (
	StageStatePending StageState = iota
	StageStateRunning
	StageStateDone
	StageStateError
)

type Reporter interface {
	Logf(format string, args ...interface{})
	Stage(stage Stage, state StageState, detail string)
}

// ReporterFunc adapts a function into a Reporter with no-op stage updates.
type ReporterFunc func(format string, args ...interface{})

func (f ReporterFunc) Logf(format string, args ...interface{}) {
	if f == nil {
		return
	}
	f(format, args...)
}

func (f ReporterFunc) Stage(stage Stage, state StageState, detail string) {}

// Run executes a full rendezvous + NAT punching flow for the configured mode.
// It performs STUN discovery, rendezvous via the mailbox, Noise+QUIC handshake,
// and then streams the file either as sender or receiver. The returned Result
// includes session metadata and transfer stats.
func Run(ctx context.Context, cfg Config, rep Reporter) (res *Result, finalErr error) {
	reporter := rep
	if reporter == nil {
		reporter = ReporterFunc(func(string, ...interface{}) {})
	}
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	started := time.Now()

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer udpConn.Close()
	reporter.Logf("udp/listen %s", udpConn.LocalAddr())

	self := rendezvous.SelfInfo{Local: localEndpoint(udpConn)}
	if cfg.Loopback {
		if addr, ok := udpConn.LocalAddr().(*net.UDPAddr); ok {
			self.Local = net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
		}
	}
	if cfg.Loopback {
		self.Public = self.Local
		reporter.Stage(StageSTUN, StageStateDone, "loopback")
	} else {
		ctxStun, cancelStun := context.WithTimeout(ctx, cfg.HandshakeTimeout)
		reporter.Stage(StageSTUN, StageStateRunning, "probing reflexive address")
		pub, err := stun.DiscoverOnConn(ctxStun, udpConn, cfg.stunServers(), 2*time.Second, 2)
		cancelStun()
		if err != nil {
			reporter.Stage(StageSTUN, StageStateError, err.Error())
			reporter.Logf("stun discovery failed: %v", err)
		} else {
			self.Public = pub.String()
			reporter.Stage(StageSTUN, StageStateDone, self.Public)
			reporter.Logf("public address %s", self.Public)
		}
		if self.Public == "" {
			self.Public = self.Local
		}
	}
	self.Candidates = buildCandidates(self, cfg.Loopback, cfg.relayCandidateAddr())

	mbox, err := newMailbox(ctx, cfg)
	if err != nil {
		finalErr = err
		return nil, err
	}
	defer mbox.Close()

	reporter.Stage(StageRendezvous, StageStateRunning, "dialing relay")
	peer, code, psk, err := rendezvousExchange(ctx, cfg, self, reporter, mbox)
	if err != nil {
		reporter.Stage(StageRendezvous, StageStateError, err.Error())
		finalErr = err
		return nil, err
	}
	chosen, relayCand, err := selectPeerCandidate(self, peer, cfg.Loopback)
	if err != nil {
		reporter.Stage(StageRendezvous, StageStateError, err.Error())
		return nil, err
	}
	reporter.Stage(StageRendezvous, StageStateDone, fmt.Sprintf("%s (%s)", chosen.Addr, chosen.Type))
	reporter.Logf("paired with code %s", code)

	stats := transferStats{
		Mode:      cfg.Mode,
		Candidate: chosen.Type,
		Transport: transportLabelForCandidate(chosen),
	}
	defer func() {
		if mbox == nil {
			return
		}
		stats.Completed = finalErr == nil
		if finalErr != nil {
			stats.Error = finalErr.Error()
		}
		stats.DurationMillis = time.Since(started).Milliseconds()
		if res != nil {
			stats.Bytes = res.FileSize
		}
		if err := mbox.ReportStats(ctx, stats); err != nil {
			reporter.Logf("report stats failed: %v", err)
		}
	}()

	peerAddr := chosen.Addr
	peerUDP, err := net.ResolveUDPAddr("udp4", peerAddr)
	if err != nil {
		return nil, err
	}

	punchCtx, cancelPunch := context.WithCancel(ctx)
	defer cancelPunch()
	stopPunch := make(chan struct{})
	var punchWG sync.WaitGroup
	punchWG.Add(1)
	go func() {
		defer punchWG.Done()
		punchLoop(punchCtx, udpConn, peerUDP, stopPunch)
	}()

	quicTransport := &quic.Transport{Conn: udpConn}
	serverTLS, err := selfSignedTLS()
	if err != nil {
		return nil, err
	}
	serverTLS.NextProtos = []string{alpn}
	clientTLS := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}}
	quicConf := &quic.Config{
		KeepAlivePeriod:      15 * time.Second,
		MaxIdleTimeout:       cfg.IdleTimeout,
		HandshakeIdleTimeout: cfg.HandshakeTimeout,
	}

	reporter.Stage(StageQUIC, StageStateRunning, "punching + dialing")
	ln, err := quicTransport.Listen(serverTLS, quicConf)
	if err != nil {
		return nil, err
	}
	defer ln.Close()

	type quicResult struct {
		conn      *quic.Conn
		initiated bool
		err       error
	}
	resCh := make(chan quicResult, 4)
	ctxConn, cancelConn := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	defer cancelConn()

	// Accept path
	go func() {
		conn, err := ln.Accept(ctxConn)
		resCh <- quicResult{conn: conn, initiated: false, err: err}
	}()

	launchDial := func(delay time.Duration) {
		go func() {
			if delay > 0 {
				time.Sleep(delay)
			}
			conn, err := quicTransport.Dial(ctxConn, peerUDP, clientTLS, quicConf)
			resCh <- quicResult{conn: conn, initiated: true, err: err}
		}()
	}
	if cfg.Mode == "send" {
		launchDial(200 * time.Millisecond)
	} else {
		launchDial(0)
	}
	// simultaneous-open retry
	launchDial(500 * time.Millisecond)

	var quicConn *quic.Conn
	initiated := cfg.Mode == "recv"
	usedCandidate := chosen
	var firstErr error
	relayAttempted := false
	var relayTransport *quic.Transport
	fallbackTimer := time.NewTimer(relayFallbackDelay)
	defer fallbackTimer.Stop()

waitLoop:
	for quicConn == nil {
		select {
		case res := <-resCh:
			if res.err == nil && res.conn != nil {
				quicConn = res.conn
				initiated = res.initiated
				break waitLoop
			}
			if firstErr == nil {
				firstErr = res.err
			}
		case <-fallbackTimer.C:
			if relayCand != nil && !relayAttempted {
				relayAttempted = true
				cancelConn()
				reporter.Logf("falling back to relay %s", relayCand.Addr)
				reporter.Stage(StageQUIC, StageStateRunning, "relay fallback")
				relayCtx, cancelRelay := context.WithTimeout(ctx, cfg.HandshakeTimeout)
				rConn, rTransport, err := dialRelay(relayCtx, relayCand.Addr, cfg)
				if err == nil {
					if err := registerRelay(relayCtx, rConn, code, cfg.Mode, psk); err == nil {
						quicConn = rConn
						relayTransport = rTransport
						initiated = cfg.Mode == "send"
						usedCandidate = *relayCand
					} else {
						_ = rConn.CloseWithError(0, err.Error())
					}
				}
				cancelRelay()
			}
		case <-ctxConn.Done():
			if quicConn != nil {
				break waitLoop
			}
			if firstErr == nil {
				firstErr = ctxConn.Err()
			}
			if relayAttempted && relayCand != nil {
				reporter.Stage(StageQUIC, StageStateError, firstErr.Error())
				return nil, firstErr
			}
			reporter.Stage(StageQUIC, StageStateError, firstErr.Error())
			return nil, firstErr
		}
	}
	cancelConn()

	if quicConn == nil {
		err := firstErr
		if err == nil {
			err = fmt.Errorf("failed to establish QUIC")
		}
		reporter.Stage(StageQUIC, StageStateError, err.Error())
		return nil, err
	}

	stats.Candidate = usedCandidate.Type
	stats.Transport = transportLabelForCandidate(usedCandidate)

	close(stopPunch)
	punchWG.Wait()
	if relayTransport != nil && relayTransport.Conn != nil {
		defer relayTransport.Conn.Close()
	}

	if usedCandidate.Type == "relay" {
		reporter.Stage(StageQUIC, StageStateDone, "relay fallback")
	} else if initiated {
		reporter.Logf("dialed QUIC peer %s", peerUDP)
		reporter.Stage(StageQUIC, StageStateDone, peerUDP.String())
	} else {
		reporter.Logf("accepted QUIC connection from %s", quicConn.RemoteAddr())
		reporter.Stage(StageQUIC, StageStateDone, quicConn.RemoteAddr().String())
	}

	reporter.Stage(StageNoise, StageStateRunning, "noise handshake")
	fileKey, sas, err := runNoiseOverQUIC(quicConn, initiated, psk)
	if err != nil {
		reporter.Stage(StageNoise, StageStateError, err.Error())
		return nil, err
	}
	reporter.Logf("noise handshake SAS %s", sas)
	reporter.Stage(StageNoise, StageStateDone, fmt.Sprintf("confirm SAS %s", sas))

	res = &Result{Code: code, Peer: peer, Mode: cfg.Mode}
	res.Transport = stats.Transport
	res.Candidate = stats.Candidate

	switch cfg.Mode {
	case "send":
		reporter.Stage(StageTransfer, StageStateRunning, "streaming file")
		sum, size, err := sendFileEncrypted(quicConn, cfg.FilePath, fileKey, cfg.IdleTimeout, reporter)
		if err != nil {
			reporter.Stage(StageTransfer, StageStateError, err.Error())
			return nil, err
		}
		res.FilePath = cfg.FilePath
		res.FileSize = size
		res.FileHash = hex.EncodeToString(sum)
		reporter.Logf("transfer complete")
		reporter.Stage(StageTransfer, StageStateDone, "file sent")
	case "recv":
		reporter.Stage(StageTransfer, StageStateRunning, "receiving file")
		path, sum, size, err := receiveFile(quicConn, fileKey, cfg.DownloadDir, cfg.IdleTimeout, reporter)
		if err != nil {
			reporter.Stage(StageTransfer, StageStateError, err.Error())
			return nil, err
		}
		res.FilePath = path
		res.FileSize = size
		res.FileHash = hex.EncodeToString(sum)
		reporter.Logf("saved file to %s", path)
		reporter.Stage(StageTransfer, StageStateDone, path)
	}

	return res, nil
}

func (cfg Config) withDefaults() Config {
	if cfg.RelayAddr == "" {
		cfg.RelayAddr = defaultRelay
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = defaultHandshakeTO
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultTransferIdleTO
	}
	return cfg
}

func (cfg Config) sessionTTL() time.Duration {
	ttl := cfg.HandshakeTimeout
	if cfg.IdleTimeout > ttl {
		ttl = cfg.IdleTimeout
	}
	if ttl <= 0 {
		ttl = defaultTransferIdleTO
	}
	return ttl + time.Minute
}

func (cfg Config) validate() error {
	if cfg.Mode != "send" && cfg.Mode != "recv" {
		return fmt.Errorf("mode must be send or recv")
	}
	if cfg.Mode == "send" && cfg.FilePath == "" {
		return fmt.Errorf("send mode requires a file path")
	}
	return nil
}

func (cfg Config) stunServers() []string {
	list := cfg.STUNServers
	if len(list) == 0 {
		list = append([]string{}, stun.StunServers...)
	} else {
		list = append([]string{}, cfg.STUNServers...)
	}
	src := mrand.NewSource(time.Now().UnixNano())
	r := mrand.New(src)
	r.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	return list
}

// DefaultRelay returns the compiled-in rendezvous Redis endpoint.
func DefaultRelay() string {
	return defaultRelay
}

func (cfg Config) relayCandidateAddr() string {
	if cfg.RelayAddr == "" {
		return ""
	}
	u, err := url.Parse(cfg.RelayAddr)
	if err == nil && u.Host != "" {
		host := u.Hostname()
		port := u.Port()
		if port == "" {
			port = strconv.Itoa(defaultRelayUDPPort)
		}
		return net.JoinHostPort(host, port)
	}
	// Non-URL input; if it already carries a port, trust it.
	if _, _, err := net.SplitHostPort(cfg.RelayAddr); err == nil {
		return cfg.RelayAddr
	}
	return net.JoinHostPort(cfg.RelayAddr, strconv.Itoa(defaultRelayUDPPort))
}

// rendezvousExchange coordinates code assignment, PAKE, and peer discovery over the mailbox.
func rendezvousExchange(ctx context.Context, cfg Config, me rendezvous.SelfInfo, rep Reporter, mb mailbox) (peer rendezvous.SelfInfo, assigned string, psk []byte, err error) {
	code, err := mb.Claim(ctx, cfg.Code)
	if err != nil {
		return peer, assigned, nil, friendlyRendezvousErr(err)
	}
	assigned = code
	rep.Stage(StageRendezvous, StageStateRunning, "code "+assigned)
	rep.Logf("rendezvous assigned code %s", assigned)

	if err := mb.StoreSelf(ctx, me); err != nil {
		return peer, assigned, nil, friendlyRendezvousErr(err)
	}

	psk, err = runPAKEOverMailbox(ctx, mb, cfg.Mode, assigned, "send", "recv")
	if err != nil {
		return peer, assigned, nil, friendlyRendezvousErr(err)
	}

	peerInfo, err := mb.WaitPeer(ctx)
	if err != nil {
		return peer, assigned, nil, friendlyRendezvousErr(err)
	}
	return *peerInfo, assigned, psk, nil
}

// runPAKEOverMailbox executes CPace over mailbox messages to derive a shared key.
func runPAKEOverMailbox(ctx context.Context, mb mailbox, role, code, idA, idB string) ([]byte, error) {
	ci := cpace.NewContextInfo(idA, idB, []byte("wormzy-pake-v1"))
	if role == "send" {
		msgA, st, err := cpace.Start(code, ci)
		if err != nil {
			return nil, err
		}
		if err := mb.Send(ctx, "pake1", msgA); err != nil {
			return nil, friendlyRendezvousErr(err)
		}
		m, err := mb.Receive(ctx)
		if err != nil {
			return nil, friendlyRendezvousErr(err)
		}
		if m.Type != "pake1" {
			return nil, fmt.Errorf("expected pake1, got %s", m.Type)
		}
		var msgB []byte
		if err := json.Unmarshal(m.Body, &msgB); err != nil {
			return nil, err
		}
		keyA, err := st.Finish(msgB)
		if err != nil {
			return nil, err
		}
		if err := mb.Send(ctx, "pake2", []byte{}); err != nil {
			return nil, err
		}
		return keyA, nil
	}

	m, err := mb.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if m.Type != "pake1" {
		return nil, fmt.Errorf("expected pake1, got %s", m.Type)
	}
	var msgA []byte
	if err := json.Unmarshal(m.Body, &msgA); err != nil {
		return nil, err
	}
	msgB, keyB, err := cpace.Exchange(code, ci, msgA)
	if err != nil {
		return nil, err
	}
	if err := mb.Send(ctx, "pake1", msgB); err != nil {
		return nil, friendlyRendezvousErr(err)
	}
	resp, err := mb.Receive(ctx)
	if err != nil {
		return nil, friendlyRendezvousErr(err)
	}
	if resp.Type != "pake2" {
		return nil, fmt.Errorf("expected pake2, got %s", resp.Type)
	}
	return keyB, nil
}

func friendlyRendezvousErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errSenderMissing):
		return fmt.Errorf("code not found (did the sender start a fresh session?)")
	case errors.Is(err, errSessionNotFound):
		return fmt.Errorf("code not found or expired; ask the sender for a new code")
	default:
		return err
	}
}

// runNoiseOverQUIC performs the Noise NN handshake over a QUIC stream and returns
// the derived file key plus a short authentication string for human verification.
func runNoiseOverQUIC(conn *quic.Conn, initiator bool, psk []byte) ([]byte, string, error) {
	var stream *quic.Stream
	var err error
	ctx := context.Background()
	if initiator {
		stream, err = conn.OpenStreamSync(ctx)
	} else {
		stream, err = conn.AcceptStream(ctx)
	}
	if err != nil {
		return nil, "", err
	}
	defer stream.Close()

	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	hs, err := noise.NewHandshakeState(noise.Config{
		Pattern:     noise.HandshakeNN,
		Initiator:   initiator,
		CipherSuite: suite,
		Prologue:    []byte("wormzy-noise-v1"),
		Random:      crand.Reader,
	})
	if err != nil {
		return nil, "", err
	}

	writeFrame := func(b []byte) error {
		if len(b) > maxUint16PayloadLen {
			return fmt.Errorf("noise frame too large")
		}
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
		if _, err := stream.Write(hdr[:]); err != nil {
			return err
		}
		_, err := stream.Write(b)
		return err
	}
	readFrame := func() ([]byte, error) {
		var ln uint16
		if err := binary.Read(stream, binary.BigEndian, &ln); err != nil {
			return nil, err
		}
		buf := make([]byte, ln)
		_, err := io.ReadFull(stream, buf)
		return buf, err
	}

	var transcript []byte
	appendTranscript := func(b []byte) { transcript = append(transcript, b...) }

	if initiator {
		msg1, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, "", err
		}
		appendTranscript(msg1)
		if err := writeFrame(msg1); err != nil {
			return nil, "", err
		}

		in2, err := readFrame()
		if err != nil {
			return nil, "", err
		}
		appendTranscript(in2)
		if _, _, _, err := hs.ReadMessage(nil, in2); err != nil {
			return nil, "", err
		}
	} else {
		in1, err := readFrame()
		if err != nil {
			return nil, "", err
		}
		appendTranscript(in1)
		if _, _, _, err := hs.ReadMessage(nil, in1); err != nil {
			return nil, "", err
		}

		msg2, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, "", err
		}
		appendTranscript(msg2)
		if err := writeFrame(msg2); err != nil {
			return nil, "", err
		}
	}

	th := sha256.Sum256(transcript)
	fileKey := make([]byte, chacha20poly1305.KeySize)
	kdf := hkdf.New(sha256.New, psk, th[:], []byte("wormzy-filekey-v1"))
	if _, err := io.ReadFull(kdf, fileKey); err != nil {
		return nil, "", err
	}
	sas := deriveSAS(transcript, psk)
	return fileKey, sas, nil
}

type cipherAEAD interface {
	Seal(dst, nonce, plaintext, ad []byte) []byte
	Open(dst, nonce, ciphertext, ad []byte) ([]byte, error)
	NonceSize() int
}

type aeadWriter struct {
	w         io.Writer
	aead      cipherAEAD
	baseNonce [24]byte
	ctr       uint64
}

type aeadReader struct {
	r         io.Reader
	aead      cipherAEAD
	baseNonce [24]byte
	ctr       uint64
}

type fileMetadata struct {
	Hash      string `json:"hash"`
	ChunkSize uint32 `json:"chunk"`
	Size      uint64 `json:"size"`
	Digest    []byte `json:"digest"`
}

func makeNonce(base [24]byte, ctr uint64) []byte {
	b := base
	for i := 0; i < 8; i++ {
		b[23-i] ^= byte(ctr >> (8 * i))
	}
	return b[:]
}

func (w *aeadWriter) WriteChunk(p []byte) error {
	n := makeNonce(w.baseNonce, w.ctr)
	ct := w.aead.Seal(nil, n, p, nil)
	if err := binary.Write(w.w, binary.BigEndian, uint32(len(ct))); err != nil {
		return err
	}
	if _, err := w.w.Write(ct); err != nil {
		return err
	}
	w.ctr++
	return nil
}

func (r *aeadReader) ReadChunk() ([]byte, error) {
	var ln uint32
	if err := binary.Read(r.r, binary.BigEndian, &ln); err != nil {
		return nil, err
	}
	ct := make([]byte, ln)
	if _, err := io.ReadFull(r.r, ct); err != nil {
		return nil, err
	}
	n := makeNonce(r.baseNonce, r.ctr)
	pt, err := r.aead.Open(nil, n, ct, nil)
	if err != nil {
		return nil, err
	}
	r.ctr++
	return pt, nil
}

// sendFileEncrypted streams a file over QUIC with per-chunk XChaCha20-Poly1305
// encryption, enforcing idle timeouts and reporting progress.
func sendFileEncrypted(conn *quic.Conn, path string, key []byte, idle time.Duration, rep Reporter) ([]byte, int64, error) {
	if idle <= 0 {
		idle = defaultTransferIdleTO
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	if fi.IsDir() {
		return nil, 0, fmt.Errorf("path %s is a directory", path)
	}
	name := filepath.Base(path)
	if len(name) > maxUint16PayloadLen {
		return nil, 0, fmt.Errorf("filename too long")
	}
	size := fi.Size()

	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	us, err := conn.OpenUniStreamSync(context.Background())
	if err != nil {
		return nil, 0, err
	}
	defer us.Close()

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, 0, err
	}
	var base [24]byte
	if _, err := crand.Read(base[:]); err != nil {
		return nil, 0, err
	}
	if _, err := us.Write(base[:]); err != nil {
		return nil, 0, err
	}
	writer := &aeadWriter{w: us, aead: aead, baseNonce: base}
	setWriteDeadline := func() {
		_ = us.SetWriteDeadline(time.Now().Add(idle))
	}
	clearDeadline := func() {
		_ = us.SetWriteDeadline(time.Time{})
	}
	setWriteDeadline()
	defer clearDeadline()

	header := make([]byte, fileHeaderFixedLen+len(name))
	binary.LittleEndian.PutUint16(header[0:fileHeaderNameLenSize], uint16(len(name)))
	binary.LittleEndian.PutUint64(header[fileHeaderNameLenSize:fileHeaderFixedLen], uint64(size))
	copy(header[fileHeaderFixedLen:], []byte(name))
	if err := writer.WriteChunk(header); err != nil {
		return nil, 0, err
	}

	hasher := blake3.New()
	buf := make([]byte, chunkSize)
	var sent int64
	lastPct := -1

	for {
		n, er := file.Read(buf)
		if n > 0 {
			if _, err := hasher.Write(buf[:n]); err != nil {
				return nil, 0, err
			}
			setWriteDeadline()
			if err := writer.WriteChunk(buf[:n]); err != nil {
				return nil, 0, err
			}
			sent += int64(n)
			reportTransferProgress(rep, "Sending", sent, size, &lastPct)
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			return nil, 0, er
		}
	}
	// Ensure we report 100% once data is flushed.
	reportTransferProgress(rep, "Sending", size, size, &lastPct)
	meta := fileMetadata{
		Hash:      "blake3-256",
		ChunkSize: uint32(chunkSize),
		Size:      uint64(size),
		Digest:    hasher.Sum(nil),
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return nil, 0, err
	}
	if err := writer.WriteChunk(append([]byte(metaPrefix), payload...)); err != nil {
		return nil, 0, err
	}
	return meta.Digest, size, nil
}

// receiveFile pulls the encrypted stream, writes it to disk with collision-safe
// naming, verifies the metadata trailer, and reports progress.
func receiveFile(conn *quic.Conn, key []byte, downloadDir string, idle time.Duration, rep Reporter) (string, []byte, int64, error) {
	if idle <= 0 {
		idle = defaultTransferIdleTO
	}
	stream, err := conn.AcceptUniStream(context.Background())
	if err != nil {
		return "", nil, 0, err
	}
	defer stream.CancelRead(0)

	var base [24]byte
	if _, err := io.ReadFull(stream, base[:]); err != nil {
		return "", nil, 0, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", nil, 0, err
	}
	reader := &aeadReader{r: stream, aead: aead, baseNonce: base}
	setReadDeadline := func() {
		_ = stream.SetReadDeadline(time.Now().Add(idle))
	}
	clearReadDeadline := func() {
		_ = stream.SetReadDeadline(time.Time{})
	}
	setReadDeadline()
	defer clearReadDeadline()

	hdr, err := reader.ReadChunk()
	if err != nil {
		return "", nil, 0, err
	}
	if len(hdr) < fileHeaderFixedLen {
		return "", nil, 0, fmt.Errorf("invalid header")
	}
	nameLen := binary.LittleEndian.Uint16(hdr[0:fileHeaderNameLenSize])
	if fileHeaderFixedLen+int(nameLen) > len(hdr) {
		return "", nil, 0, fmt.Errorf("header truncated")
	}
	size := binary.LittleEndian.Uint64(hdr[fileHeaderNameLenSize:fileHeaderFixedLen])
	name := sanitizeFilename(string(hdr[fileHeaderFixedLen : fileHeaderFixedLen+int(nameLen)]))
	if name == "" {
		name = "wormzy-file"
	}

	targetDir := downloadDir
	if targetDir == "" {
		targetDir = "."
	}
	if err := ensureFreeSpace(targetDir, size); err != nil {
		return "", nil, 0, err
	}
	outPath, renamed, err := pickDownloadPath(targetDir, name)
	if err != nil {
		return "", nil, 0, err
	}
	if renamed && rep != nil {
		rep.Logf("target %s exists; saving as %s", filepath.Join(targetDir, name), outPath)
	}

	out, err := os.Create(outPath)
	if err != nil {
		return "", nil, 0, err
	}
	defer out.Close()

	hasher := blake3.New()
	var written uint64
	lastPct := -1

	for {
		setReadDeadline()
		chunk, err := reader.ReadChunk()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, 0, err
		}
		if _, err := hasher.Write(chunk); err != nil {
			return "", nil, 0, err
		}
		if _, err := out.Write(chunk); err != nil {
			return "", nil, 0, err
		}
		written += uint64(len(chunk))
		reportTransferProgress(rep, "Receiving", clampInt64(written), clampInt64(size), &lastPct)
		if written >= size {
			break
		}
	}
	reportTransferProgress(rep, "Receiving", clampInt64(size), clampInt64(size), &lastPct)
	if written != size {
		return "", nil, 0, fmt.Errorf("expected %d bytes, wrote %d", size, written)
	}

	setReadDeadline()
	sum := hasher.Sum(nil)
	if err := verifyMetadata(reader, sum); err != nil {
		return "", nil, 0, err
	}
	return outPath, sum, clampInt64(size), nil
}

func sanitizeFilename(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

func pickDownloadPath(dir, filename string) (string, bool, error) {
	base := filepath.Join(dir, filename)
	exists, err := pathExists(base)
	if err != nil {
		return "", false, err
	}
	if !exists {
		return base, false, nil
	}

	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	for i := 1; i <= 99; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (wormzy-%d)%s", stem, i, ext))
		exists, err := pathExists(candidate)
		if err != nil {
			return "", false, err
		}
		if !exists {
			return candidate, true, nil
		}
	}
	var randBuf [4]byte
	if _, err := crand.Read(randBuf[:]); err == nil {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%s%s", stem, hex.EncodeToString(randBuf[:]), ext))
		return candidate, true, nil
	}
	return "", false, fmt.Errorf("unable to find free destination for %s", filename)
}

func pathExists(p string) (bool, error) {
	_, err := os.Stat(p)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

func localEndpoint(conn *net.UDPConn) string {
	if conn == nil {
		return ""
	}
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return conn.LocalAddr().String()
	}
	ip := addr.IP
	if ip == nil || ip.IsUnspecified() {
		if guess := pickLocalIPv4(); guess != nil {
			ip = guess
		}
	}
	if ip == nil || ip.IsUnspecified() {
		return addr.String()
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(addr.Port))
}

func pickLocalIPv4() net.IP {
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip = ip.To4(); ip != nil {
					return ip
				}
			}
		}
	}
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return nil
	}
	defer conn.Close()
	udp, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	return udp.IP.To4()
}

// ensureFreeSpace checks that the target directory has at least the required bytes.
func ensureFreeSpace(dir string, needed uint64) error {
	avail, err := diskFreeBytes(dir)
	if err != nil {
		return fmt.Errorf("checking disk space: %w", err)
	}
	if avail < needed {
		return fmt.Errorf("insufficient disk space in %q (need %s, have %s)", dir, formatBytes(clampInt64(needed)), formatBytes(clampInt64(avail)))
	}
	return nil
}

func transportLabelForCandidate(cand rendezvous.Candidate) string {
	if strings.Contains(strings.ToLower(cand.Type), "relay") {
		return "relay"
	}
	return "p2p"
}

// deriveSAS produces a short authentication string for human verification, mixing the
// Noise transcript with the PAKE-derived key.
func deriveSAS(transcript []byte, psk []byte) string {
	sum := blake3.Sum256(append(transcript, psk...))
	lo := binary.BigEndian.Uint16(sum[0:2]) % 10000
	hi := binary.BigEndian.Uint16(sum[2:4]) % 10000
	return fmt.Sprintf("%04d-%04d", hi, lo)
}

func punchLoop(ctx context.Context, conn *net.UDPConn, peer *net.UDPAddr, stop <-chan struct{}) {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	msg := []byte("punch")
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = conn.WriteToUDP(msg, peer)
		}
	}
}

func verifyMetadata(reader *aeadReader, digest []byte) error {
	chunk, err := reader.ReadChunk()
	switch {
	case err == io.EOF:
		return nil
	case err != nil:
		return err
	}
	if !bytes.HasPrefix(chunk, []byte(metaPrefix)) {
		return fmt.Errorf("unexpected trailer data")
	}
	var meta fileMetadata
	if err := json.Unmarshal(chunk[len(metaPrefix):], &meta); err != nil {
		return err
	}
	if meta.Hash == "blake3-256" && len(meta.Digest) > 0 {
		if !hmac.Equal(digest, meta.Digest) {
			return fmt.Errorf("file hash mismatch")
		}
	}
	return nil
}

func selfSignedTLS() (*tls.Config, error) {
	key, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, _ := crand.Int(crand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "wormzy-quic",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(crand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}, nil
}

func reportTransferProgress(rep Reporter, verb string, current, total int64, lastPct *int) {
	if rep == nil || total <= 0 {
		return
	}
	pct := int((current * 100) / total)
	if pct > 100 {
		pct = 100
	}
	if lastPct != nil && pct == *lastPct {
		return
	}
	detail := fmt.Sprintf("%s %s/%s (%d%%)", verb, formatBytes(current), formatBytes(total), pct)
	rep.Stage(StageTransfer, StageStateRunning, detail)
	if lastPct != nil {
		*lastPct = pct
	}
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
