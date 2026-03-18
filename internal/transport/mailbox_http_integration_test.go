package transport

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/internal/rendezvous"
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
	stats := transferStats{Mode: "recv", Transport: "p2p", Candidate: "direct", Completed: true}
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
}
