package transport

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cpace "filippo.io/cpace"
	"github.com/flynn/noise"
	"github.com/jdefrancesco/internal/rendezvous"
	"github.com/jdefrancesco/internal/stun"
	"github.com/quic-go/quic-go"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	alpn          = "p2p-wormzy-1"
	defaultRelay  = "127.0.0.1:9999"
	defaultDialTO = 60 * time.Second
)

// Config controls how a Wormzy transfer session behaves.
type Config struct {
	Mode        string
	FilePath    string
	Code        string
	RelayAddr   string
	RelayPin    string
	STUNServers []string
	Timeout     time.Duration
	Loopback    bool
}

// Result reports information about the established session.
type Result struct {
	Code string
	Peer rendezvous.SelfInfo
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
func Run(ctx context.Context, cfg Config, rep Reporter) (*Result, error) {
	reporter := rep
	if reporter == nil {
		reporter = ReporterFunc(func(string, ...interface{}) {})
	}
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer udpConn.Close()
	reporter.Logf("udp/listen %s", udpConn.LocalAddr())

	self := rendezvous.SelfInfo{Local: udpConn.LocalAddr().String()}
	ctxStun, cancelStun := context.WithTimeout(ctx, cfg.Timeout)
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
	if cfg.Loopback {
		self.Public = self.Local
	}
	if self.Public == "" {
		self.Public = self.Local
	}

	reporter.Stage(StageRendezvous, StageStateRunning, "dialing relay")
	peer, code, psk, err := rendezvousExchange(ctx, cfg, self, reporter)
	if err != nil {
		reporter.Stage(StageRendezvous, StageStateError, err.Error())
		return nil, err
	}
	reporter.Stage(StageRendezvous, StageStateDone, peer.Public)
	reporter.Logf("paired with code %s", code)

	peerAddr := peer.Public
	if cfg.Loopback && peer.Local != "" {
		peerAddr = peer.Local
	}
	peerUDP, err := net.ResolveUDPAddr("udp4", peerAddr)
	if err != nil {
		return nil, err
	}

	punchCtx, cancelPunch := context.WithTimeout(ctx, cfg.Timeout)
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
	quicConf := &quic.Config{KeepAlivePeriod: 15 * time.Second}

	var quicConn *quic.Conn

	switch cfg.Mode {
	case "send":
		reporter.Stage(StageQUIC, StageStateRunning, "listening for peer")
		ln, err := quicTransport.Listen(serverTLS, quicConf)
		if err != nil {
			return nil, err
		}
		defer ln.Close()
		reporter.Logf("waiting for peer to dial QUIC")
		ctxAccept, cancelAccept := context.WithTimeout(ctx, cfg.Timeout)
		defer cancelAccept()
		conn, err := ln.Accept(ctxAccept)
		if err != nil {
			reporter.Stage(StageQUIC, StageStateError, err.Error())
			return nil, err
		}
		quicConn = conn
		reporter.Logf("accepted QUIC connection from %s", conn.RemoteAddr())
		reporter.Stage(StageQUIC, StageStateDone, conn.RemoteAddr().String())
	case "recv":
		reporter.Stage(StageQUIC, StageStateRunning, "dialing peer")
		ctxDial, cancelDial := context.WithTimeout(ctx, cfg.Timeout)
		defer cancelDial()
		conn, err := quicTransport.Dial(ctxDial, peerUDP, clientTLS, quicConf)
		if err != nil {
			reporter.Stage(StageQUIC, StageStateError, err.Error())
			return nil, err
		}
		quicConn = conn
		reporter.Logf("dialed QUIC peer %s", peerUDP)
		reporter.Stage(StageQUIC, StageStateDone, peerUDP.String())
	}

	close(stopPunch)
	punchWG.Wait()

	reporter.Stage(StageNoise, StageStateRunning, "noise handshake")
	fileKey, err := runNoiseOverQUIC(quicConn, cfg.Mode == "recv", psk)
	if err != nil {
		reporter.Stage(StageNoise, StageStateError, err.Error())
		return nil, err
	}
	reporter.Stage(StageNoise, StageStateDone, "session keys derived")

	switch cfg.Mode {
	case "send":
		reporter.Stage(StageTransfer, StageStateRunning, "streaming file")
		if err := sendFileEncrypted(quicConn, cfg.FilePath, fileKey); err != nil {
			reporter.Stage(StageTransfer, StageStateError, err.Error())
			return nil, err
		}
		reporter.Logf("transfer complete")
		reporter.Stage(StageTransfer, StageStateDone, "file sent")
	case "recv":
		reporter.Stage(StageTransfer, StageStateRunning, "receiving file")
		path, err := receiveFile(quicConn, fileKey)
		if err != nil {
			reporter.Stage(StageTransfer, StageStateError, err.Error())
			return nil, err
		}
		reporter.Logf("saved file to %s", path)
		reporter.Stage(StageTransfer, StageStateDone, path)
	}

	return &Result{Code: code, Peer: peer}, nil
}

func (cfg Config) withDefaults() Config {
	if cfg.RelayAddr == "" {
		cfg.RelayAddr = defaultRelay
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultDialTO
	}
	return cfg
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
	if len(cfg.STUNServers) > 0 {
		return cfg.STUNServers
	}
	return stun.StunServers
}

func rendezvousExchange(ctx context.Context, cfg Config, me rendezvous.SelfInfo, rep Reporter) (peer rendezvous.SelfInfo, assigned string, psk []byte, err error) {
	dialer := func() (net.Conn, error) {
		if cfg.RelayPin != "" {
			return tls.Dial("tcp", cfg.RelayAddr, tlsPinnedConfig(cfg.RelayPin))
		}
		conn, err := tls.Dial("tcp", cfg.RelayAddr, tlsPinnedConfig(""))
		if err != nil {
			return net.Dial("tcp", cfg.RelayAddr)
		}
		return conn, nil
	}

	conn, err := dialer()
	if err != nil {
		return peer, assigned, nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	r := bufio.NewReader(conn)

	if err := writeMsg(conn, "hello", map[string]string{"role": cfg.Mode, "code": cfg.Code}); err != nil {
		return peer, assigned, nil, err
	}

	msg, err := readMsg(r)
	if err != nil {
		return peer, assigned, nil, err
	}
	if msg.Type != "code" {
		return peer, assigned, nil, fmt.Errorf("rendezvous: expected code message, got %s", msg.Type)
	}
	var codeBody map[string]string
	if err := json.Unmarshal(msg.Body, &codeBody); err != nil {
		return peer, assigned, nil, err
	}
	assigned = codeBody["code"]
	rep.Stage(StageRendezvous, StageStateRunning, "code "+assigned)
	rep.Logf("rendezvous assigned code %s", assigned)

	if err := writeMsg(conn, "self", me); err != nil {
		return peer, assigned, nil, err
	}

	psk, err = runPAKEOverRelay(r, conn, cfg.Mode, assigned, "send", "recv")
	if err != nil {
		return peer, assigned, nil, err
	}

	// peer info
	msg, err = readMsg(r)
	if err != nil {
		return peer, assigned, nil, err
	}
	if msg.Type != "peer" {
		return peer, assigned, nil, fmt.Errorf("rendezvous: expected peer info, got %s", msg.Type)
	}
	if err := json.Unmarshal(msg.Body, &peer); err != nil {
		return peer, assigned, nil, err
	}
	return peer, assigned, psk, nil
}

func tlsPinnedConfig(pinB64 string) *tls.Config {
	var pin []byte
	if pinB64 != "" {
		if dec, err := base64.StdEncoding.DecodeString(pinB64); err == nil && len(dec) == 32 {
			pin = dec
		}
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"wormzy-rendezvous-1"},
		InsecureSkipVerify: pin == nil,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if pin == nil {
				return nil
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
			if !hmac.Equal(sum[:], pin) {
				return fmt.Errorf("relay SPKI pin mismatch")
			}
			return nil
		},
	}
}

func runPAKEOverRelay(r *bufio.Reader, w io.Writer, role, code, idA, idB string) ([]byte, error) {
	ci := cpace.NewContextInfo(idA, idB, []byte("wormzy-pake-v1"))
	if role == "send" {
		msgA, st, err := cpace.Start(code, ci)
		if err != nil {
			return nil, err
		}
		if err := writeMsg(w, "pake1", msgA); err != nil {
			return nil, err
		}
		m, err := readMsg(r)
		if err != nil {
			return nil, err
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
		if err := writeMsg(w, "pake2", []byte{}); err != nil {
			return nil, err
		}
		return keyA, nil
	}

	m, err := readMsg(r)
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
	if err := writeMsg(w, "pake1", msgB); err != nil {
		return nil, err
	}
	if _, err := readMsg(r); err != nil {
		return nil, err
	}
	return keyB, nil
}

func runNoiseOverQUIC(conn *quic.Conn, initiator bool, psk []byte) ([]byte, error) {
	var stream *quic.Stream
	var err error
	ctx := context.Background()
	if initiator {
		stream, err = conn.OpenStreamSync(ctx)
	} else {
		stream, err = conn.AcceptStream(ctx)
	}
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	hs, err := noise.NewHandshakeState(noise.Config{
		Pattern:     noise.HandshakeNN,
		Initiator:   initiator,
		CipherSuite: suite,
		Prologue:    []byte("wormzy-noise-v1"),
		Random:      rand.Reader,
	})
	if err != nil {
		return nil, err
	}

	writeFrame := func(b []byte) error {
		if len(b) > 65535 {
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
			return nil, err
		}
		appendTranscript(msg1)
		if err := writeFrame(msg1); err != nil {
			return nil, err
		}

		in2, err := readFrame()
		if err != nil {
			return nil, err
		}
		appendTranscript(in2)
		if _, _, _, err := hs.ReadMessage(nil, in2); err != nil {
			return nil, err
		}
	} else {
		in1, err := readFrame()
		if err != nil {
			return nil, err
		}
		appendTranscript(in1)
		if _, _, _, err := hs.ReadMessage(nil, in1); err != nil {
			return nil, err
		}

		msg2, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, err
		}
		appendTranscript(msg2)
		if err := writeFrame(msg2); err != nil {
			return nil, err
		}
	}

	th := sha256.Sum256(transcript)
	fileKey := make([]byte, chacha20poly1305.KeySize)
	kdf := hkdf.New(sha256.New, psk, th[:], []byte("wormzy-filekey-v1"))
	if _, err := io.ReadFull(kdf, fileKey); err != nil {
		return nil, err
	}
	return fileKey, nil
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

func sendFileEncrypted(conn *quic.Conn, path string, key []byte) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("path %s is a directory", path)
	}
	name := filepath.Base(path)
	if len(name) > 65535 {
		return fmt.Errorf("filename too long")
	}
	size := fi.Size()

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	us, err := conn.OpenUniStreamSync(context.Background())
	if err != nil {
		return err
	}
	defer us.Close()

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}
	var base [24]byte
	if _, err := rand.Read(base[:]); err != nil {
		return err
	}
	if _, err := us.Write(base[:]); err != nil {
		return err
	}
	writer := &aeadWriter{w: us, aead: aead, baseNonce: base}

	header := make([]byte, 10+len(name))
	binary.LittleEndian.PutUint16(header[0:2], uint16(len(name)))
	binary.LittleEndian.PutUint64(header[2:10], uint64(size))
	copy(header[10:], []byte(name))
	if err := writer.WriteChunk(header); err != nil {
		return err
	}

	buf := make([]byte, 1<<16)
	for {
		n, er := file.Read(buf)
		if n > 0 {
			if err := writer.WriteChunk(buf[:n]); err != nil {
				return err
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			return er
		}
	}
	return nil
}

func receiveFile(conn *quic.Conn, key []byte) (string, error) {
	stream, err := conn.AcceptUniStream(context.Background())
	if err != nil {
		return "", err
	}
	defer stream.CancelRead(0)

	var base [24]byte
	if _, err := io.ReadFull(stream, base[:]); err != nil {
		return "", err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", err
	}
	reader := &aeadReader{r: stream, aead: aead, baseNonce: base}

	hdr, err := reader.ReadChunk()
	if err != nil {
		return "", err
	}
	if len(hdr) < 10 {
		return "", fmt.Errorf("invalid header")
	}
	nameLen := binary.LittleEndian.Uint16(hdr[0:2])
	if int(10+nameLen) > len(hdr) {
		return "", fmt.Errorf("header truncated")
	}
	size := binary.LittleEndian.Uint64(hdr[2:10])
	name := sanitizeFilename(string(hdr[10 : 10+nameLen]))
	if name == "" {
		name = "wormzy-file"
	}

	out, err := os.Create(name)
	if err != nil {
		return "", err
	}
	defer out.Close()

	var written uint64
	for {
		chunk, err := reader.ReadChunk()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if _, err := out.Write(chunk); err != nil {
			return "", err
		}
		written += uint64(len(chunk))
		if written >= size {
			break
		}
	}
	return out.Name(), nil
}

func sanitizeFilename(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

func punchLoop(ctx context.Context, conn *net.UDPConn, peer *net.UDPAddr, stop <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
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

func writeMsg(w io.Writer, typ string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	env := wire{Type: typ, Body: payload}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

func readMsg(r *bufio.Reader) (wire, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return wire{}, err
	}
	line = strings.TrimSpace(line)
	var env wire
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return wire{}, err
	}
	if env.Type == "err" {
		var e map[string]string
		_ = json.Unmarshal(env.Body, &e)
		return wire{}, fmt.Errorf("relay error: %s", e["error"])
	}
	return env, nil
}

type wire struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

func selfSignedTLS() (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
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
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}, nil
}
