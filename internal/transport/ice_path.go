package transport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v2"
	pionstun "github.com/pion/stun"
	"github.com/quic-go/quic-go"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const featureICEv1 = "ice-v1"

var errICESkipped = errors.New("ice path skipped")

type iceAuthMessage struct {
	Ufrag string `json:"ufrag"`
	Pwd   string `json:"pwd"`
}

type iceCandidatesMessage struct {
	Candidates []string `json:"candidates"`
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
				rep.Logf("ice/stun uri parse failed %s: %v", redactICEEndpoint(server), err)
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
		uri := server
		switch {
		case strings.HasPrefix(server, "turn://"):
			uri = "turn:" + strings.TrimPrefix(server, "turn://")
		case strings.HasPrefix(server, "turns://"):
			uri = "turns:" + strings.TrimPrefix(server, "turns://")
		case strings.HasPrefix(server, "turn:"), strings.HasPrefix(server, "turns:"):
			// already normalized
		default:
			// Host:port entries are accepted for convenience; default to UDP.
			if strings.Contains(server, "?") {
				uri = "turn:" + server
			} else {
				uri = "turn:" + server + "?transport=udp"
			}
		}
		u, err := pionstun.ParseURI(uri)
		if err != nil {
			if rep != nil {
				rep.Logf("ice/turn uri parse failed %s: %v", redactICEEndpoint(server), err)
			}
			continue
		}
		out.urls = append(out.urls, u)
		out.hasTURN = true
	}
	return out
}

func redactICEEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	at := strings.LastIndex(raw, "@")
	if at == -1 {
		return raw
	}
	// Keep scheme/host, strip credential material from logs.
	colon := strings.Index(raw, ":")
	if colon == -1 || colon > at {
		return "***@" + raw[at+1:]
	}
	return raw[:colon+1] + "***@" + raw[at+1:]
}

func runICEConnect(ctx context.Context, cfg Config, mbox mailbox, rep Reporter) (*ice.Agent, *ice.Conn, error) {
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
	if err := mbox.Send(ctx, "ice-auth", iceAuthMessage{Ufrag: ufrag, Pwd: pwd}); err != nil {
		_ = agent.Close()
		return nil, nil, err
	}
	if err := mbox.Send(ctx, "ice-candidates", iceCandidatesMessage{Candidates: localCopy}); err != nil {
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
		msg, err := mbox.Receive(ctx)
		if err != nil {
			_ = agent.Close()
			return nil, nil, err
		}
		switch msg.Type {
		case "ice-auth":
			if err := json.Unmarshal(msg.Body, &remoteAuth); err != nil {
				_ = agent.Close()
				return nil, nil, err
			}
			haveAuth = remoteAuth.Ufrag != "" && remoteAuth.Pwd != ""
		case "ice-candidates":
			if err := json.Unmarshal(msg.Body, &remoteCands); err != nil {
				_ = agent.Close()
				return nil, nil, err
			}
			haveCands = true
		default:
			if rep != nil {
				rep.Logf("ice/mailbox ignoring message type=%s", msg.Type)
			}
		}
	}

	for _, raw := range remoteCands.Candidates {
		cand, err := ice.UnmarshalCandidate(raw)
		if err != nil {
			if rep != nil {
				rep.Logf("ice/remote candidate parse failed: %v", err)
			}
			continue
		}
		if err := agent.AddRemoteCandidate(cand); err != nil {
			if rep != nil {
				rep.Logf("ice/add remote candidate failed: %v", err)
			}
		}
	}
	_ = agent.AddRemoteCandidate(nil)
	if rep != nil {
		rep.Logf("ice/remote candidates added=%d", len(remoteCands.Candidates))
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

func attemptICEQUICSession(ctx context.Context, cfg Config, mbox mailbox, rep Reporter, peer rendezvous.SelfInfo) (*iceQUICSession, error) {
	if !peerSupportsFeature(peer, featureICEv1) {
		return nil, errICESkipped
	}

	rep.Logf("ice/attempt peer feature %s detected", featureICEv1)
	iceBudget := boundedDuration(cfg.HandshakeTimeout/8, 3*time.Second, 8*time.Second)
	iceCtx, cancelICE := context.WithTimeout(ctx, iceBudget)
	defer cancelICE()

	agent, iceConn, err := runICEConnect(iceCtx, cfg, mbox, rep)
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
	clientTLS := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}}
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

	cleanup := func() {
		_ = ln.Close()
		_ = quicTransport.Close()
		_ = agent.Close()
	}

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
