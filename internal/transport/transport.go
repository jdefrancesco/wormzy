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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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
	alpn = "p2p-wormzy-2"
	// defaultRelay is the baked-in rendezvous/mailbox endpoint. Users can override
	// via CLI flag or environment (WORMZY_RELAY_URL / WORMZY_RELAY).
	defaultRelay              = "https://relay.wormzy.io"
	defaultRelayUDPPort       = 3478
	defaultHandshakeTO        = 90 * time.Second
	defaultTransferIdleTO     = 5 * time.Minute
	relayFallbackDelay        = 4 * time.Second
	relayRetryDelay           = 3 * time.Second
	relayAttemptTimeout       = 6 * time.Second
	statsReportTimeout        = 5 * time.Second
	mailboxLeaseInterval      = 2 * time.Minute
	mailboxLeaseRequestTO     = 10 * time.Second
	peerConfirmationNonceSize = 32
	peerConfirmationPreface   = "wormzy-peer-confirm-v2"

	// Wire-format sizing limits.
	maxUint16PayloadLen = (1 << 16) - 1

	// File header layout: uint16(nameLen) + uint64(fileSize) + name bytes.
	fileHeaderNameLenSize = 2
	fileHeaderSizeSize    = 8
	fileHeaderFixedLen    = fileHeaderNameLenSize + fileHeaderSizeSize
	fileHashAlgorithm     = "blake3-256"
	fileDigestSize        = 32

	// Encrypted frames carry either a file header, one file chunk, or the small
	// integrity trailer. The header is the largest legal plaintext because its
	// filename length is encoded as a uint16.
	maxAEADPlaintextSize  = fileHeaderFixedLen + maxUint16PayloadLen
	maxAEADCiphertextSize = maxAEADPlaintextSize + chacha20poly1305.Overhead
)

var errPeerKeyConfirmation = errors.New("peer key confirmation failed")

// Config controls how a Wormzy transfer session behaves.
type Config struct {
	Mode        string
	FilePath    string
	Code        string
	RelayAddr   string
	RelayPin    string
	STUNServers []string
	// TURNServers holds authenticated TURN URI strings used by ICE (for example:
	// "turn:user:pass@turn.example.com:3478?transport=udp").
	TURNServers      []string
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	Loopback         bool
	DisableUPnP      bool
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

// Run executes a full rendezvous and NAT traversal flow for the configured mode.
// It claims the pairing code before discovery, tries direct ICE, then falls back
// through legacy direct candidates and the relay before streaming the file.
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
	reporter.Stage(StageRendezvous, StageStateRunning, "dialing relay")
	mbox, err := newMailbox(ctx, cfg)
	if err != nil {
		reporter.Stage(StageRendezvous, StageStateError, err.Error())
		return nil, err
	}
	mbox = newProtocolMailbox(mbox)
	defer mbox.Close()

	claimCtx, cancelClaim := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	code, err := claimPairingCode(claimCtx, cfg, reporter, mbox)
	cancelClaim()
	if err != nil {
		reporter.Stage(StageRendezvous, StageStateError, err.Error())
		return nil, err
	}
	stats := transferStats{Mode: cfg.Mode}
	var sessionCleanup func()
	defer func() {
		finalizeTransfer(mbox, stats, res, finalErr, started, sessionCleanup, reporter)
	}()

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer udpConn.Close()
	reporter.Logf("udp/listen %s", udpConn.LocalAddr())

	self := rendezvous.SelfInfo{
		Local: localEndpoint(udpConn),
		Features: []string{
			featureICEv1,
			featureProgressiveUPnPV1,
			featureTransferCompletionV1,
			featureAuthenticatedSignalingV1,
		},
	}
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
		stunServers := cfg.stunServers()
		if len(stunServers) > 0 {
			reporter.Logf("stun/servers %s", strings.Join(stunServers, ", "))
		}
		reporter.Stage(StageSTUN, StageStateRunning, "probing reflexive address")
		pub, err := stun.DiscoverOnConn(ctxStun, udpConn, stunServers, 2*time.Second, 2)
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
	self.Candidates = buildCandidates(self, cfg.Loopback, "", cfg.relayCandidateAddr())
	reporter.Logf("candidates/self %s", formatCandidateList(self.Candidates))

	pairingCtx, cancelPairing := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	peer, psk, err := completeRendezvousExchange(pairingCtx, cfg, self, reporter, mbox, code)
	cancelPairing()
	if err != nil {
		reporter.Stage(StageRendezvous, StageStateError, err.Error())
		finalErr = err
		return nil, err
	}
	if err := requirePeerTransferCompletion(peer); err != nil {
		reporter.Stage(StageRendezvous, StageStateError, err.Error())
		return nil, err
	}
	directCandidates, relayCand, err := selectPeerCandidates(self, peer, cfg.Loopback)
	if err != nil {
		reporter.Stage(StageRendezvous, StageStateError, err.Error())
		return nil, err
	}
	reporter.Logf("candidates/peer %s", formatCandidateList(peer.Candidates))
	if len(directCandidates) > 0 {
		first := directCandidates[0]
		extra := ""
		if len(directCandidates) > 1 {
			extra = fmt.Sprintf(" +%d candidates", len(directCandidates)-1)
		}
		reporter.Stage(StageRendezvous, StageStateDone, fmt.Sprintf("%s (%s)%s", first.Addr, first.Type, extra))
	} else if relayCand != nil {
		reporter.Stage(StageRendezvous, StageStateDone, fmt.Sprintf("%s (%s)", relayCand.Addr, relayCand.Type))
	} else {
		reporter.Stage(StageRendezvous, StageStateError, "no usable transport candidates")
		return nil, fmt.Errorf("no usable transport candidates")
	}
	reporter.Logf("paired with authenticated peer")

	initialCandidate := pickFallbackDirectCandidate(directCandidates)
	if relayCand != nil && len(directCandidates) == 0 {
		initialCandidate = *relayCand
	}
	stats.Candidate = initialCandidate.Type
	stats.Transport = transportLabelForCandidate(initialCandidate)
	pathSelection := prepareProgressivePath(
		ctx,
		cfg,
		mbox,
		reporter,
		udpConn,
		self,
		peer,
		directCandidates,
		relayCand,
		code,
		psk,
	)
	defer pathSelection.cleanup(reporter)
	peer = pathSelection.peer
	directCandidates = pathSelection.directCandidates
	relayCand = pathSelection.relayCandidate

	if iceSession := pathSelection.iceSession; iceSession != nil {
		sessionCleanup = iceSession.cleanup
		stats.Candidate = iceSession.candidate.Type
		stats.Transport = transportLabelForCandidate(iceSession.candidate)
		stats.DirectOutcome = "won"
		stats.DirectSummary = fmt.Sprintf("%s@%s=won", iceSession.candidate.Type, iceSession.candidate.Addr)
		reporter.Logf("direct race outcome=%s details=%s", stats.DirectOutcome, stats.DirectSummary)
		if iceSession.initiated {
			reporter.Logf("dialed QUIC peer via ICE %s", iceSession.candidate.Addr)
			reporter.Stage(StageQUIC, StageStateDone, iceSession.candidate.Addr)
		} else {
			reporter.Logf("accepted QUIC connection via ICE from %s", iceSession.conn.RemoteAddr())
			reporter.Stage(StageQUIC, StageStateDone, iceSession.conn.RemoteAddr().String())
		}

		return transferEstablishedQUICSession(
			ctx, cfg, reporter, iceSession.conn, iceSession.initiated,
			false, mbox, self, peer, code, psk, &stats,
		)
	}

	legacyPath, err := establishLegacyQUICPath(ctx, cfg, reporter, udpConn, directCandidates, relayCand, psk)
	if err != nil {
		return nil, err
	}
	defer legacyPath.cleanup()
	stats.Candidate = legacyPath.candidate.Type
	stats.Transport = transportLabelForCandidate(legacyPath.candidate)
	stats.DirectOutcome = legacyPath.directOutcome
	stats.DirectSummary = legacyPath.directSummary

	return transferEstablishedQUICSession(
		ctx, cfg, reporter, legacyPath.conn, legacyPath.initiated, true,
		mbox, self, peer, code, psk, &stats,
	)

}

// transferEstablishedQUICSession runs the shared authenticated Noise, file,
// integrity, and completion pipeline over a selected QUIC connection.
func transferEstablishedQUICSession(
	ctx context.Context,
	cfg Config,
	reporter Reporter,
	conn *quic.Conn,
	initiated bool,
	peerConfirmed bool,
	mbox mailbox,
	self rendezvous.SelfInfo,
	peer rendezvous.SelfInfo,
	code string,
	psk []byte,
	stats *transferStats,
) (*Result, error) {
	stopLease := startMailboxLease(ctx, mbox, self, mailboxLeaseInterval, reporter)
	defer stopLease()

	reporter.Stage(StageNoise, StageStateRunning, "noise handshake")
	var (
		fileKey []byte
		sas     string
		err     error
	)
	if peerConfirmed {
		fileKey, sas, err = runNoiseOverConfirmedQUIC(ctx, conn, initiated, psk, cfg.HandshakeTimeout)
	} else {
		fileKey, sas, err = runNoiseOverQUIC(ctx, conn, initiated, psk, cfg.HandshakeTimeout)
	}
	if err != nil {
		if stats.Transport == "p2p" {
			stats.DirectOutcome = "noise-failed"
		}
		reporter.Stage(StageNoise, StageStateError, err.Error())
		return nil, err
	}
	reporter.Logf("noise handshake SAS %s", sas)
	reporter.Stage(StageNoise, StageStateDone, fmt.Sprintf("confirm SAS %s", sas))

	res := &Result{
		Code:      code,
		Peer:      peer,
		Mode:      cfg.Mode,
		Transport: stats.Transport,
		Candidate: stats.Candidate,
	}
	var (
		transferDigest    []byte
		transferDoneLabel string
	)

	switch cfg.Mode {
	case "send":
		reporter.Stage(StageTransfer, StageStateRunning, "streaming file")
		sum, size, err := sendFileEncrypted(ctx, conn, cfg.FilePath, fileKey, cfg.IdleTimeout, reporter)
		if err != nil {
			reporter.Stage(StageTransfer, StageStateError, err.Error())
			return nil, err
		}
		res.FilePath = cfg.FilePath
		res.FileSize = size
		res.FileHash = hex.EncodeToString(sum)
		transferDigest = sum
		transferDoneLabel = "file sent"
		reporter.Logf("file stream sent; awaiting authenticated receipt")
	case "recv":
		reporter.Stage(StageTransfer, StageStateRunning, "receiving file")
		path, sum, size, err := receiveFile(ctx, conn, fileKey, cfg.DownloadDir, cfg.IdleTimeout, reporter)
		if err != nil {
			reporter.Stage(StageTransfer, StageStateError, err.Error())
			return nil, err
		}
		res.FilePath = path
		res.FileSize = size
		res.FileHash = hex.EncodeToString(sum)
		transferDigest = sum
		transferDoneLabel = path
		reporter.Logf("saved file to %s", path)
	}

	if err := finishTransferSession(ctx, cfg, peer, conn, code, fileKey, transferDigest, res.FileSize, reporter); err != nil {
		reporter.Stage(StageTransfer, StageStateError, err.Error())
		return nil, err
	}
	reporter.Logf("transfer complete")
	reporter.Stage(StageTransfer, StageStateDone, transferDoneLabel)
	return res, nil
}

// startMailboxLease refreshes authenticated session state while file bytes are
// in flight so a healthy long transfer retains its final telemetry slot.
func startMailboxLease(
	ctx context.Context,
	mbox mailbox,
	self rendezvous.SelfInfo,
	interval time.Duration,
	reporter Reporter,
) func() {
	if mbox == nil || interval <= 0 {
		return func() {}
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				refreshCtx, cancelRefresh := context.WithTimeout(leaseCtx, mailboxLeaseRequestTO)
				err := mbox.StoreSelf(refreshCtx, self)
				cancelRefresh()
				if err != nil && leaseCtx.Err() == nil && reporter != nil {
					reporter.Logf("mailbox/lease refresh failed: %v", err)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
}

// finalizeTransfer reports privacy-safe outcome telemetry before releasing transport resources.
func finalizeTransfer(
	mbox mailbox,
	stats transferStats,
	res *Result,
	finalErr error,
	started time.Time,
	cleanup func(),
	rep Reporter,
) {
	if mbox != nil {
		stats.Completed = finalErr == nil
		if finalErr != nil {
			stats.Error = reportedFailureCategory(finalErr)
		}
		stats.DurationMillis = time.Since(started).Milliseconds()
		if res != nil {
			stats.Bytes = res.FileSize
		}

		statsCtx, cancel := context.WithTimeout(context.Background(), statsReportTimeout)
		err := mbox.ReportStats(statsCtx, stats)
		cancel()
		if err != nil {
			if rep != nil {
				rep.Logf("report stats failed: %v", err)
			}
		} else if rep != nil {
			rep.Logf(
				"session/stats completed=%t transport=%s candidate=%s",
				stats.Completed,
				stats.Transport,
				stats.Candidate,
			)
		}
	}
	if cleanup != nil {
		cleanup()
	}
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
	if cfg.Mode == "send" {
		if cfg.FilePath == "" {
			return fmt.Errorf("send mode requires a file path")
		}
		info, err := os.Stat(cfg.FilePath)
		if err != nil {
			return fmt.Errorf("cannot access %q: %w", cfg.FilePath, err)
		}
		if info.IsDir() {
			return fmt.Errorf("cannot send directory %q; archive or compress it first, then send the resulting file", cfg.FilePath)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cannot send %q because it is not a regular file", cfg.FilePath)
		}
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
	r := mrand.New(src) // #nosec G404 -- shuffling public STUN endpoints is not security-sensitive.
	r.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	return list
}

func (cfg Config) turnServers() []string {
	list := cfg.TURNServers
	if len(list) == 0 {
		list = DefaultTURNServers(cfg.RelayAddr)
	}
	if len(list) == 0 {
		return nil
	}
	// Keep ordering stable so admins can prioritize TURN pools explicitly.
	out := make([]string, 0, len(list))
	for _, v := range list {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// DefaultRelay returns the compiled-in rendezvous Redis endpoint.
func DefaultRelay() string {
	return defaultRelay
}

// DefaultTURNServers returns no implicit TURN server. TURN requires credentials,
// which cannot be derived safely from the rendezvous address.
func DefaultTURNServers(_ string) []string {
	return nil
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

// completeRendezvousExchange publishes candidates, runs PAKE, and waits for the peer after code assignment.
func completeRendezvousExchange(
	ctx context.Context,
	cfg Config,
	me rendezvous.SelfInfo,
	rep Reporter,
	mb mailbox,
	assigned string,
) (peer rendezvous.SelfInfo, psk []byte, err error) {
	if err := mb.StoreSelf(ctx, me); err != nil {
		return peer, nil, friendlyRendezvousErr(err)
	}

	psk, err = runPAKEOverMailbox(ctx, mb, cfg.Mode, assigned, "send", "recv")
	if err != nil {
		return peer, nil, friendlyRendezvousErr(err)
	}

	peerInfo, err := mb.WaitPeer(ctx)
	if err != nil {
		return peer, nil, friendlyRendezvousErr(err)
	}
	if err := authenticatePeerSnapshot(ctx, mb, assigned, cfg.Mode, psk, me, *peerInfo); err != nil {
		return peer, nil, friendlyRendezvousErr(err)
	}
	if rep != nil {
		rep.Logf("rendezvous peer metadata authenticated")
	}
	return *peerInfo, psk, nil
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

// confirmPeerPSK performs replay-resistant mutual proof of the PAKE key before
// either side begins the Noise handshake or opens a file stream.
func confirmPeerPSK(stream io.ReadWriter, initiator bool, psk []byte, random io.Reader) error {
	if len(psk) != sha256.Size {
		return fmt.Errorf("%w: invalid key", errPeerKeyConfirmation)
	}
	if random == nil {
		random = crand.Reader
	}

	challenge := make([]byte, peerConfirmationNonceSize)
	initiatorNonce := make([]byte, peerConfirmationNonceSize)
	if initiator {
		if err := writeAll(stream, []byte(peerConfirmationPreface)); err != nil {
			return fmt.Errorf("write peer confirmation preface: %w", err)
		}
		if _, err := io.ReadFull(stream, challenge); err != nil {
			return fmt.Errorf("read peer confirmation challenge: %w", err)
		}
		if _, err := io.ReadFull(random, initiatorNonce); err != nil {
			return fmt.Errorf("generate peer confirmation nonce: %w", err)
		}
		proof := peerConfirmationMAC(psk, "initiator", challenge, initiatorNonce)
		if err := writeAll(stream, append(initiatorNonce, proof...)); err != nil {
			return fmt.Errorf("write peer confirmation proof: %w", err)
		}
		response := make([]byte, sha256.Size)
		if _, err := io.ReadFull(stream, response); err != nil {
			return fmt.Errorf("read peer confirmation response: %w", err)
		}
		want := peerConfirmationMAC(psk, "responder", challenge, initiatorNonce)
		if !hmac.Equal(response, want) {
			return errPeerKeyConfirmation
		}
		return nil
	}

	preface := make([]byte, len(peerConfirmationPreface))
	if _, err := io.ReadFull(stream, preface); err != nil {
		return fmt.Errorf("read peer confirmation preface: %w", err)
	}
	if !hmac.Equal(preface, []byte(peerConfirmationPreface)) {
		return fmt.Errorf("%w: protocol preface", errPeerKeyConfirmation)
	}
	if _, err := io.ReadFull(random, challenge); err != nil {
		return fmt.Errorf("generate peer confirmation challenge: %w", err)
	}
	if err := writeAll(stream, challenge); err != nil {
		return fmt.Errorf("write peer confirmation challenge: %w", err)
	}
	proofFrame := make([]byte, peerConfirmationNonceSize+sha256.Size)
	if _, err := io.ReadFull(stream, proofFrame); err != nil {
		return fmt.Errorf("read peer confirmation proof: %w", err)
	}
	copy(initiatorNonce, proofFrame[:peerConfirmationNonceSize])
	want := peerConfirmationMAC(psk, "initiator", challenge, initiatorNonce)
	if !hmac.Equal(proofFrame[peerConfirmationNonceSize:], want) {
		return errPeerKeyConfirmation
	}
	response := peerConfirmationMAC(psk, "responder", challenge, initiatorNonce)
	if err := writeAll(stream, response); err != nil {
		return fmt.Errorf("write peer confirmation response: %w", err)
	}
	return nil
}

// peerConfirmationMAC computes one length-delimited proof for the pre-Noise
// PAKE key-confirmation exchange.
func peerConfirmationMAC(psk []byte, role string, fields ...[]byte) []byte {
	mac := hmac.New(sha256.New, psk)
	allFields := make([][]byte, 0, len(fields)+2)
	allFields = append(allFields, []byte("wormzy-quic-peer-confirmation-v2"), []byte(role))
	allFields = append(allFields, fields...)
	var length [4]byte
	for _, field := range allFields {
		binary.BigEndian.PutUint32(length[:], uint32(len(field))) // #nosec G115 -- bounded protocol fields.
		_, _ = mac.Write(length[:])
		_, _ = mac.Write(field)
	}
	return mac.Sum(nil)
}

// peerConfirmationTimeout bounds how long an unauthenticated QUIC path can occupy a race worker.
func peerConfirmationTimeout(handshakeTimeout time.Duration) time.Duration {
	return boundedDuration(handshakeTimeout/10, 2*time.Second, 5*time.Second)
}

// confirmQUICPeer proves PAKE-key possession before a candidate may win a connection race.
func confirmQUICPeer(ctx context.Context, conn *quic.Conn, initiator bool, psk []byte) error {
	var stream *quic.Stream
	var err error
	if initiator {
		stream, err = conn.OpenStreamSync(ctx)
	} else {
		stream, err = conn.AcceptStream(ctx)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errPeerKeyConfirmation, err)
	}
	defer stream.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetDeadline(deadline); err != nil {
			return fmt.Errorf("%w: %v", errPeerKeyConfirmation, err)
		}
	}
	stopCancel := context.AfterFunc(ctx, func() {
		stream.CancelRead(0)
		stream.CancelWrite(0)
	})
	defer stopCancel()
	if err := confirmPeerPSK(stream, initiator, psk, crand.Reader); err != nil {
		return fmt.Errorf("%w: %v", errPeerKeyConfirmation, err)
	}
	return nil
}

// runNoiseOverQUIC authenticates the selected QUIC peer, performs the Noise NN
// handshake, and returns the derived file key plus a human-verifiable SAS.
func runNoiseOverQUIC(ctx context.Context, conn *quic.Conn, initiator bool, psk []byte, timeout time.Duration) ([]byte, string, error) {
	if timeout <= 0 {
		timeout = defaultHandshakeTO
	}
	authCtx, cancelAuth := context.WithTimeout(ctx, peerConfirmationTimeout(timeout))
	defer cancelAuth()
	if err := confirmQUICPeer(authCtx, conn, initiator, psk); err != nil {
		return nil, "", err
	}
	return runNoiseOverConfirmedQUIC(ctx, conn, initiator, psk, timeout)
}

// runNoiseOverConfirmedQUIC performs Noise after the selected QUIC connection has proved the PAKE key.
func runNoiseOverConfirmedQUIC(ctx context.Context, conn *quic.Conn, initiator bool, psk []byte, timeout time.Duration) ([]byte, string, error) {
	if timeout <= 0 {
		timeout = defaultHandshakeTO
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stream *quic.Stream
	var err error
	if initiator {
		stream, err = conn.OpenStreamSync(handshakeCtx)
	} else {
		stream, err = conn.AcceptStream(handshakeCtx)
	}
	if err != nil {
		return nil, "", err
	}
	defer stream.Close()
	if deadline, ok := handshakeCtx.Deadline(); ok {
		if err := stream.SetDeadline(deadline); err != nil {
			return nil, "", err
		}
	}
	stopCancel := context.AfterFunc(handshakeCtx, func() {
		stream.CancelRead(0)
		stream.CancelWrite(0)
	})
	defer stopCancel()
	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	hs, err := noise.NewHandshakeState(noise.Config{
		Pattern:     noise.HandshakeNN,
		Initiator:   initiator,
		CipherSuite: suite,
		Prologue:    []byte("wormzy-noise-v2"),
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
		binary.BigEndian.PutUint16(hdr[:], uint16(len(b))) // #nosec G115 -- length is bounded by maxUint16PayloadLen above.
		if err := writeAll(stream, hdr[:]); err != nil {
			return err
		}
		return writeAll(stream, b)
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
		b[23-i] ^= byte(ctr >> (8 * i)) // #nosec G115 -- extracting one intentional low byte per iteration.
	}
	return b[:]
}

func (w *aeadWriter) WriteChunk(p []byte) error {
	if len(p) > maxAEADPlaintextSize {
		return fmt.Errorf("encrypted frame plaintext exceeds %d bytes", maxAEADPlaintextSize)
	}
	n := makeNonce(w.baseNonce, w.ctr)
	ct := w.aead.Seal(nil, n, p, nil)
	if err := binary.Write(w.w, binary.BigEndian, uint32(len(ct))); err != nil { // #nosec G115 -- plaintext and AEAD overhead are bounded above.
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
	if ln < chacha20poly1305.Overhead || ln > maxAEADCiphertextSize {
		return nil, fmt.Errorf("invalid encrypted frame size %d", ln)
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
func sendFileEncrypted(ctx context.Context, conn *quic.Conn, path string, key []byte, idle time.Duration, rep Reporter) ([]byte, int64, error) {
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
	if size < 0 {
		return nil, 0, errors.New("file has an invalid negative size")
	}
	wireSize := uint64(size) // #nosec G115 -- negative sizes are rejected above.

	file, err := os.Open(path) // #nosec G304 -- path is the user-selected file being sent.
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	streamCtx, cancel := context.WithTimeout(ctx, idle)
	defer cancel()
	us, err := conn.OpenUniStreamSync(streamCtx)
	if err != nil {
		return nil, 0, err
	}
	defer us.Close()
	stopCancel := context.AfterFunc(ctx, func() {
		us.CancelWrite(0)
	})
	defer stopCancel()
	setWriteDeadline := func() {
		_ = us.SetWriteDeadline(time.Now().Add(idle))
	}
	clearDeadline := func() {
		_ = us.SetWriteDeadline(time.Time{})
	}
	setWriteDeadline()
	defer clearDeadline()

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

	header := make([]byte, fileHeaderFixedLen+len(name))
	binary.LittleEndian.PutUint16(header[0:fileHeaderNameLenSize], uint16(len(name))) // #nosec G115 -- name length is bounded above.
	binary.LittleEndian.PutUint64(header[fileHeaderNameLenSize:fileHeaderFixedLen], wireSize)
	copy(header[fileHeaderFixedLen:], []byte(name))
	if err := writer.WriteChunk(header); err != nil {
		return nil, 0, err
	}

	hasher := blake3.New()
	buf := make([]byte, chunkSize)
	var sent int64
	lastPct := -1

	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
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
		Hash:      fileHashAlgorithm,
		ChunkSize: uint32(chunkSize),
		Size:      wireSize,
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
func receiveFile(ctx context.Context, conn *quic.Conn, key []byte, downloadDir string, idle time.Duration, rep Reporter) (string, []byte, int64, error) {
	if idle <= 0 {
		idle = defaultTransferIdleTO
	}
	streamCtx, cancel := context.WithTimeout(ctx, idle)
	defer cancel()
	stream, err := conn.AcceptUniStream(streamCtx)
	if err != nil {
		return "", nil, 0, err
	}
	defer stream.CancelRead(0)
	stopCancel := context.AfterFunc(ctx, func() {
		stream.CancelRead(0)
	})
	defer stopCancel()
	setReadDeadline := func() {
		_ = stream.SetReadDeadline(time.Now().Add(idle))
	}
	clearReadDeadline := func() {
		_ = stream.SetReadDeadline(time.Time{})
	}
	setReadDeadline()
	defer clearReadDeadline()

	var base [24]byte
	if _, err := io.ReadFull(stream, base[:]); err != nil {
		return "", nil, 0, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", nil, 0, err
	}
	reader := &aeadReader{r: stream, aead: aead, baseNonce: base}

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
	out, outPath, renamed, err := createDownloadFile(targetDir, name)
	if err != nil {
		return "", nil, 0, err
	}
	keepDownload := false
	defer func() {
		if keepDownload {
			return
		}
		_ = out.Close()
		_ = os.Remove(outPath)
	}()
	if renamed && rep != nil {
		rep.Logf("target %s exists; saving as %s", filepath.Join(targetDir, name), outPath)
	}

	hasher := blake3.New()
	var written uint64
	lastPct := -1

	for written < size {
		if err := ctx.Err(); err != nil {
			return "", nil, 0, err
		}
		setReadDeadline()
		chunk, err := reader.ReadChunk()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, 0, err
		}
		if len(chunk) == 0 {
			return "", nil, 0, errors.New("received an empty file-data chunk")
		}
		remaining := size - written
		if uint64(len(chunk)) > remaining {
			return "", nil, 0, fmt.Errorf("received data exceeds advertised file size")
		}
		if _, err := hasher.Write(chunk); err != nil {
			return "", nil, 0, err
		}
		if _, err := out.Write(chunk); err != nil {
			return "", nil, 0, err
		}
		written += uint64(len(chunk))
		reportTransferProgress(rep, "Receiving", clampInt64(written), clampInt64(size), &lastPct)
	}
	reportTransferProgress(rep, "Receiving", clampInt64(size), clampInt64(size), &lastPct)
	if written != size {
		return "", nil, 0, fmt.Errorf("expected %d bytes, wrote %d", size, written)
	}

	setReadDeadline()
	sum := hasher.Sum(nil)
	if err := verifyMetadata(reader, sum, size, uint32(chunkSize)); err != nil {
		return "", nil, 0, err
	}
	if err := out.Close(); err != nil {
		return "", nil, 0, err
	}
	keepDownload = true
	return outPath, sum, clampInt64(size), nil
}

// sanitizeFilename removes path separators and terminal-control characters from peer metadata.
func sanitizeFilename(s string) string {
	var sanitized strings.Builder
	sanitized.Grow(len(s))
	for _, char := range s {
		if char == '/' || char == '\\' || unicode.IsControl(char) || unicode.In(char, unicode.Cf) {
			sanitized.WriteByte('_')
			continue
		}
		sanitized.WriteRune(char)
	}
	name := sanitized.String()
	if name == "." || name == ".." {
		return "wormzy-file"
	}
	return name
}

// createDownloadFile atomically reserves a collision-safe path beneath dir.
func createDownloadFile(dir, filename string) (*os.File, string, bool, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", false, err
	}
	defer root.Close()

	openCandidate := func(candidate string) (*os.File, error) {
		return root.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if file, err := openCandidate(filename); err == nil {
		return file, filepath.Join(dir, filename), false, nil
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, "", false, err
	}

	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	for i := 1; i <= 99; i++ {
		candidate := fmt.Sprintf("%s (wormzy-%d)%s", stem, i, ext)
		file, err := openCandidate(candidate)
		if err == nil {
			return file, filepath.Join(dir, candidate), true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", false, err
		}
	}
	for i := 0; i < 16; i++ {
		var random [4]byte
		if _, err := crand.Read(random[:]); err != nil {
			return nil, "", false, err
		}
		candidate := fmt.Sprintf("%s-%s%s", stem, hex.EncodeToString(random[:]), ext)
		file, err := openCandidate(candidate)
		if err == nil {
			return file, filepath.Join(dir, candidate), true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", false, err
		}
	}
	return nil, "", false, fmt.Errorf("unable to reserve destination for %s", filename)
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

func classifyDialError(err error) string {
	if err == nil {
		return "won"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "quic-timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "no-response"
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") {
		return "quic-timeout"
	}
	return "no-response"
}

func summarizeDirectRace(status map[string]string) string {
	if len(status) == 0 {
		return ""
	}
	keys := make([]string, 0, len(status))
	for key := range status {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+status[key])
	}
	return strings.Join(parts, ",")
}

func formatCandidateList(cands []rendezvous.Candidate) string {
	if len(cands) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(cands))
	for _, cand := range cands {
		typ := cand.Type
		if typ == "" {
			typ = "unknown"
		}
		proto := cand.Proto
		if proto == "" {
			proto = "udp"
		}
		parts = append(parts, fmt.Sprintf("%s/%s@%s(p=%d)", typ, proto, cand.Addr, cand.Priority))
	}
	return strings.Join(parts, ", ")
}

func formatDirectTargets(targets []directTarget) string {
	if len(targets) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.addr == nil {
			continue
		}
		typ := target.cand.Type
		if typ == "" {
			typ = "unknown"
		}
		parts = append(parts, fmt.Sprintf("%s@%s", typ, target.addr.String()))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// deriveSAS produces a short authentication string for human verification, mixing the
// Noise transcript with the PAKE-derived key.
func deriveSAS(transcript []byte, psk []byte) string {
	sum := blake3.Sum256(append(transcript, psk...))
	lo := binary.BigEndian.Uint16(sum[0:2]) % 10000
	hi := binary.BigEndian.Uint16(sum[2:4]) % 10000
	return fmt.Sprintf("%04d-%04d", hi, lo)
}

func punchLoop(ctx context.Context, conn *net.UDPConn, peers []*net.UDPAddr, stop <-chan struct{}, rep Reporter) {
	if conn == nil || len(peers) == 0 {
		return
	}
	interval := 150 * time.Millisecond
	ticker := time.NewTicker(interval)
	heartbeat := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	defer heartbeat.Stop()

	targets := make([]*net.UDPAddr, 0, len(peers))
	seen := make(map[string]struct{}, len(peers))
	for _, peer := range peers {
		if peer == nil {
			continue
		}
		key := peer.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		targets = append(targets, peer)
	}
	if len(targets) == 0 {
		return
	}

	started := time.Now()
	if rep != nil {
		var peerList []string
		for _, target := range targets {
			peerList = append(peerList, target.String())
		}
		rep.Logf("punch/start local=%s targets=[%s] interval=%s", conn.LocalAddr(), strings.Join(peerList, ","), interval)
	}

	var rounds int64
	var sent int64
	var errs int64
	msg := []byte("punch")
	sendRound := func() {
		rounds++
		for _, peer := range targets {
			if _, err := conn.WriteToUDP(msg, peer); err != nil {
				errs++
				continue
			}
			sent++
		}
	}

	// Send one immediate round before the first ticker edge.
	sendRound()
	for {
		select {
		case <-stop:
			if rep != nil {
				rep.Logf(
					"punch/stop reason=quic-up rounds=%d packets=%d errs=%d elapsed=%s",
					rounds,
					sent,
					errs,
					time.Since(started).Round(100*time.Millisecond),
				)
			}
			return
		case <-ctx.Done():
			if rep != nil {
				rep.Logf(
					"punch/stop reason=%v rounds=%d packets=%d errs=%d elapsed=%s",
					ctx.Err(),
					rounds,
					sent,
					errs,
					time.Since(started).Round(100*time.Millisecond),
				)
			}
			return
		case <-ticker.C:
			sendRound()
		case <-heartbeat.C:
			if rep != nil {
				rep.Logf("punch/heartbeat rounds=%d packets=%d errs=%d", rounds, sent, errs)
			}
		}
	}
}

// verifyMetadata requires one complete metadata trailer followed immediately
// by EOF and verifies it against the received transfer.
func verifyMetadata(reader *aeadReader, digest []byte, expectedSize uint64, expectedChunkSize uint32) error {
	chunk, err := reader.ReadChunk()
	if errors.Is(err, io.EOF) {
		return errors.New("missing file metadata trailer")
	}
	if err != nil {
		return fmt.Errorf("read file metadata trailer: %w", err)
	}
	if !bytes.HasPrefix(chunk, []byte(metaPrefix)) {
		return fmt.Errorf("unexpected trailer data")
	}
	var meta fileMetadata
	if err := json.Unmarshal(chunk[len(metaPrefix):], &meta); err != nil {
		return err
	}
	if meta.Hash != fileHashAlgorithm {
		return fmt.Errorf("unsupported file hash %q", meta.Hash)
	}
	if meta.ChunkSize != expectedChunkSize {
		return fmt.Errorf("file chunk size mismatch: expected %d, received %d", expectedChunkSize, meta.ChunkSize)
	}
	if meta.Size != expectedSize {
		return fmt.Errorf("file size mismatch: expected %d, received %d", expectedSize, meta.Size)
	}
	if len(digest) != fileDigestSize || len(meta.Digest) != fileDigestSize {
		return fmt.Errorf("invalid file digest length")
	}
	if !hmac.Equal(digest, meta.Digest) {
		return fmt.Errorf("file hash mismatch")
	}
	if _, err := reader.ReadChunk(); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read after file metadata trailer: %w", err)
	}
	return errors.New("unexpected encrypted chunk after file metadata trailer")
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
