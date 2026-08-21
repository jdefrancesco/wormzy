package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

// newSessionStoreTestHarness creates an isolated Redis-backed session store.
func newSessionStoreTestHarness(t *testing.T, ttl time.Duration) (*sessionStore, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
		mini.Close()
	})
	return newSessionStore(client, ttl, "wormzy-test"), client, mini
}

type sessionStoreTestCapabilities struct {
	sender   string
	receiver string
}

// sessionStoreTestID returns a deterministic valid opaque mailbox session identifier.
func sessionStoreTestID(marker byte) string {
	return mailboxSessionIDPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{marker}, mailboxSessionIDBytes))
}

// sessionStoreRedisKey resolves a validated session key for test inspection.
func sessionStoreRedisKey(t *testing.T, store *sessionStore, sessionID string) string {
	t.Helper()
	key, err := store.key(sessionID)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	return key
}

// sessionStoreTestCapability returns a deterministic valid verifier for store tests.
func sessionStoreTestCapability(t *testing.T, marker byte) string {
	t.Helper()
	raw := make([]byte, mailboxCapabilitySize)
	for i := range raw {
		raw[i] = marker
	}
	return mailboxCapabilityVerifierBytes(raw)
}

// registerSessionStorePeers registers both valid roles for queue tests.
func registerSessionStorePeers(t *testing.T, store *sessionStore, code string) sessionStoreTestCapabilities {
	t.Helper()
	ctx := context.Background()
	capabilities := sessionStoreTestCapabilities{
		sender:   sessionStoreTestCapability(t, 0x11),
		receiver: sessionStoreTestCapability(t, 0x22),
	}
	if _, err := store.registerSender(ctx, code, capabilities.sender); err != nil {
		t.Fatalf("register sender: %v", err)
	}
	if _, err := store.registerReceiver(ctx, code, capabilities.receiver); err != nil {
		t.Fatalf("register receiver: %v", err)
	}
	return capabilities
}

// TestSessionStoreBoundsGlobalActiveSessions verifies expired or deleted
// sessions free capacity while live sessions enforce the shared ceiling.
func TestSessionStoreBoundsGlobalActiveSessions(t *testing.T) {
	store, client, _ := newSessionStoreTestHarness(t, time.Minute)
	store.maxSessions = 2
	ctx := context.Background()
	first := sessionStoreTestID(0x01)
	second := sessionStoreTestID(0x02)
	third := sessionStoreTestID(0x03)

	for index, code := range []string{first, second} {
		capability := sessionStoreTestCapability(t, byte(0x41+index))
		if _, err := store.registerSender(ctx, code, capability); err != nil {
			t.Fatalf("register sender %d: %v", index, err)
		}
	}
	if _, err := store.registerSender(ctx, third, sessionStoreTestCapability(t, 0x43)); !errors.Is(err, errMailboxCapacity) {
		t.Fatalf("over-capacity registration error = %v; want %v", err, errMailboxCapacity)
	}
	if err := store.delete(ctx, first); err != nil {
		t.Fatalf("delete first session: %v", err)
	}
	if _, err := store.registerSender(ctx, third, sessionStoreTestCapability(t, 0x43)); err != nil {
		t.Fatalf("registration after delete: %v", err)
	}

	if err := client.ZAdd(ctx, store.activeSessionsKey(),
		redis.Z{Score: 0, Member: second},
		redis.Z{Score: 0, Member: third},
	).Err(); err != nil {
		t.Fatalf("age active-session index: %v", err)
	}
	fourth := sessionStoreTestID(0x04)
	if _, err := store.registerSender(ctx, fourth, sessionStoreTestCapability(t, 0x44)); err != nil {
		t.Fatalf("registration after expiry: %v", err)
	}
}

// sizedJSONBody returns a valid JSON string with the requested encoded size.
func sizedJSONBody(t *testing.T, size int) json.RawMessage {
	t.Helper()
	if size < 2 {
		t.Fatalf("JSON string size %d is too small", size)
	}
	body := json.RawMessage(`"` + string(make([]byte, size-2)) + `"`)
	for i := 1; i < len(body)-1; i++ {
		body[i] = 'x'
	}
	if !json.Valid(body) || len(body) != size {
		t.Fatalf("constructed JSON body size = %d, valid = %t; want %d and valid", len(body), json.Valid(body), size)
	}
	return body
}

// TestSessionStoreQueueBoundaryAndDequeueRecovery verifies the per-role cap frees capacity after dequeue.
func TestSessionStoreQueueBoundaryAndDequeueRecovery(t *testing.T) {
	store, _, _ := newSessionStoreTestHarness(t, time.Minute)
	code := sessionStoreTestID(0x41)
	capabilities := registerSessionStorePeers(t, store, code)
	ctx := context.Background()
	message := mailboxMessage{Type: "test", Body: json.RawMessage(`{"ok":true}`)}

	for i := 0; i < maxPendingMailboxMessagesPerRole; i++ {
		if err := store.enqueue(ctx, code, "send", "recv", capabilities.sender, message); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := store.enqueue(ctx, code, "send", "recv", capabilities.sender, message); !errors.Is(err, errMailboxQueueFull) {
		t.Fatalf("overflow enqueue error = %v; want %v", err, errMailboxQueueFull)
	}
	if _, err := store.dequeue(ctx, code, "recv", capabilities.receiver); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if err := store.enqueue(ctx, code, "send", "recv", capabilities.sender, message); err != nil {
		t.Fatalf("enqueue after dequeue: %v", err)
	}
	session, err := store.load(ctx, code)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(session.Pending["recv"]); got != maxPendingMailboxMessagesPerRole {
		t.Fatalf("receiver queue length = %d; want %d", got, maxPendingMailboxMessagesPerRole)
	}
}

// TestSessionStoreAggregateByteBoundary verifies both queues share one pending-byte budget.
func TestSessionStoreAggregateByteBoundary(t *testing.T) {
	store, _, _ := newSessionStoreTestHarness(t, time.Minute)
	code := sessionStoreTestID(0x42)
	capabilities := registerSessionStorePeers(t, store, code)
	ctx := context.Background()
	message := mailboxMessage{Type: "m", Body: sizedJSONBody(t, 32767)}

	for i := 0; i < 8; i++ {
		source, destination := "send", "recv"
		if i%2 == 1 {
			source, destination = "recv", "send"
		}
		capabilityHash := capabilities.sender
		if source == "recv" {
			capabilityHash = capabilities.receiver
		}
		if err := store.enqueue(ctx, code, source, destination, capabilityHash, message); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	session, err := store.load(ctx, code)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	bytes, err := sessionPendingMailboxBytes(session)
	if err != nil {
		t.Fatalf("pending bytes: %v", err)
	}
	if bytes != maxPendingMailboxBytesPerSession {
		t.Fatalf("pending bytes = %d; want %d", bytes, maxPendingMailboxBytesPerSession)
	}
	if err := store.enqueue(ctx, code, "send", "recv", capabilities.sender, mailboxMessage{Type: "m", Body: json.RawMessage(`0`)}); !errors.Is(err, errMailboxBytesFull) {
		t.Fatalf("byte overflow error = %v; want %v", err, errMailboxBytesFull)
	}
	if _, err := store.dequeue(ctx, code, "recv", capabilities.receiver); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if err := store.enqueue(ctx, code, "send", "recv", capabilities.sender, mailboxMessage{Type: "m", Body: json.RawMessage(`0`)}); err != nil {
		t.Fatalf("enqueue after byte recovery: %v", err)
	}
}

// TestSessionStoreConcurrentEnqueueHonorsAtomicCap verifies optimistic retries cannot overfill a queue.
func TestSessionStoreConcurrentEnqueueHonorsAtomicCap(t *testing.T) {
	store, _, _ := newSessionStoreTestHarness(t, time.Minute)
	code := sessionStoreTestID(0x43)
	capabilities := registerSessionStorePeers(t, store, code)
	ctx := context.Background()
	message := mailboxMessage{Type: "test", Body: json.RawMessage(`{"ok":true}`)}

	const attempts = 32
	start := make(chan struct{})
	errs := make(chan error, attempts)
	var workers sync.WaitGroup
	workers.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer workers.Done()
			<-start
			errs <- store.enqueue(ctx, code, "send", "recv", capabilities.sender, message)
		}()
	}
	close(start)
	workers.Wait()
	close(errs)

	succeeded := 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, errMailboxQueueFull):
		default:
			t.Fatalf("unexpected enqueue error: %v", err)
		}
	}
	if succeeded != maxPendingMailboxMessagesPerRole {
		t.Fatalf("successful enqueues = %d; want %d", succeeded, maxPendingMailboxMessagesPerRole)
	}
	session, err := store.load(ctx, code)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(session.Pending["recv"]); got != maxPendingMailboxMessagesPerRole {
		t.Fatalf("receiver queue length = %d; want %d", got, maxPendingMailboxMessagesPerRole)
	}
}

// TestSessionStoreEnqueueRequiresRegisteredSource verifies role identity before queue mutation.
func TestSessionStoreEnqueueRequiresRegisteredSource(t *testing.T) {
	store, _, _ := newSessionStoreTestHarness(t, time.Minute)
	code := sessionStoreTestID(0x44)
	ctx := context.Background()
	senderCapability := sessionStoreTestCapability(t, 0x11)
	receiverCapability := sessionStoreTestCapability(t, 0x22)
	if _, err := store.registerSender(ctx, code, senderCapability); err != nil {
		t.Fatalf("register sender: %v", err)
	}
	message := mailboxMessage{Type: "test", Body: json.RawMessage(`{"ok":true}`)}

	if err := store.enqueue(ctx, code, "recv", "send", receiverCapability, message); !errors.Is(err, errMailboxAuthentication) {
		t.Fatalf("unregistered receiver error = %v; want %v", err, errMailboxAuthentication)
	}
	if err := store.enqueue(ctx, code, "invalid", "send", senderCapability, message); !errors.Is(err, errInvalidRole) {
		t.Fatalf("invalid source error = %v; want %v", err, errInvalidRole)
	}
	if err := store.enqueue(ctx, code, "send", "send", senderCapability, message); !errors.Is(err, errInvalidRole) {
		t.Fatalf("same-role destination error = %v; want %v", err, errInvalidRole)
	}
	if err := store.enqueue(ctx, code, "send", "recv", senderCapability, message); err != nil {
		t.Fatalf("registered sender enqueue: %v", err)
	}
}

// TestSessionStoreClaimIsIdempotentForSameVerifier verifies lost claim responses can be retried safely.
func TestSessionStoreClaimIsIdempotentForSameVerifier(t *testing.T) {
	store, client, _ := newSessionStoreTestHarness(t, time.Minute)
	code := sessionStoreTestID(0x45)
	ctx := context.Background()
	senderRaw, senderHash, err := generateMailboxCapability(bytes.NewReader(bytes.Repeat([]byte{0x31}, mailboxCapabilitySize)))
	if err != nil {
		t.Fatalf("sender capability: %v", err)
	}
	receiverRaw, receiverHash, err := generateMailboxCapability(bytes.NewReader(bytes.Repeat([]byte{0x32}, mailboxCapabilitySize)))
	if err != nil {
		t.Fatalf("receiver capability: %v", err)
	}
	wrongHash := sessionStoreTestCapability(t, 0x33)

	first, err := store.registerSender(ctx, code, senderHash)
	if err != nil {
		t.Fatalf("first sender claim: %v", err)
	}
	second, err := store.registerSender(ctx, code, senderHash)
	if err != nil {
		t.Fatalf("retried sender claim: %v", err)
	}
	if first.Sender.ID != second.Sender.ID || second.NextSideID != 2 {
		t.Fatalf("sender retry changed registration: first=%+v second=%+v", first.Sender, second.Sender)
	}
	if _, err := store.registerSender(ctx, code, wrongHash); !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("different sender verifier error = %v; want %v", err, errMailboxUnavailable)
	}
	if _, err := store.registerReceiver(ctx, code, senderHash); !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("shared role verifier error = %v; want %v", err, errMailboxUnavailable)
	}
	firstReceiver, err := store.registerReceiver(ctx, code, receiverHash)
	if err != nil {
		t.Fatalf("first receiver claim: %v", err)
	}
	secondReceiver, err := store.registerReceiver(ctx, code, receiverHash)
	if err != nil {
		t.Fatalf("retried receiver claim: %v", err)
	}
	if firstReceiver.Receiver.ID != secondReceiver.Receiver.ID || secondReceiver.NextSideID != 3 {
		t.Fatalf("receiver retry changed registration: first=%+v second=%+v", firstReceiver.Receiver, secondReceiver.Receiver)
	}
	if _, err := store.registerReceiver(ctx, code, wrongHash); !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("different receiver verifier error = %v; want %v", err, errMailboxUnavailable)
	}
	persisted, err := client.Get(ctx, sessionStoreRedisKey(t, store, code)).Bytes()
	if err != nil {
		t.Fatalf("load persisted session: %v", err)
	}
	if bytes.Contains(persisted, []byte(senderRaw)) || bytes.Contains(persisted, []byte(receiverRaw)) {
		t.Fatal("persisted session contains a raw capability")
	}
}

// TestSessionStoreUsesOpaqueIDAndDisplayAlias verifies storage identity remains separate from dashboard text.
func TestSessionStoreUsesOpaqueIDAndDisplayAlias(t *testing.T) {
	code := sessionStoreTestID(0x48)
	session := newSession(code, time.Minute)
	if session.Code != code {
		t.Fatalf("storage code = %q; want %q", session.Code, code)
	}
	if session.Alias != mailboxSessionAlias(code) || session.Alias == code {
		t.Fatalf("display alias = %q; want %q distinct from storage ID", session.Alias, mailboxSessionAlias(code))
	}
}

// TestSessionStoreRejectsInvalidSessionIDsBeforeRedis verifies no operation constructs an unsafe key.
func TestSessionStoreRejectsInvalidSessionIDsBeforeRedis(t *testing.T) {
	store := &sessionStore{}
	ctx := context.Background()
	capabilityHash := sessionStoreTestCapability(t, 0x49)
	message := mailboxMessage{Type: "test", Body: json.RawMessage(`{}`)}
	tests := []struct {
		name string
		call func() error
		want error
	}{
		{name: "key", call: func() error { _, err := store.key("pairing-secret"); return err }, want: errMailboxUnavailable},
		{name: "register sender", call: func() error { _, err := store.registerSender(ctx, "pairing-secret", capabilityHash); return err }, want: errMailboxUnavailable},
		{name: "register receiver", call: func() error { _, err := store.registerReceiver(ctx, "pairing-secret", capabilityHash); return err }, want: errMailboxUnavailable},
		{name: "store self", call: func() error {
			return store.updatePeerInfo(ctx, "pairing-secret", "send", capabilityHash, rendezvous.SelfInfo{})
		}, want: errMailboxAuthentication},
		{name: "wait peer", call: func() error {
			_, err := store.waitForPeer(ctx, "pairing-secret", "send", capabilityHash)
			return err
		}, want: errMailboxAuthentication},
		{name: "enqueue", call: func() error {
			return store.enqueue(ctx, "pairing-secret", "send", "recv", capabilityHash, message)
		}, want: errMailboxAuthentication},
		{name: "dequeue", call: func() error {
			_, err := store.dequeue(ctx, "pairing-secret", "send", capabilityHash)
			return err
		}, want: errMailboxAuthentication},
		{name: "stats", call: func() error {
			return store.recordStats(ctx, "pairing-secret", "send", capabilityHash, transferStats{})
		}, want: errMailboxAuthentication},
		{name: "load", call: func() error { _, err := store.load(ctx, "pairing-secret"); return err }, want: errMailboxUnavailable},
		{name: "delete", call: func() error { return store.delete(ctx, "pairing-secret") }, want: errMailboxUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v; want %v", err, tt.want)
			}
		})
	}
}

// TestSessionStoreCapabilityIsRoleIsolated verifies a receiver capability cannot act as sender.
func TestSessionStoreCapabilityIsRoleIsolated(t *testing.T) {
	store, _, _ := newSessionStoreTestHarness(t, time.Minute)
	code := sessionStoreTestID(0x46)
	capabilities := registerSessionStorePeers(t, store, code)
	ctx := context.Background()
	message := mailboxMessage{Type: "test", Body: json.RawMessage(`{"ok":true}`)}
	tests := []struct {
		name string
		call func() error
	}{
		{name: "store self", call: func() error {
			return store.updatePeerInfo(ctx, code, "send", capabilities.receiver, rendezvous.SelfInfo{})
		}},
		{name: "wait peer", call: func() error {
			_, err := store.waitForPeer(ctx, code, "send", capabilities.receiver)
			return err
		}},
		{name: "enqueue", call: func() error {
			return store.enqueue(ctx, code, "send", "recv", capabilities.receiver, message)
		}},
		{name: "dequeue", call: func() error {
			_, err := store.dequeue(ctx, code, "send", capabilities.receiver)
			return err
		}},
		{name: "stats", call: func() error {
			return store.recordStats(ctx, code, "send", capabilities.receiver, transferStats{})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, errMailboxAuthentication) {
				t.Fatalf("error = %v; want %v", err, errMailboxAuthentication)
			}
		})
	}
}

// TestSessionStoreReceiverStatsRemainAuthoritative verifies peer report order
// cannot nondeterministically change the dashboard's final transfer outcome.
func TestSessionStoreReceiverStatsRemainAuthoritative(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
	}{
		{name: "sender then receiver", roles: []string{"send", "recv"}},
		{name: "receiver then sender", roles: []string{"recv", "send"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _, _ := newSessionStoreTestHarness(t, time.Minute)
			code := sessionStoreTestID(0x50)
			capabilities := registerSessionStorePeers(t, store, code)
			byRole := map[string]struct {
				capability string
				stats      transferStats
			}{
				"send": {
					capability: capabilities.sender,
					stats: transferStats{
						Mode: "send", Completed: true, Transport: "p2p", Candidate: "ice-p2p", Bytes: 99,
					},
				},
				"recv": {
					capability: capabilities.receiver,
					stats: transferStats{
						Mode: "recv", Completed: false, Transport: "p2p", Candidate: "ice-p2p", Error: "transfer",
					},
				},
			}
			for _, role := range tt.roles {
				report := byRole[role]
				if err := store.recordStats(context.Background(), code, role, report.capability, report.stats); err != nil {
					t.Fatalf("record %s stats: %v", role, err)
				}
			}

			sess, err := store.load(context.Background(), code)
			if err != nil {
				t.Fatalf("load session: %v", err)
			}
			if len(sess.StatsByRole) != 2 {
				t.Fatalf("role stats count = %d; want 2", len(sess.StatsByRole))
			}
			if sess.Stats == nil || sess.Stats.Mode != "recv" || sess.Stats.Completed || sess.Stats.Error != "transfer" {
				t.Fatalf("authoritative stats = %+v; want receiver failure", sess.Stats)
			}
		})
	}
}

// TestSessionStoreRejectsRolesAndEnvelopesBeforeRedis verifies invalid input cannot trigger a store access.
func TestSessionStoreRejectsRolesAndEnvelopesBeforeRedis(t *testing.T) {
	store := &sessionStore{}
	ctx := context.Background()
	capabilityHash := sessionStoreTestCapability(t, 0x11)
	validMessage := mailboxMessage{Type: "test", Body: json.RawMessage(`{"ok":true}`)}
	tests := []struct {
		name string
		call func() error
	}{
		{name: "update peer", call: func() error {
			return store.updatePeerInfo(ctx, "missing", "invalid", capabilityHash, rendezvous.SelfInfo{})
		}},
		{name: "wait peer", call: func() error {
			_, err := store.waitForPeer(ctx, "missing", "invalid", capabilityHash)
			return err
		}},
		{name: "enqueue source", call: func() error { return store.enqueue(ctx, "missing", "invalid", "send", capabilityHash, validMessage) }},
		{name: "enqueue destination", call: func() error { return store.enqueue(ctx, "missing", "send", "invalid", capabilityHash, validMessage) }},
		{name: "dequeue", call: func() error {
			_, err := store.dequeue(ctx, "missing", "invalid", capabilityHash)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, errInvalidRole) {
				t.Fatalf("error = %v; want %v", err, errInvalidRole)
			}
		})
	}
	if got := oppositeRole("invalid"); got != "" {
		t.Fatalf("oppositeRole(invalid) = %q; want empty", got)
	}
	if err := store.enqueue(ctx, "missing", "send", "recv", capabilityHash, mailboxMessage{}); err == nil {
		t.Fatal("invalid envelope reached Redis")
	}
	if err := store.enqueue(ctx, "missing", "send", "recv", capabilityHash, mailboxMessage{Type: "test", Body: json.RawMessage(`not-json`)}); err == nil {
		t.Fatal("invalid JSON envelope reached Redis")
	}
}

// TestRedisMailboxRejectsInvalidRoleBeforeStore verifies every mailbox operation fails before using its store.
func TestRedisMailboxRejectsInvalidRoleBeforeStore(t *testing.T) {
	mailbox := &redisMailbox{role: "invalid", code: "test-code"}
	ctx := context.Background()
	tests := []struct {
		name string
		call func() error
	}{
		{name: "claim", call: func() error {
			_, err := mailbox.Claim(ctx, "test-code")
			return err
		}},
		{name: "store self", call: func() error { return mailbox.StoreSelf(ctx, rendezvous.SelfInfo{}) }},
		{name: "wait peer", call: func() error {
			_, err := mailbox.WaitPeer(ctx)
			return err
		}},
		{name: "send", call: func() error { return mailbox.Send(ctx, "test", struct{}{}) }},
		{name: "receive", call: func() error {
			_, err := mailbox.Receive(ctx)
			return err
		}},
		{name: "stats", call: func() error { return mailbox.ReportStats(ctx, transferStats{}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, errInvalidRole) {
				t.Fatalf("error = %v; want %v", err, errInvalidRole)
			}
		})
	}
	mailbox.Cleanup(ctx)
	if _, err := newRedisMailboxWithClient(nil, time.Minute, "invalid", nil); !errors.Is(err, errInvalidRole) {
		t.Fatalf("constructor error = %v; want %v", err, errInvalidRole)
	}
}

// TestSessionStoreRejectedEnqueueDoesNotRefreshTTL verifies a full queue cannot prolong a session.
func TestSessionStoreRejectedEnqueueDoesNotRefreshTTL(t *testing.T) {
	const ttl = time.Minute
	store, client, mini := newSessionStoreTestHarness(t, ttl)
	code := sessionStoreTestID(0x47)
	capabilities := registerSessionStorePeers(t, store, code)
	ctx := context.Background()
	message := mailboxMessage{Type: "test", Body: json.RawMessage(`{"ok":true}`)}
	for i := 0; i < maxPendingMailboxMessagesPerRole; i++ {
		if err := store.enqueue(ctx, code, "send", "recv", capabilities.sender, message); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	mini.FastForward(10 * time.Second)
	before, err := client.TTL(ctx, sessionStoreRedisKey(t, store, code)).Result()
	if err != nil {
		t.Fatalf("TTL before rejection: %v", err)
	}
	if err := store.enqueue(ctx, code, "send", "recv", capabilities.sender, message); !errors.Is(err, errMailboxQueueFull) {
		t.Fatalf("rejected enqueue error = %v; want %v", err, errMailboxQueueFull)
	}
	after, err := client.TTL(ctx, sessionStoreRedisKey(t, store, code)).Result()
	if err != nil {
		t.Fatalf("TTL after rejection: %v", err)
	}
	if after != before {
		t.Fatalf("TTL changed after rejected enqueue: before=%s after=%s", before, after)
	}
}

// TestSessionStoreRefreshStopsAtAbsoluteLifetime verifies successful activity
// refreshes the sliding lease without extending the immutable session deadline.
func TestSessionStoreRefreshStopsAtAbsoluteLifetime(t *testing.T) {
	const (
		lease       = 10 * time.Second
		maxLifetime = 25 * time.Second
	)
	store, client, mini := newSessionStoreTestHarness(t, lease)
	store.maxSessions = 1
	store.maxLifetime = maxLifetime
	now := time.Unix(1_800_000_000, 0).UTC()
	mini.SetTime(now)
	advance := func(elapsed time.Duration) {
		t.Helper()
		mini.FastForward(elapsed)
		now = now.Add(elapsed)
		mini.SetTime(now)
	}

	ctx := context.Background()
	code := sessionStoreTestID(0x70)
	capability := sessionStoreTestCapability(t, 0x70)
	sess, err := store.registerSender(ctx, code, capability)
	if err != nil {
		t.Fatalf("register sender: %v", err)
	}
	wantAbsoluteExpiry := time.Unix(sess.CreatedUnix, 0).Add(maxLifetime).UnixMilli()
	if sess.CreatedUnix != now.Unix() {
		t.Fatalf("created timestamp = %d; want Redis time %d", sess.CreatedUnix, now.Unix())
	}

	for _, elapsed := range []time.Duration{8 * time.Second, 8 * time.Second, 8 * time.Second} {
		advance(elapsed)
		if err := store.updatePeerInfo(ctx, code, "send", capability, rendezvous.SelfInfo{}); err != nil {
			t.Fatalf("refresh at %s: %v", now.Sub(time.Unix(sess.CreatedUnix, 0)), err)
		}
		score, err := client.ZScore(ctx, store.activeSessionsKey(), code).Result()
		if err != nil {
			t.Fatalf("active-session score: %v", err)
		}
		if got := int64(score); got > wantAbsoluteExpiry {
			t.Fatalf("active-session expiry = %d; exceeds absolute deadline %d", got, wantAbsoluteExpiry)
		}
	}

	remaining, err := client.PTTL(ctx, sessionStoreRedisKey(t, store, code)).Result()
	if err != nil {
		t.Fatalf("remaining TTL: %v", err)
	}
	if remaining <= 0 || remaining > time.Second {
		t.Fatalf("remaining TTL = %s; want positive and capped at 1s", remaining)
	}
	score, err := client.ZScore(ctx, store.activeSessionsKey(), code).Result()
	if err != nil {
		t.Fatalf("final active-session score: %v", err)
	}
	if got := int64(score); got != wantAbsoluteExpiry {
		t.Fatalf("final active-session expiry = %d; want absolute deadline %d", got, wantAbsoluteExpiry)
	}

	advance(2 * time.Second)
	replacement := sessionStoreTestID(0x71)
	if _, err := store.registerSender(ctx, replacement, sessionStoreTestCapability(t, 0x71)); err != nil {
		t.Fatalf("register replacement after absolute expiry: %v", err)
	}
	if count, err := client.ZCard(ctx, store.activeSessionsKey()).Result(); err != nil {
		t.Fatalf("active-session count: %v", err)
	} else if count != 1 {
		t.Fatalf("active-session count = %d; want 1 after stale capacity cleanup", count)
	}
	if _, err := store.load(ctx, code); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("expired session load error = %v; want %v", err, errSessionNotFound)
	}
}

// TestSessionStoreCreationCapsLeaseAtAbsoluteLifetime verifies an unusually
// long configured lease cannot exceed the fixed session ceiling at creation.
func TestSessionStoreCreationCapsLeaseAtAbsoluteLifetime(t *testing.T) {
	const maxLifetime = 25 * time.Second
	store, client, mini := newSessionStoreTestHarness(t, time.Minute)
	store.maxLifetime = maxLifetime
	now := time.Unix(1_800_000_000, 0).UTC()
	mini.SetTime(now)
	ctx := context.Background()
	code := sessionStoreTestID(0x75)

	sess, err := store.registerSender(ctx, code, sessionStoreTestCapability(t, 0x75))
	if err != nil {
		t.Fatalf("register sender: %v", err)
	}
	wantExpiry := time.Unix(sess.CreatedUnix, 0).Add(maxLifetime).UnixMilli()
	remaining, err := client.PTTL(ctx, sessionStoreRedisKey(t, store, code)).Result()
	if err != nil {
		t.Fatalf("session TTL: %v", err)
	}
	if remaining <= 0 || remaining > maxLifetime {
		t.Fatalf("session TTL = %s; want positive and capped at %s", remaining, maxLifetime)
	}
	score, err := client.ZScore(ctx, store.activeSessionsKey(), code).Result()
	if err != nil {
		t.Fatalf("active-session score: %v", err)
	}
	if got := int64(score); got != wantExpiry {
		t.Fatalf("active-session expiry = %d; want absolute deadline %d", got, wantExpiry)
	}
}

// TestSessionStoreRemovesInvalidLifetimeRecords verifies legacy or corrupt
// timestamps cannot bypass the absolute lifetime or retain capacity.
func TestSessionStoreRemovesInvalidLifetimeRecords(t *testing.T) {
	store, client, mini := newSessionStoreTestHarness(t, time.Minute)
	store.maxLifetime = 25 * time.Second
	now := time.Unix(1_800_000_000, 0).UTC()
	mini.SetTime(now)
	ctx := context.Background()

	tests := []struct {
		name        string
		marker      byte
		createdUnix int64
	}{
		{name: "missing creation time", marker: 0x72, createdUnix: 0},
		{name: "expired creation time", marker: 0x73, createdUnix: now.Add(-26 * time.Second).Unix()},
		{name: "future creation time", marker: 0x74, createdUnix: now.Add(time.Second).Unix()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := sessionStoreTestID(tt.marker)
			sess := newSessionAt(code, time.Minute, now)
			sess.CreatedUnix = tt.createdUnix
			payload, err := json.Marshal(sess)
			if err != nil {
				t.Fatalf("marshal corrupt session: %v", err)
			}
			key := sessionStoreRedisKey(t, store, code)
			if err := client.Set(ctx, key, payload, time.Minute).Err(); err != nil {
				t.Fatalf("store corrupt session: %v", err)
			}
			if err := client.ZAdd(ctx, store.activeSessionsKey(), redis.Z{
				Score: float64(now.Add(time.Minute).UnixMilli()), Member: code,
			}).Err(); err != nil {
				t.Fatalf("index corrupt session: %v", err)
			}

			if _, err := store.load(ctx, code); !errors.Is(err, errSessionNotFound) {
				t.Fatalf("load error = %v; want %v", err, errSessionNotFound)
			}
			if exists, err := client.Exists(ctx, key).Result(); err != nil {
				t.Fatalf("check session key: %v", err)
			} else if exists != 0 {
				t.Fatal("invalid session key was not removed")
			}
			if _, err := client.ZScore(ctx, store.activeSessionsKey(), code).Result(); !errors.Is(err, redis.Nil) {
				t.Fatalf("invalid session index error = %v; want redis.Nil", err)
			}
		})
	}
}
