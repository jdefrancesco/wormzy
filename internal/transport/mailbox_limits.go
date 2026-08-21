package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	defaultSenderClaimLimit          = 10
	defaultReceiverIPLimit           = 10
	defaultReceiverSessionLimit      = 5
	defaultGlobalClaimLimit          = 2000
	defaultAuthenticatedIPLimit      = 600
	defaultAuthenticatedGlobalLimit  = 20000
	defaultAuthenticatedRequestLimit = 120
	defaultMailboxLongPollLimit      = 256
	defaultMailboxLongPollTimeout    = 25 * time.Second
	defaultMailboxRateWindow         = time.Minute
	mailboxPollRetryDelay            = 100 * time.Millisecond
)

var (
	errMailboxPollAgain = errors.New("mailbox poll again")

	mailboxRateIncrementScript = redis.NewScript(`
local count = redis.call("INCR", KEYS[1])
if count == 1 then
  redis.call("PEXPIRE", KEYS[1], ARGV[1])
end
return count
`)
	processMailboxLongPollGate = newMailboxLongPollGate(defaultMailboxLongPollLimit)
)

// rateLimitDecision reports whether an operation fits in its current fixed window.
type rateLimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// redisFixedWindowLimiter stores bounded counters shared by all mailbox processes.
type redisFixedWindowLimiter struct {
	client *redis.Client
	prefix string
	now    func() time.Time
}

// mailboxLongPollGate limits concurrent polls and rejects duplicate capability waits.
type mailboxLongPollGate struct {
	mu     sync.Mutex
	max    int
	active map[string]struct{}
}

// mailboxAdmission combines distributed request limits with the local long-poll gate.
type mailboxAdmission struct {
	rates *redisFixedWindowLimiter
	polls *mailboxLongPollGate

	senderClaimLimit         int64
	receiverIPLimit          int64
	receiverSessionLimit     int64
	globalClaimLimit         int64
	authenticatedIPLimit     int64
	authenticatedGlobalLimit int64
	authenticatedLimit       int64
	window                   time.Duration
	pollTimeout              time.Duration
}

// newRedisFixedWindowLimiter creates a Redis-backed limiter under a dedicated key namespace.
func newRedisFixedWindowLimiter(client *redis.Client, prefix string) *redisFixedWindowLimiter {
	return &redisFixedWindowLimiter{
		client: client,
		prefix: prefix,
		now:    time.Now,
	}
}

// allow increments one hashed fixed-window counter and returns its admission decision.
func (l *redisFixedWindowLimiter) allow(
	ctx context.Context,
	scope string,
	identity string,
	limit int64,
	window time.Duration,
) (rateLimitDecision, error) {
	if l == nil || l.client == nil {
		return rateLimitDecision{}, errors.New("mailbox rate limiter is unavailable")
	}
	if scope == "" || identity == "" || limit <= 0 || window <= 0 {
		return rateLimitDecision{}, errors.New("invalid mailbox rate-limit configuration")
	}
	now := l.now()
	bucket := now.UnixNano() / int64(window)
	windowEnd := time.Unix(0, (bucket+1)*int64(window))
	retryAfter := windowEnd.Sub(now)
	if retryAfter <= 0 {
		retryAfter = time.Millisecond
	}
	expiryMillis := (retryAfter + time.Millisecond - 1) / time.Millisecond
	key := l.key(scope, identity, bucket)
	count, err := mailboxRateIncrementScript.Run(
		ctx,
		l.client,
		[]string{key},
		strconv.FormatInt(int64(expiryMillis), 10),
	).Int64()
	if err != nil {
		return rateLimitDecision{}, fmt.Errorf("increment mailbox rate limit: %w", err)
	}
	if count <= limit {
		return rateLimitDecision{Allowed: true}, nil
	}
	return rateLimitDecision{RetryAfter: retryAfter}, nil
}

// key creates a bounded Redis key without storing the raw rate-limit identity.
func (l *redisFixedWindowLimiter) key(scope, identity string, bucket int64) string {
	digest := sha256.Sum256([]byte(identity))
	return strings.Join([]string{
		l.prefix,
		"limits",
		scope,
		strconv.FormatInt(bucket, 10),
		hex.EncodeToString(digest[:]),
	}, ":")
}

// newMailboxLongPollGate creates a concurrency gate with one slot per capability.
func newMailboxLongPollGate(maximum int) *mailboxLongPollGate {
	if maximum < 1 {
		maximum = 1
	}
	return &mailboxLongPollGate{
		max:    maximum,
		active: make(map[string]struct{}, maximum),
	}
}

// acquire reserves a poll slot and returns an idempotent release function.
func (g *mailboxLongPollGate) acquire(capabilityHash string) (func(), bool) {
	if g == nil || capabilityHash == "" {
		return nil, false
	}
	g.mu.Lock()
	if _, duplicate := g.active[capabilityHash]; duplicate || len(g.active) >= g.max {
		g.mu.Unlock()
		return nil, false
	}
	g.active[capabilityHash] = struct{}{}
	g.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.active, capabilityHash)
			g.mu.Unlock()
		})
	}
	return release, true
}

// newMailboxAdmission creates the default public-mailbox admission policy.
func newMailboxAdmission(client *redis.Client, prefix string) *mailboxAdmission {
	return &mailboxAdmission{
		rates:                    newRedisFixedWindowLimiter(client, prefix),
		polls:                    processMailboxLongPollGate,
		senderClaimLimit:         defaultSenderClaimLimit,
		receiverIPLimit:          defaultReceiverIPLimit,
		receiverSessionLimit:     defaultReceiverSessionLimit,
		globalClaimLimit:         defaultGlobalClaimLimit,
		authenticatedIPLimit:     defaultAuthenticatedIPLimit,
		authenticatedGlobalLimit: defaultAuthenticatedGlobalLimit,
		authenticatedLimit:       defaultAuthenticatedRequestLimit,
		window:                   defaultMailboxRateWindow,
		pollTimeout:              defaultMailboxLongPollTimeout,
	}
}

// allowClaim applies the role-specific claim limits for an IP and opaque session ID.
func (a *mailboxAdmission) allowClaim(
	ctx context.Context,
	role string,
	clientIP string,
	sessionID string,
) (rateLimitDecision, error) {
	if a == nil {
		return rateLimitDecision{Allowed: true}, nil
	}
	switch role {
	case "send":
		decision, err := a.rates.allow(ctx, "claim-send-ip", clientIP, a.senderClaimLimit, a.window)
		if err != nil || !decision.Allowed {
			return decision, err
		}
	case "recv":
		decision, err := a.rates.allow(ctx, "claim-recv-ip", clientIP, a.receiverIPLimit, a.window)
		if err != nil || !decision.Allowed {
			return decision, err
		}
		decision, err = a.rates.allow(ctx, "claim-recv-session", sessionID, a.receiverSessionLimit, a.window)
		if err != nil || !decision.Allowed {
			return decision, err
		}
	default:
		return rateLimitDecision{}, errInvalidRole
	}
	return a.rates.allow(ctx, "claim-global", "global", a.globalClaimLimit, a.window)
}

// allowAuthenticatedAttempt bounds pre-authentication traffic by client IP and globally.
func (a *mailboxAdmission) allowAuthenticatedAttempt(ctx context.Context, clientIP string) (rateLimitDecision, error) {
	if a == nil {
		return rateLimitDecision{Allowed: true}, nil
	}
	decision, err := a.rates.allow(ctx, "authenticated-ip", clientIP, a.authenticatedIPLimit, a.window)
	if err != nil || !decision.Allowed {
		return decision, err
	}
	return a.rates.allow(
		ctx,
		"authenticated-global",
		"global",
		a.authenticatedGlobalLimit,
		a.window,
	)
}

// allowAuthenticated applies the secondary per-capability limit after session authentication.
func (a *mailboxAdmission) allowAuthenticated(ctx context.Context, capabilityHash string) (rateLimitDecision, error) {
	if a == nil {
		return rateLimitDecision{Allowed: true}, nil
	}
	return a.rates.allow(ctx, "authenticated-capability", capabilityHash, a.authenticatedLimit, a.window)
}

// mailboxClientIP returns a canonical direct client address for rate limiting.
func mailboxClientIP(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	direct, ok := parseMailboxRemoteIP(r.RemoteAddr)
	if !ok {
		return "unknown"
	}
	if direct.IsLoopback() {
		forwardedHeaders := r.Header.Values("X-Forwarded-For")
		if len(forwardedHeaders) > 0 {
			forwarded := strings.Split(forwardedHeaders[len(forwardedHeaders)-1], ",")
			if proxyClient, err := netip.ParseAddr(strings.TrimSpace(forwarded[len(forwarded)-1])); err == nil {
				return proxyClient.Unmap().String()
			}
		}
	}
	return direct.String()
}

// parseMailboxRemoteIP extracts and canonicalizes an IP from an HTTP RemoteAddr.
func parseMailboxRemoteIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

// enforceMailboxClaimLimit applies claim admission and writes a generic failure response.
func (s *MailboxHTTPServer) enforceMailboxClaimLimit(w http.ResponseWriter, r *http.Request, role, sessionID string) bool {
	if s == nil || s.admission == nil {
		return true
	}
	decision, err := s.admission.allowClaim(r.Context(), role, mailboxClientIP(r), sessionID)
	if err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, errMailboxUnavailable)
		return false
	}
	if !decision.Allowed {
		writeMailboxRateLimit(w, decision.RetryAfter)
		return false
	}
	return true
}

// authorizeMailboxOperation applies pre-authentication limits and parses one Bearer capability.
func (s *MailboxHTTPServer) authorizeMailboxOperation(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s != nil && s.admission != nil {
		decision, err := s.admission.allowAuthenticatedAttempt(r.Context(), mailboxClientIP(r))
		if err != nil {
			writeHTTPError(w, http.StatusServiceUnavailable, errMailboxUnavailable)
			return "", false
		}
		if !decision.Allowed {
			writeMailboxRateLimit(w, decision.RetryAfter)
			return "", false
		}
	}
	capabilityHash, err := authorizeMailboxRequest(r)
	if err != nil {
		writeHTTPError(w, http.StatusUnauthorized, errMailboxAuthentication)
		return "", false
	}
	return capabilityHash, true
}

// authorizeMailboxIdentity verifies the session role before creating a capability limiter key.
func (s *MailboxHTTPServer) authorizeMailboxIdentity(
	w http.ResponseWriter,
	r *http.Request,
	role string,
	sessionID string,
	capabilityHash string,
) bool {
	if s == nil || s.store == nil {
		writeHTTPError(w, http.StatusServiceUnavailable, errMailboxUnavailable)
		return false
	}
	if err := s.store.authenticate(r.Context(), sessionID, role, capabilityHash); err != nil {
		writeMailboxOperationError(w, err)
		return false
	}
	if s.admission == nil {
		return true
	}
	decision, err := s.admission.allowAuthenticated(r.Context(), capabilityHash)
	if err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, errMailboxUnavailable)
		return false
	}
	if !decision.Allowed {
		writeMailboxRateLimit(w, decision.RetryAfter)
		return false
	}
	return true
}

// beginMailboxLongPoll reserves one local long-poll slot for a capability.
func (s *MailboxHTTPServer) beginMailboxLongPoll(w http.ResponseWriter, capabilityHash string) (func(), bool) {
	if s == nil || s.admission == nil || s.admission.polls == nil {
		return func() {}, true
	}
	release, ok := s.admission.polls.acquire(capabilityHash)
	if !ok {
		writeMailboxRateLimit(w, time.Second)
		return nil, false
	}
	return release, true
}

// mailboxLongPollContext bounds one server poll independently of the caller deadline.
func (s *MailboxHTTPServer) mailboxLongPollContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := defaultMailboxLongPollTimeout
	if s != nil && s.admission != nil && s.admission.pollTimeout > 0 {
		timeout = s.admission.pollTimeout
	}
	return context.WithTimeout(parent, timeout)
}

// writeMailboxLongPollError converts an internal poll boundary into a client retry response.
func writeMailboxLongPollError(w http.ResponseWriter, r *http.Request, err error) bool {
	if r.Context().Err() != nil {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// writeMailboxRateLimit emits a generic throttling response with a bounded retry delay.
func writeMailboxRateLimit(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writeHTTPError(w, http.StatusTooManyRequests, errMailboxUnavailable)
}

// validateMailboxSelfInfo bounds candidate and feature metadata before Redis persistence.
func validateMailboxSelfInfo(info rendezvous.SelfInfo) error {
	if err := validatePeerCandidateMetadata(info); err != nil {
		return err
	}
	if len(info.Features) > maxSelfFeatureCount {
		return fmt.Errorf("peer feature count exceeds limit of %d", maxSelfFeatureCount)
	}
	for _, feature := range info.Features {
		if len(feature) > maxSelfFeatureLength || !safeCandidateText(feature) {
			return errors.New("peer feature contains invalid text")
		}
	}
	return nil
}
