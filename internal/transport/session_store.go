package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	maxPendingMailboxMessagesPerRole = 16
	maxPendingMailboxBytesPerSession = 256 << 10
	defaultMaxActiveMailboxSessions  = 2048
	defaultMaxMailboxSessionLifetime = 12 * time.Hour
	mailboxV2StorePrefix             = "wormzy:v2"
)

var (
	errSessionNotFound  = errors.New("session not found")
	errSenderInUse      = errors.New("sender already registered for pairing code")
	errReceiverInUse    = errors.New("receiver already registered for pairing code")
	errSenderMissing    = errors.New("sender not registered yet")
	errReceiverMissing  = errors.New("receiver not registered yet")
	errInvalidRole      = errors.New("invalid role")
	errNoPendingMessage = errors.New("no pending mailbox message")
	errMailboxQueueFull = errors.New("mailbox queue is full")
	errMailboxBytesFull = errors.New("mailbox pending-byte limit exceeded")
	errMailboxCapacity  = errors.New("mailbox active-session capacity reached")

	mailboxSessionCreateScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[1]) == 1 then
  return 2
end
local redis_time = redis.call("TIME")
local now_ms = (redis_time[1] * 1000) + math.floor(redis_time[2] / 1000)
redis.call("ZREMRANGEBYSCORE", KEYS[2], "-inf", now_ms)
if redis.call("ZCARD", KEYS[2]) >= tonumber(ARGV[1]) then
  return 0
end
local expires_ms = math.min(now_ms + tonumber(ARGV[3]), tonumber(ARGV[5]))
local ttl_ms = expires_ms - now_ms
if ttl_ms < 1 then
  return 3
end
local created = redis.call("SET", KEYS[1], ARGV[2], "PX", ttl_ms, "NX")
if not created then
  return 2
end
redis.call("ZADD", KEYS[2], expires_ms, ARGV[4])
return 1
`)

	mailboxSessionDeleteIfValueScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call("DEL", KEYS[1])
redis.call("ZREM", KEYS[2], ARGV[2])
return 1
`)
)

type sessionStore struct {
	client *redis.Client
	prefix string
	ttl    time.Duration

	maxSessions int64
	maxLifetime time.Duration
}

type rendezvousSession struct {
	Code        string                   `json:"code"`
	CreatedUnix int64                    `json:"created_unix"`
	TTLSeconds  int64                    `json:"ttl_seconds"`
	Sender      *sessionPeer             `json:"sender,omitempty"`
	Receiver    *sessionPeer             `json:"receiver,omitempty"`
	Pending     map[string][]msgPt       `json:"pending,omitempty"`
	NextSideID  uint32                   `json:"next_side_id"`
	Alias       string                   `json:"alias,omitempty"`
	Stats       *transferStats           `json:"stats,omitempty"`
	StatsByRole map[string]transferStats `json:"stats_by_role,omitempty"`
}

type sessionPeer struct {
	ID             uint32               `json:"id"`
	Role           string               `json:"role"`
	CapabilityHash string               `json:"capability_hash"`
	Info           *rendezvous.SelfInfo `json:"info,omitempty"`
	RegisteredAt   int64                `json:"registered_unix"`
	LastUpdate     int64                `json:"last_update_unix"`
}

type msgPt struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

type transferStats struct {
	Mode           string `json:"mode,omitempty"`
	Transport      string `json:"transport,omitempty"`
	Candidate      string `json:"candidate,omitempty"`
	DirectOutcome  string `json:"direct_outcome,omitempty"`
	DirectSummary  string `json:"direct_summary,omitempty"`
	Bytes          int64  `json:"bytes,omitempty"`
	DurationMillis int64  `json:"duration_ms,omitempty"`
	Completed      bool   `json:"completed"`
	Error          string `json:"error,omitempty"`
	UpdatedUnix    int64  `json:"updated_unix"`
}

// newSessionStore constructs a Redis-backed rendezvous session store.
func newSessionStore(client *redis.Client, ttl time.Duration, prefix string) *sessionStore {
	return &sessionStore{
		client:      client,
		prefix:      prefix,
		ttl:         ttl,
		maxSessions: defaultMaxActiveMailboxSessions,
		maxLifetime: defaultMaxMailboxSessionLifetime,
	}
}

// newSession initializes one opaque rendezvous session with a bounded lifetime.
func newSession(code string, ttl time.Duration) *rendezvousSession {
	return newSessionAt(code, ttl, time.Now())
}

// newSessionAt initializes one opaque rendezvous session at an authoritative creation time.
func newSessionAt(code string, ttl time.Duration, created time.Time) *rendezvousSession {
	alias := mailboxSessionAlias(code)
	return &rendezvousSession{
		Code:        code,
		CreatedUnix: created.Unix(),
		TTLSeconds:  int64(ttl / time.Second),
		Pending:     make(map[string][]msgPt),
		NextSideID:  1,
		Alias:       alias,
	}
}

// key validates an opaque session identifier and returns its Redis key.
func (st *sessionStore) key(code string) (string, error) {
	if !validMailboxSessionID(code) {
		return "", errMailboxUnavailable
	}
	return fmt.Sprintf("%s:sessions:%s", st.prefix, code), nil
}

// activeSessionsKey returns the shared expiry index used to bound live sessions.
func (st *sessionStore) activeSessionsKey() string {
	return fmt.Sprintf("%s:active-sessions", st.prefix)
}

// effectiveMaxSessions returns the configured global active-session ceiling.
func (st *sessionStore) effectiveMaxSessions() int64 {
	if st.maxSessions > 0 {
		return st.maxSessions
	}
	return defaultMaxActiveMailboxSessions
}

// effectiveMaxLifetime returns the absolute lifetime ceiling for one session.
func (st *sessionStore) effectiveMaxLifetime() time.Duration {
	if st.maxLifetime > 0 {
		return st.maxLifetime
	}
	return defaultMaxMailboxSessionLifetime
}

// sessionAbsoluteExpiryMillis validates the creation timestamp and returns
// the immutable absolute deadline according to Redis's clock.
func (st *sessionStore) sessionAbsoluteExpiryMillis(sess *rendezvousSession, redisNow time.Time) (int64, bool) {
	if sess == nil || sess.CreatedUnix <= 0 || sess.CreatedUnix > redisNow.Unix() {
		return 0, false
	}
	lifetimeMillis := st.effectiveMaxLifetime().Milliseconds()
	if lifetimeMillis < 1 || sess.CreatedUnix > (math.MaxInt64-lifetimeMillis)/int64(time.Second/time.Millisecond) {
		return 0, false
	}
	expiresMillis := sess.CreatedUnix*int64(time.Second/time.Millisecond) + lifetimeMillis
	if expiresMillis <= redisNow.UnixMilli() {
		return 0, false
	}
	return expiresMillis, true
}

// sessionRefreshExpiryMillis caps the sliding lease at the absolute deadline.
func (st *sessionStore) sessionRefreshExpiryMillis(sess *rendezvousSession, redisNow time.Time) (int64, bool) {
	absoluteExpiryMillis, ok := st.sessionAbsoluteExpiryMillis(sess, redisNow)
	if !ok || st.ttl <= 0 {
		return 0, false
	}
	leaseExpiryMillis := redisNow.Add(st.ttl).UnixMilli()
	if leaseExpiryMillis > absoluteExpiryMillis {
		leaseExpiryMillis = absoluteExpiryMillis
	}
	if leaseExpiryMillis <= redisNow.UnixMilli() {
		return 0, false
	}
	return leaseExpiryMillis, true
}

// registerSender claims the sender role with a capability verifier.
func (st *sessionStore) registerSender(ctx context.Context, code, capabilityHash string) (*rendezvousSession, error) {
	if err := validateMailboxCapabilityVerifier(capabilityHash); err != nil {
		return nil, err
	}
	created, err := st.createSenderSession(ctx, code, capabilityHash)
	if err != nil || created != nil {
		return created, err
	}
	return st.modify(ctx, code, func(sess *rendezvousSession) error {
		if sess.Sender != nil {
			if mailboxCapabilityVerifierEqual(sess.Sender.CapabilityHash, capabilityHash) {
				return nil
			}
			return errMailboxUnavailable
		}
		sess.Sender = &sessionPeer{
			ID:             sess.NextSideID,
			Role:           "send",
			CapabilityHash: capabilityHash,
			RegisteredAt:   time.Now().Unix(),
			LastUpdate:     time.Now().Unix(),
		}
		sess.NextSideID++
		return nil
	})
}

// createSenderSession atomically reserves bounded global capacity and creates
// a new sender session. A nil session and nil error means the key already exists.
func (st *sessionStore) createSenderSession(ctx context.Context, code, capabilityHash string) (*rendezvousSession, error) {
	key, err := st.key(code)
	if err != nil {
		return nil, err
	}
	redisNow, err := st.client.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	sess := newSessionAt(code, st.ttl, redisNow)
	sess.Sender = &sessionPeer{
		ID:             sess.NextSideID,
		Role:           "send",
		CapabilityHash: capabilityHash,
		RegisteredAt:   redisNow.Unix(),
		LastUpdate:     redisNow.Unix(),
	}
	sess.NextSideID++
	payload, err := json.Marshal(sess)
	if err != nil {
		return nil, err
	}
	ttlMillis := st.ttl.Milliseconds()
	if ttlMillis < 1 {
		ttlMillis = 1
	}
	absoluteExpiryMillis, ok := st.sessionAbsoluteExpiryMillis(sess, redisNow)
	if !ok {
		return nil, errMailboxUnavailable
	}
	result, err := mailboxSessionCreateScript.Run(
		ctx,
		st.client,
		[]string{key, st.activeSessionsKey()},
		strconv.FormatInt(st.effectiveMaxSessions(), 10),
		payload,
		strconv.FormatInt(ttlMillis, 10),
		code,
		strconv.FormatInt(absoluteExpiryMillis, 10),
	).Int64()
	if err != nil {
		return nil, err
	}
	switch result {
	case 0:
		return nil, errMailboxCapacity
	case 1:
		return sess, nil
	case 2:
		return nil, nil
	case 3:
		return nil, errMailboxUnavailable
	default:
		return nil, errMailboxUnavailable
	}
}

// registerReceiver claims the receiver role after a sender has created the session.
func (st *sessionStore) registerReceiver(ctx context.Context, code, capabilityHash string) (*rendezvousSession, error) {
	if err := validateMailboxCapabilityVerifier(capabilityHash); err != nil {
		return nil, err
	}
	sess, err := st.modify(ctx, code, func(sess *rendezvousSession) error {
		if sess.Receiver != nil {
			if mailboxCapabilityVerifierEqual(sess.Receiver.CapabilityHash, capabilityHash) {
				return nil
			}
			return errMailboxUnavailable
		}
		if sess.Sender == nil {
			return errMailboxUnavailable
		}
		if mailboxCapabilityVerifierEqual(sess.Sender.CapabilityHash, capabilityHash) {
			return errMailboxUnavailable
		}
		sess.Receiver = &sessionPeer{
			ID:             sess.NextSideID,
			Role:           "recv",
			CapabilityHash: capabilityHash,
			RegisteredAt:   time.Now().Unix(),
			LastUpdate:     time.Now().Unix(),
		}
		sess.NextSideID++
		return nil
	})
	if errors.Is(err, errSessionNotFound) {
		compareMissingMailboxCapability(capabilityHash)
		return nil, errMailboxUnavailable
	}
	return sess, err
}

// updatePeerInfo stores candidate metadata for an authenticated session role.
func (st *sessionStore) updatePeerInfo(ctx context.Context, code, role, capabilityHash string, info rendezvous.SelfInfo) error {
	if err := validateMailboxRole(role); err != nil {
		return err
	}
	_, err := st.modify(ctx, code, func(sess *rendezvousSession) error {
		peer, err := st.authenticateSessionRole(sess, role, capabilityHash)
		if err != nil {
			return err
		}
		cpy := info
		peer.Info = &cpy
		peer.LastUpdate = time.Now().Unix()
		return nil
	})
	return authenticatedStoreError(err, capabilityHash)
}

// peerForRole returns the registered peer for a valid role.
func (st *sessionStore) peerForRole(sess *rendezvousSession, role string) *sessionPeer {
	if sess == nil {
		return nil
	}
	switch role {
	case "send":
		return sess.Sender
	case "recv":
		return sess.Receiver
	default:
		return nil
	}
}

// waitForPeer waits until the opposite role has published candidate metadata.
func (st *sessionStore) waitForPeer(ctx context.Context, code, role, capabilityHash string) (*rendezvous.SelfInfo, error) {
	if err := validateMailboxRole(role); err != nil {
		return nil, err
	}
	peerRole := oppositeRole(role)
	for {
		sess, err := st.load(ctx, code)
		if err != nil {
			return nil, authenticatedStoreError(err, capabilityHash)
		}
		if _, err := st.authenticateSessionRole(sess, role, capabilityHash); err != nil {
			return nil, err
		}
		peer := st.peerForRole(sess, peerRole)
		if peer != nil && peer.Info != nil {
			info := *peer.Info
			return &info, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// oppositeRole returns the peer role for a valid sender or receiver role.
func oppositeRole(role string) string {
	switch role {
	case "send":
		return "recv"
	case "recv":
		return "send"
	default:
		return ""
	}
}

// validateMailboxRole accepts only the two roles represented by a rendezvous session.
func validateMailboxRole(role string) error {
	if role != "send" && role != "recv" {
		return errInvalidRole
	}
	return nil
}

// enqueue appends one bounded message from an authenticated role to its peer's queue.
func (st *sessionStore) enqueue(ctx context.Context, code, sourceRole, destRole, capabilityHash string, msg mailboxMessage) error {
	if err := validateMailboxRole(sourceRole); err != nil {
		return err
	}
	if err := validateMailboxRole(destRole); err != nil {
		return err
	}
	if destRole != oppositeRole(sourceRole) {
		return errInvalidRole
	}
	if err := validateMailboxMessage(msg); err != nil {
		return err
	}
	if !json.Valid(msg.Body) {
		return errors.New("mailbox message body is invalid JSON")
	}
	_, err := st.modify(ctx, code, func(sess *rendezvousSession) error {
		if _, err := st.authenticateSessionRole(sess, sourceRole, capabilityHash); err != nil {
			return err
		}
		if sess.Pending == nil {
			sess.Pending = make(map[string][]msgPt)
		}
		if len(sess.Pending[destRole]) >= maxPendingMailboxMessagesPerRole {
			return errMailboxQueueFull
		}
		pendingBytes, err := sessionPendingMailboxBytes(sess)
		if err != nil {
			return err
		}
		messageBytes := len(msg.Type) + len(msg.Body)
		if pendingBytes > maxPendingMailboxBytesPerSession-messageBytes {
			return errMailboxBytesFull
		}
		raw := msgPt{Type: msg.Type, Body: msg.Body}
		sess.Pending[destRole] = append(sess.Pending[destRole], raw)
		return nil
	})
	return authenticatedStoreError(err, capabilityHash)
}

// sessionPendingMailboxBytes totals queued envelope data while enforcing the aggregate limit.
func sessionPendingMailboxBytes(sess *rendezvousSession) (int, error) {
	total := 0
	for _, queue := range sess.Pending {
		for _, message := range queue {
			messageBytes := len(message.Type) + len(message.Body)
			if total > maxPendingMailboxBytesPerSession-messageBytes {
				return 0, errMailboxBytesFull
			}
			total += messageBytes
		}
	}
	return total, nil
}

// dequeue waits for and removes the oldest message addressed to an authenticated role.
func (st *sessionStore) dequeue(ctx context.Context, code, role, capabilityHash string) (mailboxMessage, error) {
	if err := validateMailboxRole(role); err != nil {
		return mailboxMessage{}, err
	}
	for {
		var out mailboxMessage
		var ok bool
		_, err := st.modify(ctx, code, func(sess *rendezvousSession) error {
			if _, err := st.authenticateSessionRole(sess, role, capabilityHash); err != nil {
				return err
			}
			queue := sess.Pending[role]
			if len(queue) == 0 {
				return errNoPendingMessage
			}
			item := queue[0]
			sess.Pending[role] = queue[1:]
			ok = true
			out = mailboxMessage{Type: item.Type, Body: item.Body}
			return nil
		})
		if err == errNoPendingMessage {
			select {
			case <-ctx.Done():
				return mailboxMessage{}, ctx.Err()
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		if err != nil {
			return mailboxMessage{}, authenticatedStoreError(err, capabilityHash)
		}
		if ok {
			return out, nil
		}
	}
}

// delete removes one opaque rendezvous session.
func (st *sessionStore) delete(ctx context.Context, code string) error {
	key, err := st.key(code)
	if err != nil {
		return err
	}
	_, err = st.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, key)
		pipe.ZRem(ctx, st.activeSessionsKey(), code)
		return nil
	})
	return err
}

// recordStats validates and stores client-reported telemetry for an authenticated role.
func (st *sessionStore) recordStats(ctx context.Context, code, role, capabilityHash string, stats transferStats) error {
	if err := validateMailboxRole(role); err != nil {
		return err
	}
	_, err := st.modify(ctx, code, func(sess *rendezvousSession) error {
		if _, err := st.authenticateSessionRole(sess, role, capabilityHash); err != nil {
			return err
		}
		validated, err := validateAndSanitizeTransferStats(role, stats)
		if err != nil {
			return err
		}
		validated.UpdatedUnix = time.Now().Unix()
		if sess.StatsByRole == nil {
			sess.StatsByRole = make(map[string]transferStats, 2)
		}
		sess.StatsByRole[role] = validated
		resolved := authoritativeTransferStats(sess.StatsByRole)
		sess.Stats = &resolved
		return nil
	})
	return authenticatedStoreError(err, capabilityHash)
}

// authoritativeTransferStats selects a deterministic dashboard outcome from
// independently reported peer results. The receiver is authoritative once it
// reports because it confirms whether the destination file was accepted.
func authoritativeTransferStats(statsByRole map[string]transferStats) transferStats {
	if stats, ok := statsByRole["recv"]; ok {
		return stats
	}
	return statsByRole["send"]
}

// authenticate verifies that a capability owns one role without refreshing session state.
func (st *sessionStore) authenticate(ctx context.Context, code, role, capabilityHash string) error {
	if err := validateMailboxRole(role); err != nil {
		return err
	}
	sess, err := st.load(ctx, code)
	if err != nil {
		return authenticatedStoreError(err, capabilityHash)
	}
	_, err = st.authenticateSessionRole(sess, role, capabilityHash)
	return authenticatedStoreError(err, capabilityHash)
}

// authenticateSessionRole verifies that a session role owns the supplied capability verifier.
func (st *sessionStore) authenticateSessionRole(sess *rendezvousSession, role, capabilityHash string) (*sessionPeer, error) {
	peer := st.peerForRole(sess, role)
	expected := missingMailboxVerifier
	if peer != nil {
		expected = peer.CapabilityHash
	}
	if !mailboxCapabilityVerifierEqual(expected, capabilityHash) || peer == nil || peer.Role != role {
		return nil, errMailboxAuthentication
	}
	return peer, nil
}

// authenticatedStoreError hides session existence when authentication cannot be completed.
func authenticatedStoreError(err error, capabilityHash string) error {
	if errors.Is(err, errSessionNotFound) || errors.Is(err, errMailboxUnavailable) {
		compareMissingMailboxCapability(capabilityHash)
		return errMailboxAuthentication
	}
	return err
}

// load reads one opaque rendezvous session without refreshing its TTL.
func (st *sessionStore) load(ctx context.Context, code string) (*rendezvousSession, error) {
	key, err := st.key(code)
	if err != nil {
		return nil, err
	}
	pipe := st.client.Pipeline()
	dataCmd := pipe.Get(ctx, key)
	timeCmd := pipe.Time(ctx)
	_, err = pipe.Exec(ctx)
	data, dataErr := dataCmd.Bytes()
	if dataErr == redis.Nil {
		return nil, errSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	if dataErr != nil {
		return nil, dataErr
	}
	redisNow, err := timeCmd.Result()
	if err != nil {
		return nil, err
	}
	var sess rendezvousSession
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, st.removeSessionValue(ctx, key, code, data)
	}
	if sess.Code != code {
		return nil, st.removeSessionValue(ctx, key, code, data)
	}
	if _, ok := st.sessionAbsoluteExpiryMillis(&sess, redisNow); !ok {
		return nil, st.removeSessionValue(ctx, key, code, data)
	}
	if sess.Pending == nil {
		sess.Pending = make(map[string][]msgPt)
	}
	return &sess, nil
}

// removeSessionValue deletes stale session data only if it has not changed
// since the caller inspected it.
func (st *sessionStore) removeSessionValue(ctx context.Context, key, code string, expected []byte) error {
	if _, err := mailboxSessionDeleteIfValueScript.Run(
		ctx,
		st.client,
		[]string{key, st.activeSessionsKey()},
		expected,
		code,
	).Result(); err != nil {
		return err
	}
	return errSessionNotFound
}

// removeWatchedSession deletes an expired or corrupt session and its capacity entry.
func (st *sessionStore) removeWatchedSession(ctx context.Context, tx *redis.Tx, key, code string) error {
	_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, key)
		pipe.ZRem(ctx, st.activeSessionsKey(), code)
		return nil
	})
	if err != nil {
		return err
	}
	return errSessionNotFound
}

// modify atomically applies a mutation while preserving the session TTL policy.
func (st *sessionStore) modify(ctx context.Context, code string, mutate func(*rendezvousSession) error) (*rendezvousSession, error) {
	key, err := st.key(code)
	if err != nil {
		return nil, err
	}
	var result *rendezvousSession
	for {
		err := st.client.Watch(ctx, func(tx *redis.Tx) error {
			data, err := tx.Get(ctx, key).Bytes()
			if err == redis.Nil {
				return errSessionNotFound
			}
			if err != nil {
				return err
			}
			var sess rendezvousSession
			if err := json.Unmarshal(data, &sess); err != nil {
				return st.removeWatchedSession(ctx, tx, key, code)
			}
			redisNow, err := tx.Time(ctx).Result()
			if err != nil {
				return err
			}
			leaseExpiryMillis, ok := st.sessionRefreshExpiryMillis(&sess, redisNow)
			if sess.Code != code || !ok {
				return st.removeWatchedSession(ctx, tx, key, code)
			}
			if sess.Pending == nil {
				sess.Pending = make(map[string][]msgPt)
			}
			if err := mutate(&sess); err != nil {
				return err
			}
			payload, err := json.Marshal(&sess)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, payload, 0)
				pipe.PExpireAt(ctx, key, time.UnixMilli(leaseExpiryMillis))
				pipe.ZAdd(ctx, st.activeSessionsKey(), redis.Z{
					Score:  float64(leaseExpiryMillis),
					Member: code,
				})
				return nil
			})
			if err == redis.TxFailedErr {
				return err
			}
			result = &sess
			return err
		}, key)
		if err == redis.TxFailedErr {
			continue
		}
		if err != nil {
			return nil, err
		}
		return result, nil
	}
}
