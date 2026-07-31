package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

// Test the healthz endpoint.
func TestMailboxHTTPServer_Healthz(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	srv, err := NewMailboxHTTPServer(mini.Addr(), time.Minute)
	if err != nil {
		t.Fatalf("NewMailboxHTTPServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
}

func TestMailboxHTTPServer_DrainRejectsNewClaims(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	ctx := context.Background()
	collector, err := NewMetricsCollector("redis://"+mini.Addr(), "wormzy")
	if err != nil {
		t.Fatalf("NewMetricsCollector: %v", err)
	}
	defer collector.Close()
	if err := collector.SetDraining(ctx, true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}

	srv, err := NewMailboxHTTPServer(mini.Addr(), time.Minute)
	if err != nil {
		t.Fatalf("NewMailboxHTTPServer: %v", err)
	}
	if err := srv.SyncOperations(ctx); err != nil {
		t.Fatalf("SyncOperations: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		ts.URL+"/v1/claim",
		strings.NewReader(`{"role":"send","requested":""}`),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/claim: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("claim status = %d; want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer redisClient.Close()
	store := newSessionStore(redisClient, time.Minute, "wormzy")
	if _, err := store.registerSender(ctx, "existing-01"); err != nil {
		t.Fatalf("register existing sender: %v", err)
	}
	req, err = http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		ts.URL+"/v1/claim",
		strings.NewReader(`{"role":"recv","requested":"existing-01"}`),
	)
	if err != nil {
		t.Fatalf("new receiver request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatalf("receiver POST /v1/claim: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("receiver claim during drain = %d; want %d", resp.StatusCode, http.StatusOK)
	}
	if err := srv.telemetry.Publish(ctx); err != nil {
		t.Fatalf("publish mailbox telemetry: %v", err)
	}
	metrics, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("collect mailbox telemetry: %v", err)
	}
	mailbox := findServiceSnapshot(t, metrics.Services, "mailbox")
	if mailbox.Requests != 2 || mailbox.RequestErrors != 1 || mailbox.ActiveRequests != 0 {
		t.Fatalf("unexpected mailbox request telemetry: %+v", mailbox)
	}
}

func TestMailboxHTTPServer_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	srv, err := NewMailboxHTTPServer(mini.Addr(), time.Minute)
	if err != nil {
		t.Fatalf("NewMailboxHTTPServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	sender := newHTTPMailbox(ts.URL, "send", 2*time.Second)
	receiver := newHTTPMailbox(ts.URL, "recv", 2*time.Second)

	code, err := sender.Claim(ctx, "")
	if err != nil {
		t.Fatalf("sender claim: %v", err)
	}
	if _, err := receiver.Claim(ctx, code); err != nil {
		t.Fatalf("receiver claim: %v", err)
	}

	sInfo := rendezvous.SelfInfo{Local: "sender-local"}
	rInfo := rendezvous.SelfInfo{Local: "receiver-local"}
	if err := sender.StoreSelf(ctx, sInfo); err != nil {
		t.Fatalf("sender store self: %v", err)
	}
	if err := receiver.StoreSelf(ctx, rInfo); err != nil {
		t.Fatalf("receiver store self: %v", err)
	}

	gotRecv, err := sender.WaitPeer(ctx)
	if err != nil {
		t.Fatalf("sender wait peer: %v", err)
	}
	if gotRecv.Local != rInfo.Local {
		t.Fatalf("sender saw peer %v, want %v", gotRecv.Local, rInfo.Local)
	}

	gotSend, err := receiver.WaitPeer(ctx)
	if err != nil {
		t.Fatalf("receiver wait peer: %v", err)
	}
	if gotSend.Local != sInfo.Local {
		t.Fatalf("receiver saw peer %v, want %v", gotSend.Local, sInfo.Local)
	}

	// mailbox message round-trip
	if err := sender.Send(ctx, "hello", map[string]string{"msg": "hi"}); err != nil {
		t.Fatalf("sender send: %v", err)
	}
	msg, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatalf("receiver receive: %v", err)
	}
	if msg.Type != "hello" {
		t.Fatalf("unexpected message type %s", msg.Type)
	}

	// stats round-trip persisted in Redis
	stats := transferStats{
		Mode:           "recv",
		Transport:      "p2p",
		Candidate:      "direct",
		DirectOutcome:  "won",
		DirectSummary:  "reflexive@203.0.113.10:4242=won",
		Bytes:          65536,
		DurationMillis: 1234,
		Completed:      true,
	}
	if err := receiver.ReportStats(ctx, stats); err != nil {
		t.Fatalf("receiver report stats: %v", err)
	}

	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	val, err := redisClient.Get(ctx, "wormzy:sessions:"+code).Result()
	if err != nil {
		t.Fatalf("redis get session: %v", err)
	}
	var sess rendezvousSession
	if err := json.Unmarshal([]byte(val), &sess); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	if sess.Stats == nil || !sess.Stats.Completed || sess.Stats.Transport != "p2p" {
		t.Fatalf("stats not stored as expected: %+v", sess.Stats)
	}
	if sess.Stats.DirectOutcome != "won" {
		t.Fatalf("direct outcome not stored: %+v", sess.Stats)
	}
	if sess.Stats.Bytes != 65536 || sess.Stats.DurationMillis != 1234 {
		t.Fatalf("stats size/duration not stored: %+v", sess.Stats)
	}
}
