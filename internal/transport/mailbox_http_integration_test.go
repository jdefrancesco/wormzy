package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

// TestMailboxHTTPServer_ReceiverMayClaimFirst verifies immediate sender code
// display cannot race a receiver that enters the code before sender admission.
func TestMailboxHTTPServer_ReceiverMayClaimFirst(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	server, err := NewMailboxHTTPServer(mini.Addr(), time.Minute)
	if err != nil {
		t.Fatalf("NewMailboxHTTPServer: %v", err)
	}
	endpoint := httptest.NewServer(server)
	defer endpoint.Close()
	defer server.Close()

	sender := newHTTPMailbox(endpoint.URL, "send", 2*time.Second)
	receiver := newHTTPMailbox(endpoint.URL, "recv", 2*time.Second)
	code := sessionStoreTestID(0x4f)
	type claimResult struct {
		code string
		err  error
	}
	receiverResult := make(chan claimResult, 1)
	go func() {
		claimed, claimErr := receiver.Claim(context.Background(), code)
		receiverResult <- claimResult{code: claimed, err: claimErr}
	}()

	select {
	case result := <-receiverResult:
		t.Fatalf("receiver claim returned before sender: code=%q err=%v", result.code, result.err)
	case <-time.After(50 * time.Millisecond):
	}
	if claimed, err := sender.Claim(context.Background(), code); err != nil || claimed != code {
		t.Fatalf("sender claim = %q, %v; want %q", claimed, err, code)
	}
	select {
	case result := <-receiverResult:
		if result.err != nil || result.code != code {
			t.Fatalf("receiver claim = %q, %v; want %q", result.code, result.err, code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiver claim did not complete after sender admission")
	}
}

// TestMailboxHTTPServer_Healthz verifies a healthy Redis dependency is reported.
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

// TestMailboxHTTPServerRejectsNonpositiveTTL preserves the active-session
// expiry invariant used by the distributed capacity limit.
func TestMailboxHTTPServerRejectsNonpositiveTTL(t *testing.T) {
	if _, err := NewMailboxHTTPServer("127.0.0.1:6379", 0); err == nil {
		t.Fatal("NewMailboxHTTPServer accepted a zero session TTL")
	}
}

// TestMailboxHTTPServer_HealthzHidesStorageDetails verifies dependency errors stay server-side.
func TestMailboxHTTPServer_HealthzHidesStorageDetails(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	srv, err := NewMailboxHTTPServer(mini.Addr(), time.Minute)
	if err != nil {
		mini.Close()
		t.Fatalf("NewMailboxHTTPServer: %v", err)
	}
	defer srv.Close()
	storageAddress := mini.Addr()
	mini.Close()

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	srv.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz status = %d; want %d", response.Code, http.StatusServiceUnavailable)
	}
	result := response.Result()
	defer result.Body.Close()
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), storageAddress) || strings.Contains(string(body), "dial tcp") {
		t.Fatalf("health response exposed storage details: %q", body)
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
		ts.URL+mailboxHTTPClaimPath,
		strings.NewReader(`{"role":"send","requested":"`+sessionStoreTestID(0x50)+`","capability_hash":"`+sessionStoreTestCapability(t, 0x43)+`"}`),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", mailboxHTTPClaimPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("claim status = %d; want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer redisClient.Close()
	store := newSessionStore(redisClient, time.Minute, mailboxV2StorePrefix)
	existingID := sessionStoreTestID(0x51)
	if _, err := store.registerSender(ctx, existingID, sessionStoreTestCapability(t, 0x44)); err != nil {
		t.Fatalf("register existing sender: %v", err)
	}
	req, err = http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		ts.URL+mailboxHTTPClaimPath,
		strings.NewReader(`{"role":"recv","requested":"`+existingID+`","capability_hash":"`+sessionStoreTestCapability(t, 0x45)+`"}`),
	)
	if err != nil {
		t.Fatalf("new receiver request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatalf("receiver POST %s: %v", mailboxHTTPClaimPath, err)
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

	requestedID := sessionStoreTestID(0x52)
	code, err := sender.Claim(ctx, requestedID)
	if err != nil {
		t.Fatalf("sender claim: %v", err)
	}
	if retryCode, err := sender.Claim(ctx, requestedID); err != nil || retryCode != code {
		t.Fatalf("sender claim retry = %q, %v; want %q", retryCode, err, code)
	}
	conflictingSender := newHTTPMailbox(ts.URL, "send", 2*time.Second)
	if _, err := conflictingSender.Claim(ctx, requestedID); !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("conflicting sender claim error = %v; want %v", err, errMailboxUnavailable)
	}
	if _, err := receiver.Claim(ctx, code); err != nil {
		t.Fatalf("receiver claim: %v", err)
	}
	if retryCode, err := receiver.Claim(ctx, code); err != nil || retryCode != code {
		t.Fatalf("receiver claim retry = %q, %v; want %q", retryCode, err, code)
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
		Candidate:      "reflexive",
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
	val, err := redisClient.Get(ctx, mailboxV2StorePrefix+":sessions:"+code).Result()
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
	legacyExists, err := redisClient.Exists(ctx, "wormzy:sessions:"+code).Result()
	if err != nil {
		t.Fatalf("check legacy namespace: %v", err)
	}
	if legacyExists != 0 {
		t.Fatal("v2 session was written into the legacy Redis namespace")
	}
}

// TestMailboxHTTPServer_RequiresRoleCapabilityEveryEndpoint verifies missing and cross-role credentials fail closed.
func TestMailboxHTTPServer_RequiresRoleCapabilityEveryEndpoint(t *testing.T) {
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

	sender := newHTTPMailbox(ts.URL, "send", time.Second).(*httpMailbox)
	receiver := newHTTPMailbox(ts.URL, "recv", time.Second).(*httpMailbox)
	code, err := sender.Claim(ctx, sessionStoreTestID(0x57))
	if err != nil {
		t.Fatalf("sender claim: %v", err)
	}
	if _, err := receiver.Claim(ctx, code); err != nil {
		t.Fatalf("receiver claim: %v", err)
	}

	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "store self", path: mailboxHTTPSelfPath, body: `{"role":"send","code":"` + code + `","info":{}}`},
		{name: "wait peer", path: mailboxHTTPWaitPeerPath, body: `{"role":"send","code":"` + code + `"}`},
		{name: "send", path: mailboxHTTPSendPath, body: `{"role":"send","code":"` + code + `","type":"test","body":"e30="}`},
		{name: "receive", path: mailboxHTTPReceivePath, body: `{"role":"send","code":"` + code + `"}`},
		{name: "stats", path: mailboxHTTPStatsPath, body: `{"role":"send","code":"` + code + `","completed":false}`},
	}
	for _, tt := range tests {
		for _, auth := range []struct {
			name   string
			header []string
		}{
			{name: "missing"},
			{name: "receiver acting as sender", header: []string{"Bearer " + receiver.capability}},
			{name: "multiple headers", header: []string{"Bearer " + sender.capability, "Bearer " + receiver.capability}},
		} {
			t.Run(tt.name+"/"+auth.name, func(t *testing.T) {
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+tt.path, strings.NewReader(tt.body))
				if err != nil {
					t.Fatalf("new request: %v", err)
				}
				request.Header.Set("Content-Type", "application/json")
				for _, header := range auth.header {
					request.Header.Add("Authorization", header)
				}
				response, err := ts.Client().Do(request)
				if err != nil {
					t.Fatalf("request: %v", err)
				}
				_ = response.Body.Close()
				if response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("status = %d; want %d", response.StatusCode, http.StatusUnauthorized)
				}
			})
		}
	}
}

// TestMailboxHTTPServer_ProgressiveUPnPBarrier verifies ordered candidate refresh through the deployed protocol shape.
func TestMailboxHTTPServer_ProgressiveUPnPBarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	code, err := sender.Claim(ctx, sessionStoreTestID(0x53))
	if err != nil {
		t.Fatalf("sender claim: %v", err)
	}
	if _, err := receiver.Claim(ctx, code); err != nil {
		t.Fatalf("receiver claim: %v", err)
	}

	senderInfo := rendezvous.SelfInfo{
		Public:     "8.8.8.10:4000",
		Features:   []string{featureICEv1, featureProgressiveUPnPV1},
		Candidates: []rendezvous.Candidate{{Type: "reflexive", Proto: "udp", Addr: "8.8.8.10:4000", Priority: 100}},
	}
	receiverInfo := rendezvous.SelfInfo{
		Public:     "1.1.1.20:4000",
		Features:   []string{featureICEv1, featureProgressiveUPnPV1},
		Candidates: []rendezvous.Candidate{{Type: "reflexive", Proto: "udp", Addr: "1.1.1.20:4000", Priority: 100}},
	}
	if err := sender.StoreSelf(ctx, senderInfo); err != nil {
		t.Fatalf("sender initial StoreSelf: %v", err)
	}
	if err := receiver.StoreSelf(ctx, receiverInfo); err != nil {
		t.Fatalf("receiver initial StoreSelf: %v", err)
	}

	psk := []byte("shared progressive UPnP test key")
	type syncResult struct {
		peer rendezvous.SelfInfo
		err  error
	}
	results := make(chan syncResult, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		peer, err := synchronizeUPnPFallback(
			ctx,
			sender,
			senderInfo,
			receiverInfo,
			upnpFallbackResult{mapping: &upnpMapping{externalAddr: "8.8.8.10:5000", externalPort: 5000}, status: upnpFallbackMapped},
			code,
			"send",
			psk,
			nil,
		)
		results <- syncResult{peer: peer, err: err}
	}()
	go func() {
		defer wg.Done()
		peer, err := synchronizeUPnPFallback(
			ctx,
			receiver,
			receiverInfo,
			senderInfo,
			upnpFallbackResult{mapping: &upnpMapping{externalAddr: "1.1.1.20:5000", externalPort: 5000}, status: upnpFallbackMapped},
			code,
			"recv",
			psk,
			nil,
		)
		results <- syncResult{peer: peer, err: err}
	}()
	wg.Wait()
	close(results)

	seenSenderMapping := false
	seenReceiverMapping := false
	for result := range results {
		if result.err != nil {
			t.Fatalf("progressive synchronization: %v", result.err)
		}
		seenSenderMapping = seenSenderMapping || hasCandidate(result.peer.Candidates, "upnp", "8.8.8.10:5000")
		seenReceiverMapping = seenReceiverMapping || hasCandidate(result.peer.Candidates, "upnp", "1.1.1.20:5000")
	}
	if !seenSenderMapping || !seenReceiverMapping {
		t.Fatalf("refreshed mappings sender=%t receiver=%t", seenSenderMapping, seenReceiverMapping)
	}
}
