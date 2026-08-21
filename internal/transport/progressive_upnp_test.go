package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

type progressiveTestMailbox struct {
	claimCode string
	requested string
	events    []string
	claimHook func()
	stored    rendezvous.SelfInfo
	peer      rendezvous.SelfInfo
	receive   mailboxMessage
}

// Claim records pairing-code allocation for progressive transport tests.
func (m *progressiveTestMailbox) Claim(_ context.Context, requested string) (string, error) {
	if m.claimHook != nil {
		m.claimHook()
	}
	m.events = append(m.events, "claim")
	m.requested = requested
	if m.claimCode == "" {
		return requested, nil
	}
	return m.claimCode, nil
}

// StoreSelf records the latest local candidate snapshot.
func (m *progressiveTestMailbox) StoreSelf(_ context.Context, info rendezvous.SelfInfo) error {
	m.events = append(m.events, "store")
	m.stored = info
	return nil
}

// WaitPeer returns the configured refreshed peer snapshot.
func (m *progressiveTestMailbox) WaitPeer(context.Context) (*rendezvous.SelfInfo, error) {
	m.events = append(m.events, "wait")
	peer := m.peer
	return &peer, nil
}

// Send records a readiness message after encoding its body.
func (m *progressiveTestMailbox) Send(_ context.Context, typ string, body any) error {
	m.events = append(m.events, "send:"+typ)
	_, err := json.Marshal(body)
	return err
}

// Receive returns the configured peer readiness message.
func (m *progressiveTestMailbox) Receive(context.Context) (mailboxMessage, error) {
	m.events = append(m.events, "receive")
	return m.receive, nil
}

// ReportStats is a no-op for progressive transport tests.
func (m *progressiveTestMailbox) ReportStats(context.Context, transferStats) error { return nil }

// Close is a no-op for progressive transport tests.
func (m *progressiveTestMailbox) Close() error { return nil }

type orderedReporter struct {
	mu     sync.Mutex
	events []string
}

// record appends one synthetic ordering event under the reporter lock.
func (r *orderedReporter) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

// Logf is a no-op for ordering tests.
func (r *orderedReporter) Logf(string, ...interface{}) {}

// Stage records transport stage details for ordering tests.
func (r *orderedReporter) Stage(_ Stage, _ StageState, detail string) {
	r.record(detail)
}

// TestClaimPairingCode_ReportsAssignedCode verifies code publication at claim time.
func TestClaimPairingCode_ReportsAssignedCode(t *testing.T) {
	reporter := &orderedReporter{}
	mbox := &progressiveTestMailbox{claimHook: func() { reporter.record("mailbox claim") }}

	code, err := claimPairingCode(context.Background(), Config{Mode: "send", Code: testPairingCode}, reporter, mbox)
	if err != nil {
		t.Fatalf("claimPairingCode: %v", err)
	}
	if code != testPairingCode {
		t.Fatalf("code = %q; want %q", code, testPairingCode)
	}
	wantSessionID, err := deriveMailboxSessionID(testPairingCode)
	if err != nil {
		t.Fatalf("derive expected session ID: %v", err)
	}
	if mbox.requested != wantSessionID {
		t.Fatalf("mailbox received %q; want opaque ID %q", mbox.requested, wantSessionID)
	}
	if !reflect.DeepEqual(reporter.events, []string{"code " + testPairingCode, "mailbox claim"}) {
		t.Fatalf("stage details = %v", reporter.events)
	}
}

// TestAttemptUPnPAfter_WaitsForTrigger verifies UPnP does not run during the initial punch window.
func TestAttemptUPnPAfter_WaitsForTrigger(t *testing.T) {
	trigger := make(chan time.Time)
	called := make(chan struct{}, 1)
	result := make(chan upnpFallbackResult, 1)

	go func() {
		result <- attemptUPnPAfter(context.Background(), trigger, func(context.Context) (*upnpMapping, error) {
			called <- struct{}{}
			return &upnpMapping{externalAddr: "8.8.8.10:5000", externalPort: 5000}, nil
		})
	}()

	select {
	case <-called:
		t.Fatal("UPnP attempt started before fallback trigger")
	default:
	}
	select {
	case trigger <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("timed out triggering UPnP attempt")
	}

	var outcome upnpFallbackResult
	select {
	case outcome = <-result:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for UPnP outcome")
	}
	if outcome.err != nil || outcome.mapping == nil {
		t.Fatalf("outcome = %+v; want successful mapping", outcome)
	}
}

// TestAttemptUPnPAfter_CancelPreventsMapping verifies a direct win cancels delayed UPnP.
func TestAttemptUPnPAfter_CancelPreventsMapping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false

	outcome := attemptUPnPAfter(ctx, make(chan time.Time), func(context.Context) (*upnpMapping, error) {
		called = true
		return nil, nil
	})

	if called {
		t.Fatal("UPnP mapper ran after cancellation")
	}
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("outcome error = %v; want context canceled", outcome.err)
	}
}

// TestStopAndCleanupUPnPFallback_CancelsInflightMapper verifies a direct ICE win joins active discovery.
func TestStopAndCleanupUPnPFallback_CancelsInflightMapper(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan upnpFallbackResult, 1)
	trigger := make(chan time.Time, 1)
	started := make(chan struct{})
	attempt := &delayedUPnPAttempt{cancel: cancel, result: results}
	go func() {
		results <- attemptUPnPAfter(ctx, trigger, func(mapCtx context.Context) (*upnpMapping, error) {
			close(started)
			<-mapCtx.Done()
			return nil, mapCtx.Err()
		})
	}()
	trigger <- time.Now()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("mapper did not start")
	}

	outcome := stopAndCleanupUPnPFallback(attempt, nil, "test direct ICE win")
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("outcome error = %v; want context canceled", outcome.err)
	}
}

// TestStopAndCleanupUPnPFallback_CleansCompletedMappingOnce verifies direct success removes a ready mapping.
func TestStopAndCleanupUPnPFallback_CleansCompletedMappingOnce(t *testing.T) {
	const description = "wormzy-progressive-cleanup"
	client := &fakeUPnPClient{
		specific: map[uint16]fakeUPnPMappingEntry{
			42424: {
				internalPort: 42424,
				internalIP:   "192.168.1.20",
				enabled:      true,
				description:  description,
			},
		},
	}
	mapping := &upnpMapping{
		client:       client,
		externalAddr: "8.8.8.8:42424",
		externalPort: 42424,
		internalIP:   net.IPv4(192, 168, 1, 20),
		internalPort: 42424,
		description:  description,
	}
	results := make(chan upnpFallbackResult, 1)
	results <- upnpFallbackResult{mapping: mapping, status: upnpFallbackMapped}
	_, cancel := context.WithCancel(context.Background())
	attempt := &delayedUPnPAttempt{cancel: cancel, result: results}

	stopAndCleanupUPnPFallback(attempt, nil, "test direct ICE win")
	cleanupUPnPMapping(mapping, nil)

	if client.deleteCalls != 1 {
		t.Fatalf("mapping deletion calls = %d; want 1", client.deleteCalls)
	}
}

// TestSynchronizeUPnPFallback_OrdersCandidateBeforeReady verifies the bilateral refresh barrier.
func TestSynchronizeUPnPFallback_OrdersCandidateBeforeReady(t *testing.T) {
	code := "quick-code"
	psk := []byte("test progressive upnp psk")
	mbox := &progressiveTestMailbox{
		peer: rendezvous.SelfInfo{
			Public:   "1.1.1.20:4000",
			Features: []string{featureProgressiveUPnPV1},
			Candidates: []rendezvous.Candidate{{
				Type: "upnp", Proto: "udp", Addr: "1.1.1.20:5000", Priority: 999,
			}},
		},
	}
	ready, err := newLegacyCandidatesReadyMessage(code, "recv", upnpFallbackMapped, mbox.peer.Candidates, psk)
	if err != nil {
		t.Fatalf("create ready message: %v", err)
	}
	readyBody, err := json.Marshal(ready)
	if err != nil {
		t.Fatalf("marshal ready message: %v", err)
	}
	mbox.receive = mailboxMessage{Type: legacyCandidatesReadyMessageType, Body: readyBody}
	self := rendezvous.SelfInfo{
		Public:     "8.8.8.10:4000",
		Features:   []string{featureICEv1, featureProgressiveUPnPV1},
		Candidates: []rendezvous.Candidate{{Type: "reflexive", Proto: "udp", Addr: "8.8.8.10:4000", Priority: 100}},
	}
	initialPeer := rendezvous.SelfInfo{
		Public:   "1.1.1.20:4000",
		Features: []string{featureICEv1, featureProgressiveUPnPV1},
	}
	outcome := upnpFallbackResult{
		mapping: &upnpMapping{externalAddr: "8.8.8.10:5000", externalPort: 5000},
		status:  upnpFallbackMapped,
	}

	peer, err := synchronizeUPnPFallback(context.Background(), mbox, self, initialPeer, outcome, code, "send", psk, nil)
	if err != nil {
		t.Fatalf("synchronizeUPnPFallback: %v", err)
	}
	wantEvents := []string{"store", "send:" + legacyCandidatesReadyMessageType, "receive", "wait"}
	if !reflect.DeepEqual(mbox.events, wantEvents) {
		t.Fatalf("events = %v; want %v", mbox.events, wantEvents)
	}
	if !hasCandidate(mbox.stored.Candidates, "upnp", "8.8.8.10:5000") {
		t.Fatalf("stored candidates = %#v; want local UPnP candidate", mbox.stored.Candidates)
	}
	if !hasCandidate(peer.Candidates, "upnp", "1.1.1.20:5000") {
		t.Fatalf("peer candidates = %#v; want refreshed UPnP candidate", peer.Candidates)
	}
	for _, candidate := range peer.Candidates {
		if strings.EqualFold(candidate.Type, "upnp") && candidate.Priority != 110 {
			t.Fatalf("refreshed UPnP priority = %d; want normalized 110", candidate.Priority)
		}
	}
}

// TestMergeUPnPFallbackPeer_RejectsMismatchedAddress prevents refreshed metadata from widening dial targets.
func TestMergeUPnPFallbackPeer_RejectsMismatchedAddress(t *testing.T) {
	initial := rendezvous.SelfInfo{Public: "8.8.4.20:4000"}
	refreshed := initial
	refreshed.Candidates = []rendezvous.Candidate{{
		Type: "upnp", Proto: "udp", Addr: "1.0.0.99:5000", Priority: 110,
	}}

	got := mergeUPnPFallbackPeer(initial, refreshed, upnpFallbackMapped)
	if len(got.Candidates) != 0 {
		t.Fatalf("accepted mismatched UPnP candidate: %#v", got.Candidates)
	}
}

// TestSynchronizeUPnPFallback_RejectsTamperedCandidateSnapshot verifies refreshed candidates are PAKE-authenticated.
func TestSynchronizeUPnPFallback_RejectsTamperedCandidateSnapshot(t *testing.T) {
	code := "quick-code"
	psk := []byte("test progressive upnp psk")
	signedCandidates := []rendezvous.Candidate{{
		Type: "upnp", Proto: "udp", Addr: "1.1.1.20:5000", Priority: 110,
	}}
	ready, err := newLegacyCandidatesReadyMessage(code, "recv", upnpFallbackMapped, signedCandidates, psk)
	if err != nil {
		t.Fatalf("create ready message: %v", err)
	}
	body, err := json.Marshal(ready)
	if err != nil {
		t.Fatalf("marshal ready message: %v", err)
	}
	mbox := &progressiveTestMailbox{
		peer: rendezvous.SelfInfo{
			Public:   "1.1.1.20:4000",
			Features: []string{featureICEv1, featureProgressiveUPnPV1},
			Candidates: []rendezvous.Candidate{{
				Type: "upnp", Proto: "udp", Addr: "1.1.1.20:5999", Priority: 110,
			}},
		},
		receive: mailboxMessage{Type: legacyCandidatesReadyMessageType, Body: body},
	}
	self := rendezvous.SelfInfo{
		Public:     "8.8.8.10:4000",
		Features:   []string{featureICEv1, featureProgressiveUPnPV1},
		Candidates: []rendezvous.Candidate{{Type: "reflexive", Proto: "udp", Addr: "8.8.8.10:4000", Priority: 100}},
	}
	initialPeer := rendezvous.SelfInfo{
		Public:   "1.1.1.20:4000",
		Features: []string{featureICEv1, featureProgressiveUPnPV1},
	}
	outcome := upnpFallbackResult{status: upnpFallbackFailed}

	if _, err := synchronizeUPnPFallback(context.Background(), mbox, self, initialPeer, outcome, code, "send", psk, nil); err == nil {
		t.Fatal("accepted a peer candidate snapshot that did not match its authenticated digest")
	}
}

// TestLegacyCandidatesReadyMessage_AuthenticatesStatuses verifies every protocol status and rejects tampering.
func TestLegacyCandidatesReadyMessage_AuthenticatesStatuses(t *testing.T) {
	code := "quick-code"
	psk := []byte("test progressive upnp psk")
	candidates := []rendezvous.Candidate{{
		Type: "reflexive", Proto: "udp", Addr: "8.8.8.10:4000", Priority: 100,
	}}

	for _, status := range []upnpFallbackStatus{upnpFallbackMapped, upnpFallbackFailed, upnpFallbackDisabled} {
		t.Run(string(status), func(t *testing.T) {
			message, err := newLegacyCandidatesReadyMessage(code, "send", status, candidates, psk)
			if err != nil {
				t.Fatalf("newLegacyCandidatesReadyMessage: %v", err)
			}
			if err := verifyLegacyCandidatesReadyMessage(message, code, "send", psk); err != nil {
				t.Fatalf("verifyLegacyCandidatesReadyMessage: %v", err)
			}

			tampered := message
			tampered.UPnP = upnpFallbackStatus("forged")
			if err := verifyLegacyCandidatesReadyMessage(tampered, code, "send", psk); err == nil {
				t.Fatal("accepted a tampered readiness status")
			}

			tampered = message
			tampered.MAC = strings.Repeat("0", 64)
			if err := verifyLegacyCandidatesReadyMessage(tampered, code, "send", psk); err == nil {
				t.Fatal("accepted a tampered readiness MAC")
			}
		})
	}
}

// TestDecodeLegacyCandidatesReadyMessage_RejectsMalformedPayloads verifies strict bounded JSON decoding.
func TestDecodeLegacyCandidatesReadyMessage_RejectsMalformedPayloads(t *testing.T) {
	tests := map[string]json.RawMessage{
		"unknown field": []byte(`{"role":"send","upnp":"failed","digest":"x","mac":"y","extra":true}`),
		"trailing JSON": []byte(`{"role":"send","upnp":"failed","digest":"x","mac":"y"}{}`),
		"oversized":     []byte(strings.Repeat("x", maxLegacyReadyMessageSize+1)),
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeLegacyCandidatesReadyMessage(raw); err == nil {
				t.Fatal("accepted malformed progressive UPnP readiness payload")
			}
		})
	}
}

// TestCandidateSetDigest_RejectsExcessiveCandidates verifies the authenticated snapshot has a hard count limit.
func TestCandidateSetDigest_RejectsExcessiveCandidates(t *testing.T) {
	candidates := make([]rendezvous.Candidate, maxProgressiveCandidateCount+1)
	if _, err := candidateSetDigest(candidates); err == nil {
		t.Fatalf("accepted %d candidates; limit is %d", len(candidates), maxProgressiveCandidateCount)
	}
}

// TestMergeUPnPFallbackPeer_RequiresMappedPublicUDP verifies refreshed candidates cannot widen the dial surface.
func TestMergeUPnPFallbackPeer_RequiresMappedPublicUDP(t *testing.T) {
	initial := rendezvous.SelfInfo{Public: "8.8.4.20:4000"}
	tests := []struct {
		name      string
		status    upnpFallbackStatus
		candidate rendezvous.Candidate
	}{
		{
			name:      "failed status",
			status:    upnpFallbackFailed,
			candidate: rendezvous.Candidate{Type: "upnp", Proto: "udp", Addr: "8.8.4.20:5000"},
		},
		{
			name:      "private address",
			status:    upnpFallbackMapped,
			candidate: rendezvous.Candidate{Type: "upnp", Proto: "udp", Addr: "192.168.1.20:5000"},
		},
		{
			name:      "non UDP protocol",
			status:    upnpFallbackMapped,
			candidate: rendezvous.Candidate{Type: "upnp", Proto: "tcp", Addr: "8.8.4.20:5000"},
		},
		{
			name:      "wrong candidate type",
			status:    upnpFallbackMapped,
			candidate: rendezvous.Candidate{Type: "reflexive", Proto: "udp", Addr: "8.8.4.20:5000"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			refreshed := initial
			refreshed.Candidates = []rendezvous.Candidate{test.candidate}
			got := mergeUPnPFallbackPeer(initial, refreshed, test.status)
			if len(got.Candidates) != 0 {
				t.Fatalf("accepted candidate %#v", got.Candidates)
			}
		})
	}
}

// TestConfigValidate_SendDirectory verifies direct transport callers cannot claim a session for a directory.
func TestConfigValidate_SendDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := (Config{Mode: "send", FilePath: dir}).validate(); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("validate directory error = %v; want directory rejection", err)
	}

	file := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := (Config{Mode: "send", FilePath: file}).validate(); err != nil {
		t.Fatalf("validate regular file: %v", err)
	}

	if info, err := os.Stat(os.DevNull); err == nil && !info.Mode().IsRegular() {
		if err := (Config{Mode: "send", FilePath: os.DevNull}).validate(); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("validate special-file error = %v; want regular-file rejection", err)
		}
	}
}

// hasCandidate reports whether candidates contain the requested type and address.
func hasCandidate(candidates []rendezvous.Candidate, typ, addr string) bool {
	for _, candidate := range candidates {
		if candidate.Type == typ && candidate.Addr == addr {
			return true
		}
	}
	return false
}
