package transport

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

type routingTestMailbox struct {
	messages []mailboxMessage
}

// Claim is unused by mailbox routing tests.
func (m *routingTestMailbox) Claim(context.Context, string) (string, error) { return "", nil }

// StoreSelf is unused by mailbox routing tests.
func (m *routingTestMailbox) StoreSelf(context.Context, rendezvous.SelfInfo) error { return nil }

// WaitPeer is unused by mailbox routing tests.
func (m *routingTestMailbox) WaitPeer(context.Context) (*rendezvous.SelfInfo, error) {
	return nil, nil
}

// Send is unused by mailbox routing tests.
func (m *routingTestMailbox) Send(context.Context, string, any) error { return nil }

// Receive returns queued messages in FIFO order.
func (m *routingTestMailbox) Receive(context.Context) (mailboxMessage, error) {
	if len(m.messages) == 0 {
		return mailboxMessage{}, errors.New("no test messages")
	}
	message := m.messages[0]
	m.messages = m.messages[1:]
	return message, nil
}

// ReportStats is unused by mailbox routing tests.
func (m *routingTestMailbox) ReportStats(context.Context, transferStats) error { return nil }

// Close is unused by mailbox routing tests.
func (m *routingTestMailbox) Close() error { return nil }

// TestProtocolMailbox_DefersFuturePhaseMessages verifies ICE cannot consume readiness messages.
func TestProtocolMailbox_DefersFuturePhaseMessages(t *testing.T) {
	base := &routingTestMailbox{messages: []mailboxMessage{
		{Type: legacyCandidatesReadyMessageType},
		{Type: "ice-auth"},
		{Type: "ice-candidates"},
	}}
	mbox := newProtocolMailbox(base)

	for _, want := range []string{"ice-auth", "ice-candidates"} {
		message, err := receiveMailboxType(context.Background(), mbox, "ice-auth", "ice-candidates")
		if err != nil {
			t.Fatalf("receive ICE message: %v", err)
		}
		if message.Type != want {
			t.Fatalf("ICE message type = %q; want %q", message.Type, want)
		}
	}

	message, err := receiveMailboxType(context.Background(), mbox, legacyCandidatesReadyMessageType)
	if err != nil {
		t.Fatalf("receive deferred readiness: %v", err)
	}
	if message.Type != legacyCandidatesReadyMessageType {
		t.Fatalf("deferred message type = %q; want %q", message.Type, legacyCandidatesReadyMessageType)
	}
}

// TestProtocolMailbox_SkipsStaleICEForReadiness verifies fallback synchronization can pass stale ICE messages.
func TestProtocolMailbox_SkipsStaleICEForReadiness(t *testing.T) {
	base := &routingTestMailbox{messages: []mailboxMessage{
		{Type: "ice-auth"},
		{Type: "ice-candidates"},
		{Type: legacyCandidatesReadyMessageType},
	}}
	mbox := newProtocolMailbox(base)

	message, err := receiveMailboxType(context.Background(), mbox, legacyCandidatesReadyMessageType)
	if err != nil {
		t.Fatalf("receive readiness after stale ICE: %v", err)
	}
	if message.Type != legacyCandidatesReadyMessageType {
		t.Fatalf("message type = %q; want %q", message.Type, legacyCandidatesReadyMessageType)
	}

	for _, want := range []string{"ice-auth", "ice-candidates"} {
		message, err := mbox.Receive(context.Background())
		if err != nil {
			t.Fatalf("receive deferred stale message: %v", err)
		}
		if message.Type != want {
			t.Fatalf("deferred stale message = %q; want %q", message.Type, want)
		}
	}
}

// TestProgressiveUPnPSyncTimeout_CoversDiscoverySkew verifies a fast peer waits through the other peer's mapping budget.
func TestProgressiveUPnPSyncTimeout_CoversDiscoverySkew(t *testing.T) {
	got := progressiveUPnPSyncTimeout(Config{HandshakeTimeout: defaultHandshakeTO})
	minimum := upnpFallbackDelay + defaultUPnPTimeout
	if got <= minimum {
		t.Fatalf("sync timeout %s must exceed delayed UPnP budget %s", got, minimum)
	}
}

// TestProtocolMailbox_RejectsOversizedAcceptedMessage verifies accepted phases cannot bypass envelope bounds.
func TestProtocolMailbox_RejectsOversizedAcceptedMessage(t *testing.T) {
	base := &routingTestMailbox{messages: []mailboxMessage{{
		Type: "ice-auth",
		Body: []byte(strings.Repeat("x", maxDeferredMailboxBodySize+1)),
	}}}
	mbox := newProtocolMailbox(base)
	if _, err := receiveMailboxType(context.Background(), mbox, "ice-auth"); err == nil {
		t.Fatal("accepted oversized current-phase mailbox message")
	}
}

// TestProtocolMailbox_RejectsUnsafeMessageType verifies terminal controls cannot enter routing or logs.
func TestProtocolMailbox_RejectsUnsafeMessageType(t *testing.T) {
	base := &routingTestMailbox{messages: []mailboxMessage{{Type: "ice-auth\x1b[2J"}}}
	mbox := newProtocolMailbox(base)
	if _, err := mbox.Receive(context.Background()); err == nil {
		t.Fatal("accepted unsafe mailbox message type")
	}
}
