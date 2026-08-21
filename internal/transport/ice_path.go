package transport

import (
	"bytes"
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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pion/ice/v4"
	pionstun "github.com/pion/stun/v3"
	"github.com/quic-go/quic-go"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	featureICEv1                = "ice-v1"
	iceAuthMACLabel             = "wormzy-ice-auth-v1"
	iceCandidatesMACLabel       = "wormzy-ice-candidates-v1"
	maxICEAuthMessageSize       = 4096
	maxICECandidatesMessageSize = 64 << 10
	maxICECredentialLength      = 256
	maxICECandidateCount        = 32
	maxICECandidateLength       = 2048
)

var errICESkipped = errors.New("ice path skipped")

type iceAuthMessage struct {
	Role  string `json:"role"`
	Ufrag string `json:"ufrag"`
	Pwd   string `json:"pwd"`
	MAC   string `json:"mac"`
}

type iceCandidatesMessage struct {
	Role       string   `json:"role"`
	Candidates []string `json:"candidates"`
	MAC        string   `json:"mac"`
}

type iceQUICSession struct {
	conn      *quic.Conn
	initiated bool
	candidate rendezvous.Candidate
	cleanup   func()
}

type icePacketConn struct {
	conn *ice.Conn
}

type iceResourceCloser interface {
	Close() error
}

type iceURLSet struct {
	urls    []*pionstun.URI
	hasSTUN bool
	hasTURN bool
}

func (p *icePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := p.conn.Read(b)
	return n, p.conn.RemoteAddr(), err
}

func (p *icePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	remote := p.conn.RemoteAddr()
	if addr != nil && remote != nil && addr.String() != remote.String() {
		return 0, fmt.Errorf("ice packet conn remote mismatch: got %s want %s", addr.String(), remote.String())
	}
	return p.conn.Write(b)
}

func (p *icePacketConn) Close() error {
	return p.conn.Close()
}

func (p *icePacketConn) LocalAddr() net.Addr {
	return p.conn.LocalAddr()
}

func (p *icePacketConn) SetDeadline(t time.Time) error {
	return p.conn.SetDeadline(t)
}

func (p *icePacketConn) SetReadDeadline(t time.Time) error {
	return p.conn.SetReadDeadline(t)
}

func (p *icePacketConn) SetWriteDeadline(t time.Time) error {
	return p.conn.SetWriteDeadline(t)
}

func (p *icePacketConn) SetReadBuffer(n int) error {
	return nil
}

func (p *icePacketConn) SetWriteBuffer(n int) error {
	return nil
}

func newICEQUICCleanup(packet, transport, agent iceResourceCloser) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, resource := range []iceResourceCloser{packet, transport, agent} {
				if resource != nil {
					_ = resource.Close()
				}
			}
		})
	}
}

func peerSupportsFeature(info rendezvous.SelfInfo, feature string) bool {
	for _, v := range info.Features {
		if strings.EqualFold(v, feature) {
			return true
		}
	}
	return false
}

func boundedDuration(value, floor, ceil time.Duration) time.Duration {
	if value <= 0 {
		value = ceil
	}
	if value < floor {
		return floor
	}
	if value > ceil {
		return ceil
	}
	return value
}

func buildICEURLs(stunServers, turnServers []string, rep Reporter) iceURLSet {
	out := iceURLSet{
		urls: make([]*pionstun.URI, 0, len(stunServers)+len(turnServers)),
	}
	for _, server := range stunServers {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		uri := server
		if !strings.Contains(server, ":") || !(strings.HasPrefix(server, "stun:") || strings.HasPrefix(server, "stuns:")) {
			uri = "stun:" + server
		}
		u, err := pionstun.ParseURI(uri)
		if err != nil {
			if rep != nil {
				rep.Logf("ice/stun uri parse failed %s: %v", RedactICEServerEndpoint(server), err)
			}
			continue
		}
		out.urls = append(out.urls, u)
		out.hasSTUN = true
	}
	for _, server := range turnServers {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		u, err := parseTURNURI(server)
		if err != nil {
			if rep != nil {
				rep.Logf("ice/turn skipped %s: %v", RedactICEServerEndpoint(server), err)
			}
			continue
		}
		out.urls = append(out.urls, u)
		out.hasTURN = true
	}
	return out
}

func parseTURNURI(raw string) (*pionstun.URI, error) {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)
	scheme := "turn"
	rest := raw
	switch {
	case strings.HasPrefix(lower, "turn://"):
		rest = raw[len("turn://"):]
	case strings.HasPrefix(lower, "turns://"):
		scheme = "turns"
		rest = raw[len("turns://"):]
	case strings.HasPrefix(lower, "turn:"):
		rest = raw[len("turn:"):]
	case strings.HasPrefix(lower, "turns:"):
		scheme = "turns"
		rest = raw[len("turns:"):]
	}

	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return nil, errors.New("username and password are required")
	}
	credentialText, endpoint := rest[:at], rest[at+1:]
	usernameText, passwordText, ok := strings.Cut(credentialText, ":")
	if !ok || usernameText == "" || passwordText == "" {
		return nil, errors.New("username and password are required")
	}
	username, err := url.PathUnescape(usernameText)
	if err != nil {
		return nil, errors.New("username has invalid escaping")
	}
	password, err := url.PathUnescape(passwordText)
	if err != nil {
		return nil, errors.New("password has invalid escaping")
	}
	if username == "" || password == "" {
		return nil, errors.New("username and password are required")
	}

	u, err := pionstun.ParseURI(scheme + ":" + endpoint)
	if err != nil {
		return nil, err
	}
	u.Username = username
	u.Password = password
	return u, nil
}

// RedactICEServerEndpoint returns a validated TURN/STUN endpoint without credentials.
func RedactICEServerEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	scheme := "turn"
	prefix := ""
	rest := raw
	lower := strings.ToLower(raw)
	for _, candidate := range []struct {
		prefix string
		scheme string
	}{
		{prefix: "turn://", scheme: "turn"},
		{prefix: "turns://", scheme: "turns"},
		{prefix: "stun://", scheme: "stun"},
		{prefix: "stuns://", scheme: "stuns"},
		{prefix: "turn:", scheme: "turn"},
		{prefix: "turns:", scheme: "turns"},
		{prefix: "stun:", scheme: "stun"},
		{prefix: "stuns:", scheme: "stuns"},
	} {
		if strings.HasPrefix(lower, candidate.prefix) {
			scheme = candidate.scheme
			prefix = candidate.prefix
			rest = raw[len(candidate.prefix):]
			break
		}
	}
	credentialMarker := ""
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		credentialMarker = "***@"
		rest = rest[at+1:]
	}
	parsed, err := pionstun.ParseURI(scheme + ":" + rest)
	if err != nil {
		return "(invalid endpoint redacted)"
	}
	endpoint := net.JoinHostPort(parsed.Host, strconv.Itoa(parsed.Port))
	if strings.Contains(rest, "?") && (scheme == "turn" || scheme == "turns") {
		endpoint += "?transport=" + parsed.Proto.String()
	}
	return prefix + credentialMarker + endpoint
}

// newICEAuthMessage creates a PAKE-authenticated ICE credential message.
func newICEAuthMessage(code, role string, psk []byte, ufrag, pwd string) (iceAuthMessage, error) {
	message := iceAuthMessage{Role: role, Ufrag: ufrag, Pwd: pwd}
	if err := validateICEAuthMessage(message); err != nil {
		return iceAuthMessage{}, err
	}
	if code == "" || len(psk) == 0 {
		return iceAuthMessage{}, errors.New("missing ICE authentication key material")
	}
	message.MAC = iceSignalingMAC(iceAuthMACLabel, code, role, []string{ufrag, pwd}, psk)
	return message, nil
}

// verifyICEAuthMessage verifies ICE credentials and their PAKE-keyed authenticator.
func verifyICEAuthMessage(message iceAuthMessage, code, expectedRole string, psk []byte) error {
	if code == "" || len(psk) == 0 {
		return errors.New("missing ICE authentication key material")
	}
	if message.Role != expectedRole {
		return fmt.Errorf("ICE credential role %q; want %q", message.Role, expectedRole)
	}
	if err := validateICEAuthMessage(message); err != nil {
		return err
	}
	want := iceSignalingMAC(iceAuthMACLabel, code, message.Role, []string{message.Ufrag, message.Pwd}, psk)
	if !equalHexMAC(message.MAC, want) {
		return errors.New("ICE credential authentication failed")
	}
	return nil
}

// validateICEAuthMessage enforces strict role and credential bounds.
func validateICEAuthMessage(message iceAuthMessage) error {
	if message.Role != "send" && message.Role != "recv" {
		return fmt.Errorf("invalid ICE credential role %q", message.Role)
	}
	for name, value := range map[string]string{"ufrag": message.Ufrag, "password": message.Pwd} {
		if len(value) == 0 || len(value) > maxICECredentialLength || !printableASCII(value, false) {
			return fmt.Errorf("ICE %s is invalid", name)
		}
	}
	return nil
}

// decodeICEAuthMessage strictly decodes a bounded ICE credential message.
func decodeICEAuthMessage(raw json.RawMessage) (iceAuthMessage, error) {
	var message iceAuthMessage
	if err := decodeStrictICEMessage(raw, maxICEAuthMessageSize, &message); err != nil {
		return iceAuthMessage{}, fmt.Errorf("decode ICE credentials: %w", err)
	}
	return message, nil
}

// newICECandidatesMessage creates a PAKE-authenticated ICE candidate batch.
func newICECandidatesMessage(code, role string, psk []byte, candidates []string) (iceCandidatesMessage, error) {
	message := iceCandidatesMessage{Role: role, Candidates: append([]string(nil), candidates...)}
	if err := validateICECandidatesMessage(message); err != nil {
		return iceCandidatesMessage{}, err
	}
	if code == "" || len(psk) == 0 {
		return iceCandidatesMessage{}, errors.New("missing ICE candidate authentication key material")
	}
	message.MAC = iceSignalingMAC(iceCandidatesMACLabel, code, role, message.Candidates, psk)
	return message, nil
}

// verifyICECandidatesMessage verifies a bounded candidate batch and its PAKE-keyed authenticator.
func verifyICECandidatesMessage(message iceCandidatesMessage, code, expectedRole string, psk []byte) error {
	if code == "" || len(psk) == 0 {
		return errors.New("missing ICE candidate authentication key material")
	}
	if message.Role != expectedRole {
		return fmt.Errorf("ICE candidate role %q; want %q", message.Role, expectedRole)
	}
	if err := validateICECandidatesMessage(message); err != nil {
		return err
	}
	want := iceSignalingMAC(iceCandidatesMACLabel, code, message.Role, message.Candidates, psk)
	if !equalHexMAC(message.MAC, want) {
		return errors.New("ICE candidate authentication failed")
	}
	return nil
}

// validateICECandidatesMessage enforces candidate count, length, and terminal-safety bounds.
func validateICECandidatesMessage(message iceCandidatesMessage) error {
	if message.Role != "send" && message.Role != "recv" {
		return fmt.Errorf("invalid ICE candidate role %q", message.Role)
	}
	if len(message.Candidates) > maxICECandidateCount {
		return fmt.Errorf("ICE candidate count exceeds limit of %d", maxICECandidateCount)
	}
	for _, candidate := range message.Candidates {
		if len(candidate) == 0 || len(candidate) > maxICECandidateLength || !printableASCII(candidate, true) {
			return errors.New("ICE candidate contains invalid text")
		}
	}
	return nil
}

// decodeICECandidatesMessage strictly decodes a bounded ICE candidate batch.
func decodeICECandidatesMessage(raw json.RawMessage) (iceCandidatesMessage, error) {
	var message iceCandidatesMessage
	if err := decodeStrictICEMessage(raw, maxICECandidatesMessageSize, &message); err != nil {
		return iceCandidatesMessage{}, fmt.Errorf("decode ICE candidates: %w", err)
	}
	return message, nil
}

// decodeStrictICEMessage decodes one JSON object with bounded size and no unknown or trailing data.
func decodeStrictICEMessage(raw json.RawMessage, maxSize int, dst any) error {
	if len(raw) == 0 || len(raw) > maxSize {
		return errors.New("invalid ICE message size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("ICE message contains trailing data")
	}
	return nil
}

// iceSignalingMAC computes a length-delimited, domain-separated signaling authenticator.
func iceSignalingMAC(label, code, role string, fields []string, psk []byte) string {
	mac := hmac.New(sha256.New, psk)
	for _, field := range append([]string{label, code, role}, fields...) {
		_, _ = mac.Write([]byte(strconv.Itoa(len(field))))
		_, _ = mac.Write([]byte{':'})
		_, _ = mac.Write([]byte(field))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// equalHexMAC compares encoded authenticators without leaking comparison timing.
func equalHexMAC(gotText, wantText string) bool {
	got, err := hex.DecodeString(gotText)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	want, err := hex.DecodeString(wantText)
	return err == nil && hmac.Equal(got, want)
}

// printableASCII rejects control, non-ASCII, and optionally space characters.
func printableASCII(value string, allowSpace bool) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char > 0x7e || char < 0x20 || (!allowSpace && char == 0x20) {
			return false
		}
	}
	return true
}

// validateRemoteICECandidate limits authenticated peer candidates to UDP4
// endpoints that ICE can safely probe. Private host candidates remain allowed
// for same-LAN connectivity, while special-use targets and private relay/STUN
// claims are rejected.
func validateRemoteICECandidate(candidate ice.Candidate, loopback bool, localPrivateSubnets []*net.IPNet) error {
	if candidate == nil {
		return errors.New("ICE candidate is nil")
	}
	if candidate.NetworkType() != ice.NetworkTypeUDP4 || candidate.Component() != 1 {
		return errors.New("ICE candidate must use UDP4 component 1")
	}
	if candidate.Port() <= 0 || candidate.Port() > 65535 {
		return errors.New("ICE candidate port is invalid")
	}
	ip := net.ParseIP(candidate.Address()).To4()
	if ip == nil {
		return errors.New("ICE candidate address must be a literal IPv4 address")
	}
	if ip.IsLoopback() {
		if loopback && candidate.Type() == ice.CandidateTypeHost {
			return nil
		}
		return errors.New("ICE loopback candidate is disabled")
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || !ip.IsGlobalUnicast() {
		return errors.New("ICE candidate address is not usable")
	}
	switch candidate.Type() {
	case ice.CandidateTypeHost:
		if ip.IsPrivate() {
			if !ipInDirectlyConnectedSubnet(ip, localPrivateSubnets) {
				return errors.New("ICE private host candidate is outside a directly connected subnet")
			}
			return nil
		}
		if !isUsableExternalIPv4(ip) {
			return errors.New("ICE host candidate must use a connected private or public IPv4 address")
		}
		return nil
	case ice.CandidateTypeServerReflexive, ice.CandidateTypePeerReflexive, ice.CandidateTypeRelay:
		if !isUsableExternalIPv4(ip) {
			return errors.New("ICE non-host candidate must use a public IPv4 address")
		}
		return nil
	default:
		return errors.New("ICE candidate type is unsupported")
	}
}

// localPrivateIPv4Subnets returns bounded RFC 1918 networks configured on active interfaces.
func localPrivateIPv4Subnets() ([]*net.IPNet, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list local interfaces for ICE validation: %w", err)
	}
	const maxSubnets = 64
	subnets := make([]*net.IPNet, 0)
	seen := make(map[string]struct{})
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ipNet, ok := address.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			network := &net.IPNet{IP: append(net.IP(nil), ip.Mask(ipNet.Mask)...), Mask: append(net.IPMask(nil), ipNet.Mask...)}
			key := network.String()
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			subnets = append(subnets, network)
			if len(subnets) == maxSubnets {
				return subnets, nil
			}
		}
	}
	return subnets, nil
}

// ipInDirectlyConnectedSubnet reports whether an IPv4 address belongs to an approved local network.
func ipInDirectlyConnectedSubnet(ip net.IP, subnets []*net.IPNet) bool {
	ip = ip.To4()
	if ip == nil {
		return false
	}
	for _, subnet := range subnets {
		if subnet != nil && subnet.Contains(ip) {
			return true
		}
	}
	return false
}

// runICEConnectWithSignal establishes ICE and reports when connectivity checks are about to start.
func runICEConnectWithSignal(
	ctx context.Context,
	cfg Config,
	mbox mailbox,
	rep Reporter,
	code string,
	psk []byte,
	checksStarted func(),
) (*ice.Agent, *ice.Conn, error) {
	stunServers := cfg.stunServers()
	turnServers := cfg.turnServers()
	serverSet := buildICEURLs(stunServers, turnServers, rep)
	if rep != nil {
		rep.Logf(
			"ice/servers configured stun=%d turn=%d usable=%d",
			len(stunServers),
			len(turnServers),
			len(serverSet.urls),
		)
	}

	candidateTypes := []ice.CandidateType{ice.CandidateTypeHost}
	if serverSet.hasSTUN {
		candidateTypes = append(candidateTypes, ice.CandidateTypeServerReflexive)
	}
	// TURN enables relay candidates inside ICE before custom fallback relay.
	if serverSet.hasTURN {
		candidateTypes = append(candidateTypes, ice.CandidateTypeRelay)
	}

	check := 100 * time.Millisecond
	keepAlive := 1 * time.Second
	disconnected := 4 * time.Second
	failed := 8 * time.Second

	agent, err := ice.NewAgent(&ice.AgentConfig{
		Urls:                serverSet.urls,
		NetworkTypes:        []ice.NetworkType{ice.NetworkTypeUDP4},
		CandidateTypes:      candidateTypes,
		CheckInterval:       &check,
		KeepaliveInterval:   &keepAlive,
		DisconnectedTimeout: &disconnected,
		FailedTimeout:       &failed,
		IncludeLoopback:     cfg.Loopback,
	})
	if err != nil {
		return nil, nil, err
	}

	if err := agent.OnConnectionStateChange(func(state ice.ConnectionState) {
		if rep != nil {
			rep.Logf("ice/state %s", strings.ToLower(state.String()))
		}
	}); err != nil {
		_ = agent.Close()
		return nil, nil, err
	}

	var (
		candMu     sync.Mutex
		localCands []string
		doneOnce   sync.Once
	)
	gatherDone := make(chan struct{})
	if err := agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			doneOnce.Do(func() { close(gatherDone) })
			return
		}
		candMu.Lock()
		localCands = append(localCands, c.Marshal())
		candMu.Unlock()
	}); err != nil {
		_ = agent.Close()
		return nil, nil, err
	}
	if err := agent.GatherCandidates(); err != nil {
		_ = agent.Close()
		return nil, nil, err
	}

	gatherWait := boundedDuration(cfg.HandshakeTimeout/12, 800*time.Millisecond, 2500*time.Millisecond)
	select {
	case <-gatherDone:
	case <-time.After(gatherWait):
		if rep != nil {
			rep.Logf("ice/gather timeout after %s; continuing with partial candidates", gatherWait)
		}
	case <-ctx.Done():
		_ = agent.Close()
		return nil, nil, ctx.Err()
	}

	candMu.Lock()
	localCopy := append([]string{}, localCands...)
	candMu.Unlock()
	if rep != nil {
		rep.Logf("ice/local candidates gathered=%d", len(localCopy))
	}

	ufrag, pwd, err := agent.GetLocalUserCredentials()
	if err != nil {
		_ = agent.Close()
		return nil, nil, err
	}
	localAuth, err := newICEAuthMessage(code, cfg.Mode, psk, ufrag, pwd)
	if err != nil {
		_ = agent.Close()
		return nil, nil, err
	}
	localCandidates, err := newICECandidatesMessage(code, cfg.Mode, psk, localCopy)
	if err != nil {
		_ = agent.Close()
		return nil, nil, err
	}
	if err := mbox.Send(ctx, "ice-auth", localAuth); err != nil {
		_ = agent.Close()
		return nil, nil, err
	}
	if err := mbox.Send(ctx, "ice-candidates", localCandidates); err != nil {
		_ = agent.Close()
		return nil, nil, err
	}

	var (
		remoteAuth  iceAuthMessage
		remoteCands iceCandidatesMessage
		haveAuth    bool
		haveCands   bool
	)
	for !(haveAuth && haveCands) {
		msg, err := receiveMailboxType(ctx, mbox, "ice-auth", "ice-candidates")
		if err != nil {
			_ = agent.Close()
			return nil, nil, err
		}
		switch msg.Type {
		case "ice-auth":
			remoteAuth, err = decodeICEAuthMessage(msg.Body)
			if err != nil {
				_ = agent.Close()
				return nil, nil, err
			}
			if err := verifyICEAuthMessage(remoteAuth, code, oppositeRole(cfg.Mode), psk); err != nil {
				_ = agent.Close()
				return nil, nil, err
			}
			haveAuth = true
		case "ice-candidates":
			remoteCands, err = decodeICECandidatesMessage(msg.Body)
			if err != nil {
				_ = agent.Close()
				return nil, nil, err
			}
			if err := verifyICECandidatesMessage(remoteCands, code, oppositeRole(cfg.Mode), psk); err != nil {
				_ = agent.Close()
				return nil, nil, err
			}
			haveCands = true
		}
	}

	privateSubnets, subnetErr := localPrivateIPv4Subnets()
	if subnetErr != nil && rep != nil {
		rep.Logf("ice/local subnet discovery failed: %v", subnetErr)
	}
	addedRemoteCandidates := 0
	for _, raw := range remoteCands.Candidates {
		cand, err := ice.UnmarshalCandidate(raw)
		if err != nil {
			if rep != nil {
				rep.Logf("ice/remote candidate parse failed: %v", err)
			}
			continue
		}
		if err := validateRemoteICECandidate(cand, cfg.Loopback, privateSubnets); err != nil {
			if rep != nil {
				rep.Logf("ice/remote candidate rejected: %v", err)
			}
			continue
		}
		if err := agent.AddRemoteCandidate(cand); err != nil {
			if rep != nil {
				rep.Logf("ice/add remote candidate failed: %v", err)
			}
			continue
		}
		addedRemoteCandidates++
	}
	if addedRemoteCandidates == 0 {
		_ = agent.Close()
		return nil, nil, errors.New("peer did not provide any safe ICE candidates")
	}
	_ = agent.AddRemoteCandidate(nil)
	if rep != nil {
		rep.Logf("ice/remote candidates added=%d", addedRemoteCandidates)
	}
	if checksStarted != nil {
		checksStarted()
	}

	var conn *ice.Conn
	if cfg.Mode == "send" {
		conn, err = agent.Dial(ctx, remoteAuth.Ufrag, remoteAuth.Pwd)
	} else {
		conn, err = agent.Accept(ctx, remoteAuth.Ufrag, remoteAuth.Pwd)
	}
	if err != nil {
		_ = agent.Close()
		return nil, nil, err
	}
	return agent, conn, nil
}

// attemptICEQUICSession runs authenticated ICE signaling and establishes QUIC over the winning pair.
func attemptICEQUICSession(
	ctx context.Context,
	cfg Config,
	mbox mailbox,
	rep Reporter,
	peer rendezvous.SelfInfo,
	code string,
	psk []byte,
	checksStarted func(),
) (*iceQUICSession, error) {
	if !peerSupportsFeature(peer, featureICEv1) {
		return nil, errICESkipped
	}

	rep.Logf("ice/attempt peer feature %s detected", featureICEv1)
	iceBudget := boundedDuration(cfg.HandshakeTimeout/8, 3*time.Second, 8*time.Second)
	iceCtx, cancelICE := context.WithTimeout(ctx, iceBudget)
	defer cancelICE()

	agent, iceConn, err := runICEConnectWithSignal(iceCtx, cfg, mbox, rep, code, psk, checksStarted)
	if err != nil {
		return nil, err
	}
	packetConn := &icePacketConn{conn: iceConn}

	serverTLS, err := selfSignedTLS()
	if err != nil {
		_ = agent.Close()
		return nil, err
	}
	serverTLS.NextProtos = []string{alpn}
	// ICE credentials and the following PAKE proof authenticate the peer; the
	// short-lived QUIC TLS certificate provides only transport encryption.
	clientTLS := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}} // #nosec G402 -- PAKE authenticates the selected peer.
	quicConf := &quic.Config{
		KeepAlivePeriod:      15 * time.Second,
		MaxIdleTimeout:       cfg.IdleTimeout,
		HandshakeIdleTimeout: cfg.HandshakeTimeout,
	}

	quicTransport := &quic.Transport{Conn: packetConn}
	ln, err := quicTransport.Listen(serverTLS, quicConf)
	if err != nil {
		_ = agent.Close()
		return nil, err
	}

	cleanup := newICEQUICCleanup(packetConn, quicTransport, agent)

	var (
		quicConn  *quic.Conn
		initiated bool
	)
	remoteAddr := iceConn.RemoteAddr()
	if remoteAddr == nil {
		cleanup()
		return nil, fmt.Errorf("ice selected pair missing remote address")
	}
	selectedPair, err := agent.GetSelectedCandidatePair()
	if err != nil && rep != nil {
		rep.Logf("ice/selected pair unavailable: %v", err)
	}
	if cfg.Mode == "send" {
		initiated = true
		remoteUDP, err := net.ResolveUDPAddr("udp4", remoteAddr.String())
		if err != nil {
			cleanup()
			return nil, err
		}
		var dialErr error
		for attempt := 1; attempt <= 3; attempt++ {
			rep.Logf("ice/quic dial attempt=%d target=%s", attempt, remoteUDP)
			quicConn, dialErr = quicTransport.Dial(iceCtx, remoteUDP, clientTLS, quicConf)
			if dialErr == nil && quicConn != nil {
				break
			}
			if attempt < 3 {
				select {
				case <-iceCtx.Done():
				case <-time.After(150 * time.Millisecond):
				}
			}
		}
		if dialErr != nil {
			cleanup()
			return nil, dialErr
		}
	} else {
		rep.Logf("ice/quic accept waiting on %s", packetConn.LocalAddr())
		quicConn, err = ln.Accept(iceCtx)
		if err != nil {
			cleanup()
			return nil, err
		}
	}

	candType := "ice-p2p"
	candPriority := 200
	if selectedPair != nil {
		localType := strings.ToLower(selectedPair.Local.Type().String())
		remoteType := strings.ToLower(selectedPair.Remote.Type().String())
		if rep != nil {
			rep.Logf("ice/selected pair local=%s remote=%s", localType, remoteType)
		}
		// If either side selected a relay candidate, treat the path as relayed.
		if localType == "relay" || remoteType == "relay" {
			candType = "ice-relay"
			candPriority = 80
		}
	}
	cand := rendezvous.Candidate{
		Type:     candType,
		Proto:    "udp",
		Addr:     remoteAddr.String(),
		Priority: candPriority,
	}
	return &iceQUICSession{
		conn:      quicConn,
		initiated: initiated,
		candidate: cand,
		cleanup:   cleanup,
	}, nil
}
