package transport

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
	"github.com/quic-go/quic-go"
)

type lifecycleMailbox struct {
	report func(context.Context, transferStats) error
}

func (m *lifecycleMailbox) Claim(context.Context, string) (string, error) { return "", nil }

func (m *lifecycleMailbox) StoreSelf(context.Context, rendezvous.SelfInfo) error { return nil }

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

type lifecycleQUICConn struct {
	ctx        context.Context
	closeCalls int
}

func (c *lifecycleQUICConn) Context() context.Context { return c.ctx }

func (c *lifecycleQUICConn) CloseWithError(quic.ApplicationErrorCode, string) error {
	c.closeCalls++
	return nil
}

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

func TestFinishQUICConnection(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		peerClosed bool
		finishDone bool
		wantCloses int
	}{
		{name: "receiver signals verified completion", mode: "recv", wantCloses: 1},
		{name: "sender observes receiver close", mode: "send", peerClosed: true, wantCloses: 0},
		{name: "sender closes after finish deadline", mode: "send", finishDone: true, wantCloses: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peerCtx := context.Background()
			if tt.peerClosed {
				var cancel context.CancelFunc
				peerCtx, cancel = context.WithCancel(peerCtx)
				cancel()
			}
			finishCtx := context.Background()
			if tt.finishDone {
				var cancel context.CancelFunc
				finishCtx, cancel = context.WithCancel(finishCtx)
				cancel()
			}
			conn := &lifecycleQUICConn{ctx: peerCtx}

			finishQUICConnection(finishCtx, conn, tt.mode)

			if conn.closeCalls != tt.wantCloses {
				t.Fatalf("CloseWithError calls = %d; want %d", conn.closeCalls, tt.wantCloses)
			}
		})
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
