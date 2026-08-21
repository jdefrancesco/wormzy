package transport

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

type lifecycleMailbox struct {
	report func(context.Context, transferStats) error
	store  func(context.Context, rendezvous.SelfInfo) error
}

func (m *lifecycleMailbox) Claim(context.Context, string) (string, error) { return "", nil }

// StoreSelf records a lease refresh when the test installs a callback.
func (m *lifecycleMailbox) StoreSelf(ctx context.Context, info rendezvous.SelfInfo) error {
	if m.store == nil {
		return nil
	}
	return m.store(ctx, info)
}

func (m *lifecycleMailbox) WaitPeer(context.Context) (*rendezvous.SelfInfo, error) {
	return nil, nil
}

func (m *lifecycleMailbox) Send(context.Context, string, any) error { return nil }

func (m *lifecycleMailbox) Receive(context.Context) (mailboxMessage, error) {
	return mailboxMessage{}, nil
}

func (m *lifecycleMailbox) ReportStats(ctx context.Context, stats transferStats) error {
	return m.report(ctx, stats)
}

func (m *lifecycleMailbox) Close() error { return nil }

type lifecycleCloser struct {
	name  string
	order *[]string
}

func (c *lifecycleCloser) Close() error {
	*c.order = append(*c.order, c.name)
	return nil
}

func TestFinalizeTransferReportsCompletedStatsBeforeCleanup(t *testing.T) {
	var order []string
	var got transferStats
	mbox := &lifecycleMailbox{report: func(ctx context.Context, stats transferStats) error {
		order = append(order, "stats")
		got = stats
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("stats context has no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > statsReportTimeout {
			t.Fatalf("unexpected stats deadline remaining: %s", remaining)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("stats context was canceled before reporting: %v", context.Cause(ctx))
		default:
		}
		return nil
	}}

	started := time.Now().Add(-2 * time.Second)
	finalizeTransfer(
		mbox,
		transferStats{Mode: "send", Transport: "p2p", Candidate: "ice-p2p"},
		&Result{FileSize: 4096},
		nil,
		started,
		func() { order = append(order, "cleanup") },
		nil,
	)

	if want := []string{"stats", "cleanup"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("finalization order = %v; want %v", order, want)
	}
	if !got.Completed || got.Bytes != 4096 {
		t.Fatalf("unexpected completed stats: %+v", got)
	}
	if got.DurationMillis < 1900 {
		t.Fatalf("duration was not recorded: %dms", got.DurationMillis)
	}
}

// TestMailboxLeaseRefreshesUntilStopped verifies long transfers renew their
// authenticated mailbox session without leaking a background goroutine.
func TestMailboxLeaseRefreshesUntilStopped(t *testing.T) {
	refreshed := make(chan rendezvous.SelfInfo, 1)
	var refreshCount atomic.Int32
	mbox := &lifecycleMailbox{store: func(_ context.Context, info rendezvous.SelfInfo) error {
		refreshCount.Add(1)
		select {
		case refreshed <- info:
		default:
		}
		return nil
	}}
	self := rendezvous.SelfInfo{Public: "203.0.113.8:4242"}
	stop := startMailboxLease(context.Background(), mbox, self, 5*time.Millisecond, nil)

	select {
	case got := <-refreshed:
		if got.Public != self.Public {
			t.Fatalf("refreshed info = %+v; want %+v", got, self)
		}
	case <-time.After(time.Second):
		t.Fatal("mailbox lease was not refreshed")
	}
	stop()
	stop()
	stoppedCount := refreshCount.Load()
	time.Sleep(25 * time.Millisecond)
	if got := refreshCount.Load(); got != stoppedCount {
		t.Fatalf("refresh count after stop = %d; want %d", got, stoppedCount)
	}
}

func TestNewICECleanupClosesPacketTransportAndAgentOnce(t *testing.T) {
	var order []string
	cleanup := newICEQUICCleanup(
		&lifecycleCloser{name: "packet", order: &order},
		&lifecycleCloser{name: "transport", order: &order},
		&lifecycleCloser{name: "agent", order: &order},
	)

	cleanup()
	cleanup()

	if want := []string{"packet", "transport", "agent"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("cleanup order = %v; want %v", order, want)
	}
}

// TestDrainRaceLosersCleansLateSuccesses verifies direct and relay resources
// that finish after a winner are not left alive in buffered result channels.
func TestDrainRaceLosersCleansLateSuccesses(t *testing.T) {
	direct := make(chan directRaceResult, 1)
	relay := make(chan relayRaceResult, 1)
	var closed []string
	direct <- directRaceResult{discard: func() { closed = append(closed, "direct") }}
	relay <- relayRaceResult{discard: func() { closed = append(closed, "relay") }}
	close(direct)
	close(relay)

	drainRaceLosers(direct, relay)

	if want := []string{"direct", "relay"}; !reflect.DeepEqual(closed, want) {
		t.Fatalf("cleanup order = %v; want %v", closed, want)
	}
}
