package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
	"github.com/pion/ice/v4"
)

type iceTestMailbox struct {
	outbound chan<- mailboxMessage
	inbound  <-chan mailboxMessage
}

type iceOrderReporter struct {
	mu     sync.Mutex
	events []string
}

// Stage satisfies Reporter for connectivity-check ordering tests.
func (r *iceOrderReporter) Stage(Stage, StageState, string) {}

// Logf records ICE lifecycle messages in call order.
func (r *iceOrderReporter) Logf(format string, args ...interface{}) {
	r.record(fmt.Sprintf(format, args...))
}

// record appends one lifecycle marker safely.
func (r *iceOrderReporter) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

// snapshot returns a stable copy of the recorded lifecycle markers.
func (r *iceOrderReporter) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (m *iceTestMailbox) Claim(context.Context, string) (string, error) { return "", nil }

func (m *iceTestMailbox) StoreSelf(context.Context, rendezvous.SelfInfo) error { return nil }

func (m *iceTestMailbox) WaitPeer(context.Context) (*rendezvous.SelfInfo, error) {
	return &rendezvous.SelfInfo{}, nil
}

func (m *iceTestMailbox) Send(ctx context.Context, typ string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	select {
	case m.outbound <- mailboxMessage{Type: typ, Body: raw}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *iceTestMailbox) Receive(ctx context.Context) (mailboxMessage, error) {
	select {
	case msg := <-m.inbound:
		return msg, nil
	case <-ctx.Done():
		return mailboxMessage{}, ctx.Err()
	}
}

func (m *iceTestMailbox) ReportStats(context.Context, transferStats) error { return nil }

func (m *iceTestMailbox) Close() error { return nil }

func TestRunICEConnect_Loopback(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	toReceiver := make(chan mailboxMessage, 4)
	toSender := make(chan mailboxMessage, 4)
	senderMailbox := &iceTestMailbox{outbound: toReceiver, inbound: toSender}
	receiverMailbox := &iceTestMailbox{outbound: toSender, inbound: toReceiver}

	type connectResult struct {
		mode   string
		agent  iceResourceCloser
		conn   net.Conn
		events []string
		err    error
	}
	results := make(chan connectResult, 2)
	connect := func(mode string, mbox mailbox) {
		// Non-empty blank lists suppress production defaults without contacting
		// public STUN or TURN services during this loopback test.
		order := &iceOrderReporter{}
		agent, conn, err := runICEConnectWithSignal(ctx, Config{
			Mode:             mode,
			Loopback:         true,
			STUNServers:      []string{" "},
			TURNServers:      []string{" "},
			HandshakeTimeout: 10 * time.Second,
		}, mbox, order, "ice-test-code", []byte("shared ICE signaling test key"), func() {
			order.record("checks-started")
		})
		results <- connectResult{mode: mode, agent: agent, conn: conn, events: order.snapshot(), err: err}
	}

	go connect("send", senderMailbox)
	go connect("recv", receiverMailbox)

	var senderConn, receiverConn net.Conn
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s ICE connect failed: %v", result.mode, result.err)
		}
		localIndex, remoteIndex, checksIndex := -1, -1, -1
		for i, event := range result.events {
			switch {
			case strings.HasPrefix(event, "ice/local candidates gathered="):
				localIndex = i
			case strings.HasPrefix(event, "ice/remote candidates added="):
				remoteIndex = i
			case event == "checks-started":
				checksIndex = i
			}
		}
		if localIndex < 0 || remoteIndex <= localIndex || checksIndex <= remoteIndex {
			t.Fatalf("%s ICE check signal order is invalid: %v", result.mode, result.events)
		}
		t.Cleanup(func() {
			_ = result.conn.Close()
			_ = result.agent.Close()
		})
		if result.mode == "send" {
			senderConn = result.conn
		} else {
			receiverConn = result.conn
		}
	}

	payload := []byte("wormzy-pion-v4")
	if _, err := senderConn.Write(payload); err != nil {
		t.Fatalf("write over ICE: %v", err)
	}
	if err := receiverConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set ICE read deadline: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(receiverConn, got); err != nil {
		t.Fatalf("read over ICE: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("ICE payload mismatch: got %q want %q", got, payload)
	}
}

// TestICESignalingAuthentication_RejectsTampering verifies credentials and candidates are PAKE-bound.
func TestICESignalingAuthentication_RejectsTampering(t *testing.T) {
	psk := []byte("shared ICE signaling test key")
	auth, err := newICEAuthMessage("code-1", "send", psk, "local-ufrag", "local-password")
	if err != nil {
		t.Fatalf("newICEAuthMessage: %v", err)
	}
	if err := verifyICEAuthMessage(auth, "code-1", "send", psk); err != nil {
		t.Fatalf("verifyICEAuthMessage: %v", err)
	}
	auth.Ufrag = "attacker-ufrag"
	if err := verifyICEAuthMessage(auth, "code-1", "send", psk); err == nil {
		t.Fatal("accepted tampered ICE credentials")
	}

	candidates, err := newICECandidatesMessage("code-1", "recv", psk, []string{
		"candidate:1 1 udp 2130706431 192.0.2.10 5000 typ host",
	})
	if err != nil {
		t.Fatalf("newICECandidatesMessage: %v", err)
	}
	if err := verifyICECandidatesMessage(candidates, "code-1", "recv", psk); err != nil {
		t.Fatalf("verifyICECandidatesMessage: %v", err)
	}
	candidates.Candidates[0] = "candidate:1 1 udp 2130706431 10.0.0.9 22 typ host"
	if err := verifyICECandidatesMessage(candidates, "code-1", "recv", psk); err == nil {
		t.Fatal("accepted tampered ICE candidates")
	}
}

// TestICECandidateMessage_RejectsUnboundedOrMalformedInput verifies strict signaling limits.
func TestICECandidateMessage_RejectsUnboundedOrMalformedInput(t *testing.T) {
	tooMany := make([]string, maxICECandidateCount+1)
	for i := range tooMany {
		tooMany[i] = "candidate:1 1 udp 1 192.0.2.1 5000 typ host"
	}
	if _, err := newICECandidatesMessage("code-1", "send", []byte("key"), tooMany); err == nil {
		t.Fatal("accepted excessive ICE candidates")
	}

	malformed := []json.RawMessage{
		[]byte(`{"role":"send","candidates":[],"mac":"x","extra":true}`),
		[]byte(`{"role":"send","candidates":[],"mac":"x"}{}`),
		[]byte(strings.Repeat("x", maxICECandidatesMessageSize+1)),
	}
	for _, raw := range malformed {
		if _, err := decodeICECandidatesMessage(raw); err == nil {
			t.Fatal("accepted malformed ICE candidate message")
		}
	}
}

// TestValidateRemoteICECandidateRestrictsProbeTargets verifies special-use
// endpoints are rejected while authenticated same-LAN hosts remain available.
func TestValidateRemoteICECandidateRestrictsProbeTargets(t *testing.T) {
	_, localSubnet, err := net.ParseCIDR("192.168.1.10/24")
	if err != nil {
		t.Fatalf("parse local subnet: %v", err)
	}
	tests := []struct {
		name     string
		raw      string
		loopback bool
		wantErr  bool
	}{
		{name: "private host", raw: "candidate:1 1 udp 2130706431 192.168.1.20 5000 typ host"},
		{name: "foreign private host", raw: "candidate:9 1 udp 2130706431 10.20.30.40 5000 typ host", wantErr: true},
		{name: "CGNAT host", raw: "candidate:10 1 udp 2130706431 100.64.1.20 5000 typ host", wantErr: true},
		{name: "documentation host", raw: "candidate:11 1 udp 2130706431 192.0.2.20 5000 typ host", wantErr: true},
		{name: "benchmark host", raw: "candidate:12 1 udp 2130706431 198.18.0.20 5000 typ host", wantErr: true},
		{name: "public reflexive", raw: "candidate:2 1 udp 1694498815 8.8.8.8 5001 typ srflx raddr 192.168.1.20 rport 5000"},
		{name: "private reflexive", raw: "candidate:3 1 udp 1694498815 192.168.1.20 5001 typ srflx raddr 192.168.1.20 rport 5000", wantErr: true},
		{name: "loopback production", raw: "candidate:4 1 udp 2130706431 127.0.0.1 5002 typ host", wantErr: true},
		{name: "loopback development", raw: "candidate:5 1 udp 2130706431 127.0.0.1 5003 typ host", loopback: true},
		{name: "unspecified", raw: "candidate:6 1 udp 2130706431 0.0.0.0 5004 typ host", wantErr: true},
		{name: "multicast", raw: "candidate:7 1 udp 2130706431 224.0.0.1 5005 typ host", wantErr: true},
		{name: "hostname", raw: "candidate:8 1 udp 2130706431 attacker.local 5006 typ host", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate, err := ice.UnmarshalCandidate(tt.raw)
			if err != nil {
				t.Fatalf("unmarshal candidate: %v", err)
			}
			err = validateRemoteICECandidate(candidate, tt.loopback, []*net.IPNet{localSubnet})
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRemoteICECandidate error = %v; wantErr=%t", err, tt.wantErr)
			}
		})
	}
}

func TestPeerSupportsFeatureCaseInsensitive(t *testing.T) {
	peer := rendezvous.SelfInfo{Features: []string{"ICE-V1", "foo"}}
	if !peerSupportsFeature(peer, "ice-v1") {
		t.Fatalf("expected feature match")
	}
	if peerSupportsFeature(peer, "missing") {
		t.Fatalf("unexpected match for missing feature")
	}
}

func TestBoundedDurationClamp(t *testing.T) {
	floor := 2 * time.Second
	ceil := 10 * time.Second
	if got := boundedDuration(0, floor, ceil); got != ceil {
		t.Fatalf("value=0 expected ceil %s got %s", ceil, got)
	}
	if got := boundedDuration(500*time.Millisecond, floor, ceil); got != floor {
		t.Fatalf("below floor expected %s got %s", floor, got)
	}
	if got := boundedDuration(15*time.Second, floor, ceil); got != ceil {
		t.Fatalf("above ceil expected %s got %s", ceil, got)
	}
	if got := boundedDuration(4*time.Second, floor, ceil); got != 4*time.Second {
		t.Fatalf("within bounds expected unchanged, got %s", got)
	}
}

func TestBuildICEURLs_STUNAndTURN(t *testing.T) {
	set := buildICEURLs(
		[]string{"stun.l.google.com:19302"},
		[]string{"turn:user:pass@turn.example.com:3478?transport=udp"},
		nil,
	)
	if !set.hasSTUN {
		t.Fatalf("expected STUN support")
	}
	if !set.hasTURN {
		t.Fatalf("expected TURN support")
	}
	if len(set.urls) != 2 {
		t.Fatalf("expected 2 urls, got %d", len(set.urls))
	}
	turn := set.urls[1]
	if turn.Username != "user" || turn.Password != "pass" {
		t.Fatalf("unexpected TURN credentials: username=%q password=%q", turn.Username, turn.Password)
	}
}

func TestBuildICEURLs_SkipsTURNWithoutCredentials(t *testing.T) {
	set := buildICEURLs(nil, []string{"turn.example.com:3478"}, nil)
	if set.hasTURN {
		t.Fatalf("unexpected TURN support without credentials")
	}
	if len(set.urls) != 0 {
		t.Fatalf("expected credential-less TURN URL to be skipped, got %d url(s)", len(set.urls))
	}
}

func TestBuildICEURLs_NormalizesAuthenticatedTURN(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		username string
		password string
		wantURI  string
	}{
		{
			name:     "opaque URI",
			raw:      "turn:user:pass@turn.example.com:3478?transport=udp",
			username: "user",
			password: "pass",
			wantURI:  "turn:turn.example.com:3478?transport=udp",
		},
		{
			name:     "double slash URI",
			raw:      "turn://user:pass@turn.example.com:3478?transport=udp",
			username: "user",
			password: "pass",
			wantURI:  "turn:turn.example.com:3478?transport=udp",
		},
		{
			name:     "escaped credentials",
			raw:      "turn:user%40example.com:p%3Aass@turn.example.com:3478?transport=udp",
			username: "user@example.com",
			password: "p:ass",
			wantURI:  "turn:turn.example.com:3478?transport=udp",
		},
		{
			name:     "secure TURN",
			raw:      "turns:user:pass@turn.example.com:5349?transport=tcp",
			username: "user",
			password: "pass",
			wantURI:  "turns:turn.example.com:5349?transport=tcp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := buildICEURLs(nil, []string{tt.raw}, nil)
			if !set.hasTURN || len(set.urls) != 1 {
				t.Fatalf("expected one usable TURN URL, got hasTURN=%t urls=%d", set.hasTURN, len(set.urls))
			}
			u := set.urls[0]
			if u.Username != tt.username || u.Password != tt.password {
				t.Fatalf("unexpected TURN credentials: username=%q password=%q", u.Username, u.Password)
			}
			if got := u.String(); got != tt.wantURI {
				t.Fatalf("unexpected normalized TURN URL: %s", got)
			}
		})
	}
}

func TestBuildICEURLs_RedactsSkippedTURNCredentials(t *testing.T) {
	var logLine string
	reporter := ReporterFunc(func(format string, args ...interface{}) {
		logLine = fmt.Sprintf(format, args...)
	})

	set := buildICEURLs(nil, []string{"turn:private-user@turn.example.com:3478"}, reporter)
	if set.hasTURN || len(set.urls) != 0 {
		t.Fatalf("expected invalid TURN URL to be skipped")
	}
	if strings.Contains(logLine, "private-user") {
		t.Fatalf("TURN username leaked in log: %q", logLine)
	}
	if !strings.Contains(logLine, "turn:***@turn.example.com:3478") {
		t.Fatalf("expected redacted TURN endpoint in log, got %q", logLine)
	}
}

func TestRedactICEEndpoint(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "TURN URL", in: "turn:user:pass@turn.example.com:3478?transport=udp", want: "turn:***@turn.example.com:3478?transport=udp"},
		{name: "unprefixed credentials", in: "private-user:private-pass@turn.example.com:3478", want: "***@turn.example.com:3478"},
		{name: "credential query", in: "turn:user:pass@turn.example.com:3478?credential=query-secret", want: "(invalid endpoint redacted)"},
		{name: "malformed credential-like endpoint", in: "turn:user:password", want: "(invalid endpoint redacted)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactICEServerEndpoint(tt.in); got != tt.want {
				t.Fatalf("redaction mismatch: got %q want %q", got, tt.want)
			}
		})
	}
}
