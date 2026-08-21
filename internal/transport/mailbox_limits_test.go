package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

// mailboxLimitRoundTripFunc adapts a function into an HTTP test transport.
type mailboxLimitRoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip executes the mailbox limit test transport function.
func (fn mailboxLimitRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

// TestRedisFixedWindowLimiterCountsAndExpires verifies counters are shared in Redis without exposing identifiers.
func TestRedisFixedWindowLimiterCountsAndExpires(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	limiter := newRedisFixedWindowLimiter(client, mailboxV2StorePrefix)
	now := time.Unix(120, 0)
	limiter.now = func() time.Time { return now }

	for attempt := 1; attempt <= 3; attempt++ {
		decision, err := limiter.allow(context.Background(), "sender-ip", "203.0.113.14", 2, time.Minute)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		wantAllowed := attempt <= 2
		if decision.Allowed != wantAllowed {
			t.Fatalf("attempt %d allowed = %t; want %t", attempt, decision.Allowed, wantAllowed)
		}
		if !decision.Allowed && (decision.RetryAfter <= 0 || decision.RetryAfter > time.Minute) {
			t.Fatalf("retry after = %s; want within one window", decision.RetryAfter)
		}
	}

	keys := mini.Keys()
	if len(keys) != 1 {
		t.Fatalf("rate-limit keys = %v; want one", keys)
	}
	if strings.Contains(keys[0], "203.0.113.14") {
		t.Fatalf("rate-limit key exposes client identity: %q", keys[0])
	}
	ttl := mini.TTL(keys[0])
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("rate-limit TTL = %s; want a bounded positive TTL", ttl)
	}

	mini.FastForward(time.Minute + time.Second)
	now = now.Add(time.Minute + time.Second)
	if mini.Exists(keys[0]) {
		t.Fatalf("expired rate-limit key %q still exists", keys[0])
	}
	decision, err := limiter.allow(context.Background(), "sender-ip", "203.0.113.14", 2, time.Minute)
	if err != nil || !decision.Allowed {
		t.Fatalf("new window decision = %+v, %v; want allowed", decision, err)
	}
}

// TestMailboxClientIPTrustsOnlyLoopbackProxy verifies clients cannot spoof forwarding headers directly.
func TestMailboxClientIPTrustsOnlyLoopbackProxy(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{name: "direct ignores spoof", remoteAddr: "198.51.100.8:4567", forwarded: "203.0.113.9", want: "198.51.100.8"},
		{name: "loopback trusts rightmost", remoteAddr: "127.0.0.1:4567", forwarded: "198.51.100.4, 203.0.113.9", want: "203.0.113.9"},
		{name: "loopback malformed falls back", remoteAddr: "[::1]:4567", forwarded: "not-an-ip", want: "::1"},
		{name: "IPv4 mapped canonicalizes", remoteAddr: "[::ffff:192.0.2.5]:4567", want: "192.0.2.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, mailboxHTTPClaimPath, nil)
			request.RemoteAddr = tt.remoteAddr
			request.Header.Set("X-Forwarded-For", tt.forwarded)
			if got := mailboxClientIP(request); got != tt.want {
				t.Fatalf("mailboxClientIP() = %q; want %q", got, tt.want)
			}
		})
	}

	request := httptest.NewRequest(http.MethodPost, mailboxHTTPClaimPath, nil)
	request.RemoteAddr = "127.0.0.1:4567"
	request.Header.Add("X-Forwarded-For", "198.51.100.4")
	request.Header.Add("X-Forwarded-For", "203.0.113.9")
	if got := mailboxClientIP(request); got != "203.0.113.9" {
		t.Fatalf("multiple X-Forwarded-For headers resolved to %q; want rightmost proxy value", got)
	}
}

// TestMailboxLongPollGateEnforcesDuplicateAndGlobalLimits verifies poll slots are bounded and released safely.
func TestMailboxLongPollGateEnforcesDuplicateAndGlobalLimits(t *testing.T) {
	gate := newMailboxLongPollGate(2)
	releaseA, ok := gate.acquire("capability-a")
	if !ok {
		t.Fatal("first capability was rejected")
	}
	if _, ok := gate.acquire("capability-a"); ok {
		t.Fatal("duplicate capability acquired a second slot")
	}
	releaseB, ok := gate.acquire("capability-b")
	if !ok {
		t.Fatal("second capability was rejected")
	}
	if _, ok := gate.acquire("capability-c"); ok {
		t.Fatal("global ceiling admitted a third poll")
	}

	releaseA()
	releaseA()
	releaseC, ok := gate.acquire("capability-c")
	if !ok {
		t.Fatal("released slot was not reusable")
	}
	releaseB()
	releaseC()
}

// TestHTTPMailboxRetriesPollAgain verifies a bounded server poll is transparent to callers.
func TestHTTPMailboxRetriesPollAgain(t *testing.T) {
	mailbox := newHTTPMailbox("http://127.0.0.1", "send", time.Second).(*httpMailbox)
	mailbox.code = mailboxLimitTestSessionID(0x71)
	var requests atomic.Int32
	mailbox.client = &http.Client{Transport: mailboxLimitRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != mailboxHTTPWaitPeerPath {
			t.Fatalf("path = %q; want %q", request.URL.Path, mailboxHTTPWaitPeerPath)
		}
		if requests.Add(1) == 1 {
			return mailboxLimitTestHTTPResponse(http.StatusNoContent, ""), nil
		}
		return mailboxLimitTestHTTPResponse(http.StatusOK, `{"info":{"local":"127.0.0.1:4444"}}`), nil
	})}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := mailbox.WaitPeer(ctx)
	if err != nil {
		t.Fatalf("WaitPeer: %v", err)
	}
	if info.Local != "127.0.0.1:4444" {
		t.Fatalf("peer local = %q; want %q", info.Local, "127.0.0.1:4444")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d; want 2", got)
	}
}

// TestHTTPMailboxPollStopsAtCallerDeadline verifies repeated bounded polls do
// not reset the transfer's total rendezvous timeout.
func TestHTTPMailboxPollStopsAtCallerDeadline(t *testing.T) {
	mailbox := newHTTPMailbox("http://127.0.0.1", "send", time.Second).(*httpMailbox)
	mailbox.code = mailboxLimitTestSessionID(0x72)
	var requests atomic.Int32
	mailbox.client = &http.Client{Transport: mailboxLimitRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return mailboxLimitTestHTTPResponse(http.StatusNoContent, ""), nil
	})}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := mailbox.WaitPeer(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitPeer error = %v; want %v", err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("WaitPeer stopped after %s; want bounded caller deadline", elapsed)
	}
	if got := requests.Load(); got < 2 {
		t.Fatalf("poll requests = %d; want at least one retry before deadline", got)
	}
}

// TestMailboxHTTPServerRateLimitsClaims verifies sender-IP and receiver-session limits return generic 429 responses.
func TestMailboxHTTPServerRateLimitsClaims(t *testing.T) {
	t.Run("global", func(t *testing.T) {
		server, endpoint := newMailboxLimitTestServer(t)
		server.admission.globalClaimLimit = 2
		for attempt := 0; attempt < 3; attempt++ {
			_, verifier := mailboxLimitTestCapability(t, byte(0x10+attempt))
			body := `{"role":"send","requested":"` + mailboxLimitTestSessionID(byte(0x10+attempt)) + `","capability_hash":"` + verifier + `"}`
			response := postMailboxLimitTestRequest(t, endpoint+mailboxHTTPClaimPath, body, "")
			want := http.StatusOK
			if attempt == 2 {
				want = http.StatusTooManyRequests
			}
			if response.Code != want {
				t.Fatalf("attempt %d status = %d; want %d", attempt+1, response.Code, want)
			}
		}
	})

	t.Run("sender IP", func(t *testing.T) {
		server, endpoint := newMailboxLimitTestServer(t)
		for attempt := 0; attempt <= defaultSenderClaimLimit; attempt++ {
			_, verifier := mailboxLimitTestCapability(t, byte(0x20+attempt))
			body := `{"role":"send","requested":"` + mailboxLimitTestSessionID(byte(0x20+attempt)) + `","capability_hash":"` + verifier + `"}`
			response := postMailboxLimitTestRequest(t, endpoint+mailboxHTTPClaimPath, body, "")
			want := http.StatusOK
			if attempt == defaultSenderClaimLimit {
				want = http.StatusTooManyRequests
			}
			if response.Code != want {
				t.Fatalf("attempt %d status = %d; want %d", attempt+1, response.Code, want)
			}
			if want == http.StatusTooManyRequests {
				assertGenericMailboxRateLimit(t, response)
			}
		}
		assertMailboxLimitScopeCount(t, server.client, "claim-global", defaultSenderClaimLimit)
	})

	t.Run("receiver session", func(t *testing.T) {
		server, endpoint := newMailboxLimitTestServer(t)
		ctx := context.Background()
		code := mailboxLimitTestSessionID(0x45)
		_, senderVerifier := mailboxLimitTestCapability(t, 0x31)
		if _, err := server.store.registerSender(ctx, code, senderVerifier); err != nil {
			t.Fatalf("register sender: %v", err)
		}
		_, verifier := mailboxLimitTestCapability(t, 0x32)
		body := `{"role":"recv","requested":"` + code + `","capability_hash":"` + verifier + `"}`
		for attempt := 0; attempt <= defaultReceiverSessionLimit; attempt++ {
			response := postMailboxLimitTestRequest(t, endpoint+mailboxHTTPClaimPath, body, "")
			want := http.StatusOK
			if attempt == defaultReceiverSessionLimit {
				want = http.StatusTooManyRequests
			}
			if response.Code != want {
				t.Fatalf("attempt %d status = %d; want %d", attempt+1, response.Code, want)
			}
			if want == http.StatusTooManyRequests {
				assertGenericMailboxRateLimit(t, response)
			}
		}
		assertMailboxLimitScopeCount(t, server.client, "claim-global", defaultReceiverSessionLimit)
	})
}

// TestMailboxHTTPServerPreAuthLimitsRotatingCapabilities verifies random Bearers cannot create unbounded limiter keys.
func TestMailboxHTTPServerPreAuthLimitsRotatingCapabilities(t *testing.T) {
	server, endpoint := newMailboxLimitTestServer(t)
	server.admission.authenticatedGlobalLimit = 10
	server.admission.authenticatedIPLimit = 2
	body := `{"role":"send","code":"` + mailboxLimitTestSessionID(0x70) + `","info":{}}`
	for attempt := 0; attempt < 3; attempt++ {
		raw, _ := mailboxLimitTestCapability(t, byte(0x60+attempt))
		response := postMailboxLimitTestRequest(t, endpoint+mailboxHTTPSelfPath, body, raw)
		want := http.StatusUnauthorized
		if attempt == 2 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("attempt %d status = %d; want %d", attempt+1, response.Code, want)
		}
	}
	keys, err := server.client.Keys(context.Background(), mailboxV2StorePrefix+":limits:authenticated-capability:*").Result()
	if err != nil {
		t.Fatalf("list capability limiter keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("unauthenticated requests created capability limiter keys: %v", keys)
	}
	assertMailboxLimitScopeCount(t, server.client, "authenticated-global", 2)
}

// TestMailboxHTTPServerBoundsAuthenticatedOperations verifies capability traffic is independently limited.
func TestMailboxHTTPServerBoundsAuthenticatedOperations(t *testing.T) {
	server, endpoint := newMailboxLimitTestServer(t)
	server.admission.authenticatedLimit = 1
	mailbox := newHTTPMailbox(endpoint, "send", time.Second).(*httpMailbox)
	code, err := mailbox.Claim(context.Background(), mailboxLimitTestSessionID(0x72))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	body := `{"role":"send","code":"` + code + `","info":{}}`
	first := postMailboxLimitTestRequest(t, endpoint+mailboxHTTPSelfPath, body, mailbox.capability)
	if first.Code != http.StatusOK {
		t.Fatalf("first operation status = %d; want %d", first.Code, http.StatusOK)
	}
	second := postMailboxLimitTestRequest(t, endpoint+mailboxHTTPSelfPath, body, mailbox.capability)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second operation status = %d; want %d", second.Code, http.StatusTooManyRequests)
	}
	assertGenericMailboxRateLimit(t, second)
}

// TestMailboxHTTPServerPollTimeoutReturnsNoContent verifies server-side waits yield bounded poll-again responses.
func TestMailboxHTTPServerPollTimeoutReturnsNoContent(t *testing.T) {
	server, endpoint := newMailboxLimitTestServer(t)
	server.admission.pollTimeout = 10 * time.Millisecond
	mailbox := newHTTPMailbox(endpoint, "send", time.Second).(*httpMailbox)
	code, err := mailbox.Claim(context.Background(), mailboxLimitTestSessionID(0x73))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	body := `{"role":"send","code":"` + code + `"}`
	response := postMailboxLimitTestRequest(t, endpoint+mailboxHTTPWaitPeerPath, body, mailbox.capability)
	if response.Code != http.StatusNoContent {
		t.Fatalf("poll status = %d; want %d; body=%q", response.Code, http.StatusNoContent, response.Body.String())
	}
}

// TestMailboxHTTPServerRejectsInvalidSelfInfoBeforePersistence verifies unbounded peer metadata never reaches Redis.
func TestMailboxHTTPServerRejectsInvalidSelfInfoBeforePersistence(t *testing.T) {
	server, endpoint := newMailboxLimitTestServer(t)
	mailbox := newHTTPMailbox(endpoint, "send", time.Second).(*httpMailbox)
	code, err := mailbox.Claim(context.Background(), mailboxLimitTestSessionID(0x74))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	features := make([]string, maxSelfFeatureCount+1)
	for index := range features {
		features[index] = "feature"
	}
	var body bytes.Buffer
	if err := jsonEncodeMailboxLimitTest(&body, map[string]any{
		"role": "send",
		"code": code,
		"info": rendezvous.SelfInfo{Features: features},
	}); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	response := postMailboxLimitTestRequest(t, endpoint+mailboxHTTPSelfPath, strings.TrimSpace(body.String()), mailbox.capability)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want %d", response.Code, http.StatusBadRequest)
	}
	session, err := server.store.load(context.Background(), code)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if session.Sender == nil || session.Sender.Info != nil {
		t.Fatalf("invalid self info was persisted: %+v", session.Sender)
	}
}

// newMailboxLimitTestServer starts an HTTP mailbox backed by an isolated Redis instance.
func newMailboxLimitTestServer(t *testing.T) (*MailboxHTTPServer, string) {
	t.Helper()
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	server, err := NewMailboxHTTPServer(mini.Addr(), time.Minute)
	if err != nil {
		mini.Close()
		t.Fatalf("NewMailboxHTTPServer: %v", err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		httpServer.Close()
		_ = server.Close()
		mini.Close()
	})
	return server, httpServer.URL
}

// postMailboxLimitTestRequest posts one JSON request and captures its response.
func postMailboxLimitTestRequest(t *testing.T, endpoint, body, capability string) *httptest.ResponseRecorder {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if capability != "" {
		request.Header.Set("Authorization", "Bearer "+capability)
	}
	response := httptest.NewRecorder()
	result, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	defer result.Body.Close()
	for key, values := range result.Header {
		response.Header()[key] = append([]string(nil), values...)
	}
	response.WriteHeader(result.StatusCode)
	_, _ = response.Body.ReadFrom(result.Body)
	return response
}

// assertGenericMailboxRateLimit verifies a throttled response exposes no limiter identity.
func assertGenericMailboxRateLimit(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("rate-limited response omitted Retry-After")
	}
	if !strings.Contains(response.Body.String(), errMailboxUnavailable.Error()) {
		t.Fatalf("rate-limit body = %q; want generic mailbox error", response.Body.String())
	}
}

// assertMailboxLimitScopeCount verifies rejected narrow-scope traffic does not consume the global ceiling.
func assertMailboxLimitScopeCount(t *testing.T, client *redis.Client, scope string, want int) {
	t.Helper()
	keys, err := client.Keys(context.Background(), mailboxV2StorePrefix+":limits:"+scope+":*").Result()
	if err != nil {
		t.Fatalf("list %s limiter keys: %v", scope, err)
	}
	if len(keys) != 1 {
		t.Fatalf("%s limiter keys = %v; want one", scope, keys)
	}
	raw, err := client.Get(context.Background(), keys[0]).Result()
	if err != nil {
		t.Fatalf("read %s limiter key: %v", scope, err)
	}
	got, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("parse %s limiter count %q: %v", scope, raw, err)
	}
	if got != want {
		t.Fatalf("%s limiter count = %d; want %d", scope, got, want)
	}
}

// jsonEncodeMailboxLimitTest encodes request data without duplicating JSON escaping in tests.
func jsonEncodeMailboxLimitTest(dst *bytes.Buffer, value any) error {
	return json.NewEncoder(dst).Encode(value)
}

// mailboxLimitTestSessionID returns a deterministic valid opaque mailbox identifier.
func mailboxLimitTestSessionID(marker byte) string {
	return mailboxSessionIDPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{marker}, mailboxSessionIDBytes))
}

// mailboxLimitTestCapability returns deterministic raw and verifier capability material.
func mailboxLimitTestCapability(t *testing.T, marker byte) (string, string) {
	t.Helper()
	raw, verifier, err := generateMailboxCapability(bytes.NewReader(bytes.Repeat([]byte{marker}, mailboxCapabilitySize)))
	if err != nil {
		t.Fatalf("generate capability: %v", err)
	}
	return raw, verifier
}

// mailboxLimitTestHTTPResponse builds an in-memory HTTP response.
func mailboxLimitTestHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
