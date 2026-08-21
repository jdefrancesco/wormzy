package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
	"github.com/redis/go-redis/v9"
)

type MailboxHTTPServer struct {
	client    *redis.Client
	ttl       time.Duration
	store     *sessionStore
	admission *mailboxAdmission
	telemetry *ServiceTelemetry
}

var errServiceDraining = errors.New("wormzy is draining; new sessions are temporarily disabled")

// NewMailboxHTTPServer creates the bounded public mailbox API backed by Redis.
func NewMailboxHTTPServer(redisURL string, ttl time.Duration) (*MailboxHTTPServer, error) {
	if ttl <= 0 {
		return nil, errors.New("mailbox session TTL must be positive")
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		opts = &redis.Options{Addr: redisURL}
	}
	client := redis.NewClient(opts)
	server := &MailboxHTTPServer{
		client:    client,
		ttl:       ttl,
		store:     newSessionStore(client, ttl, mailboxV2StorePrefix),
		admission: newMailboxAdmission(client, mailboxV2StorePrefix),
	}
	server.telemetry = newServiceTelemetryWithClient(client, "wormzy", "mailbox")
	return server, nil
}

func (s *MailboxHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	statusWriter := &responseStatusWriter{ResponseWriter: w, status: http.StatusOK}
	if s.telemetry != nil {
		s.telemetry.BeginRequest()
		defer func() { s.telemetry.EndRequest(statusWriter.status) }()
	}
	w = statusWriter
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxHTTPMailboxPayloadSize)
	}
	switch r.URL.Path {
	case mailboxHTTPClaimPath:
		if !requireMailboxJSONPost(w, r) {
			return
		}
		s.handleClaim(w, r)
	case mailboxHTTPSelfPath:
		if !requireMailboxJSONPost(w, r) {
			return
		}
		s.handleStoreSelf(w, r)
	case mailboxHTTPWaitPeerPath:
		if !requireMailboxJSONPost(w, r) {
			return
		}
		s.handleWaitPeer(w, r)
	case mailboxHTTPSendPath:
		if !requireMailboxJSONPost(w, r) {
			return
		}
		s.handleSend(w, r)
	case mailboxHTTPReceivePath:
		if !requireMailboxJSONPost(w, r) {
			return
		}
		s.handleReceive(w, r)
	case mailboxHTTPStatsPath:
		if !requireMailboxJSONPost(w, r) {
			return
		}
		s.handleStats(w, r)
	case "/healthz":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeHTTPError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		s.handleHealthz(w, r)
	default:
		http.NotFound(w, r)
	}
}

type responseStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseStatusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// SyncOperations refreshes drain control and publishes a mailbox heartbeat.
func (s *MailboxHTTPServer) SyncOperations(ctx context.Context) error {
	if s == nil || s.telemetry == nil {
		return nil
	}
	return s.telemetry.Sync(ctx)
}

// RunOperations keeps the mailbox visible to the operator dashboard.
func (s *MailboxHTTPServer) RunOperations(ctx context.Context, interval time.Duration) {
	if s == nil || s.telemetry == nil {
		return
	}
	s.telemetry.Run(ctx, interval)
}

// Close releases the mailbox Redis connection.
func (s *MailboxHTTPServer) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

// newMailbox binds one authenticated role to the server's shared session store.
func (s *MailboxHTTPServer) newMailbox(role, code, capabilityHash string) *redisMailbox {
	return &redisMailbox{
		client:         s.client,
		ttl:            s.ttl,
		prefix:         "wormzy",
		role:           role,
		code:           code,
		capabilityHash: capabilityHash,
		store:          s.store,
	}
}

func (s *MailboxHTTPServer) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role           string `json:"role"`
		Requested      string `json:"requested"`
		CapabilityHash string `json:"capability_hash"`
	}
	if err := decodeStrictBoundedHTTPJSON(r.Body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateMailboxCapabilityVerifier(req.CapabilityHash); err != nil {
		writeHTTPError(w, http.StatusBadRequest, errMailboxUnavailable)
		return
	}
	if validateMailboxRole(req.Role) != nil || !validMailboxSessionID(req.Requested) {
		writeHTTPError(w, http.StatusBadRequest, errMailboxUnavailable)
		return
	}
	if !s.enforceMailboxClaimLimit(w, r, req.Role, req.Requested) {
		return
	}
	claimCtx := r.Context()
	if req.Role == "recv" {
		release, ok := s.beginMailboxLongPoll(w, req.CapabilityHash)
		if !ok {
			return
		}
		defer release()
		var cancel context.CancelFunc
		claimCtx, cancel = s.mailboxLongPollContext(r.Context())
		defer cancel()
	}
	mb := s.newMailbox(req.Role, "", req.CapabilityHash)
	code, err := mb.Claim(claimCtx, req.Requested)
	if err != nil {
		if req.Role == "recv" && writeMailboxLongPollError(w, r, err) {
			return
		}
		if errors.Is(err, errServiceDraining) {
			writeHTTPError(w, http.StatusServiceUnavailable, err)
			return
		}
		if errors.Is(err, errMailboxCapacity) {
			writeHTTPError(w, http.StatusServiceUnavailable, errMailboxUnavailable)
			return
		}
		writeHTTPError(w, http.StatusBadRequest, errMailboxUnavailable)
		return
	}
	writeHTTPJSON(w, map[string]string{"code": code})
}

func (s *MailboxHTTPServer) handleStoreSelf(w http.ResponseWriter, r *http.Request) {
	capabilityHash, ok := s.authorizeMailboxOperation(w, r)
	if !ok {
		return
	}
	var req struct {
		Role string              `json:"role"`
		Code string              `json:"code"`
		Info rendezvous.SelfInfo `json:"info"`
	}
	if err := decodeStrictBoundedHTTPJSON(r.Body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateMailboxSelfInfo(req.Info); err != nil {
		writeHTTPError(w, http.StatusBadRequest, errMailboxUnavailable)
		return
	}
	if !s.authorizeMailboxIdentity(w, r, req.Role, req.Code, capabilityHash) {
		return
	}
	mb := s.newMailbox(req.Role, req.Code, capabilityHash)
	if err := mb.StoreSelf(r.Context(), req.Info); err != nil {
		writeMailboxOperationError(w, err)
		return
	}
	writeHTTPJSON(w, map[string]string{"status": "ok"})
}

func (s *MailboxHTTPServer) handleWaitPeer(w http.ResponseWriter, r *http.Request) {
	capabilityHash, ok := s.authorizeMailboxOperation(w, r)
	if !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
		Code string `json:"code"`
	}
	if err := decodeStrictBoundedHTTPJSON(r.Body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	if !s.authorizeMailboxIdentity(w, r, req.Role, req.Code, capabilityHash) {
		return
	}
	release, ok := s.beginMailboxLongPoll(w, capabilityHash)
	if !ok {
		return
	}
	defer release()
	pollCtx, cancel := s.mailboxLongPollContext(r.Context())
	defer cancel()
	mb := s.newMailbox(req.Role, req.Code, capabilityHash)
	info, err := mb.WaitPeer(pollCtx)
	if err != nil {
		if writeMailboxLongPollError(w, r, err) {
			return
		}
		writeMailboxOperationError(w, err)
		return
	}
	writeHTTPJSON(w, map[string]any{"info": info})
}

func (s *MailboxHTTPServer) handleSend(w http.ResponseWriter, r *http.Request) {
	capabilityHash, ok := s.authorizeMailboxOperation(w, r)
	if !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
		Code string `json:"code"`
		Type string `json:"type"`
		Body string `json:"body"`
	}
	if err := decodeStrictBoundedHTTPJSON(r.Body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Body)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	var raw json.RawMessage = json.RawMessage(data)
	if err := validateMailboxMessage(mailboxMessage{Type: req.Type, Body: raw}); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	if !json.Valid(raw) {
		writeHTTPError(w, http.StatusBadRequest, errors.New("mailbox message body is invalid JSON"))
		return
	}
	if !s.authorizeMailboxIdentity(w, r, req.Role, req.Code, capabilityHash) {
		return
	}
	mb := s.newMailbox(req.Role, req.Code, capabilityHash)
	if err := mb.Send(r.Context(), req.Type, raw); err != nil {
		writeMailboxOperationError(w, err)
		return
	}
	writeHTTPJSON(w, map[string]string{"status": "ok"})
}

func (s *MailboxHTTPServer) handleReceive(w http.ResponseWriter, r *http.Request) {
	capabilityHash, ok := s.authorizeMailboxOperation(w, r)
	if !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
		Code string `json:"code"`
	}
	if err := decodeStrictBoundedHTTPJSON(r.Body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	if !s.authorizeMailboxIdentity(w, r, req.Role, req.Code, capabilityHash) {
		return
	}
	release, ok := s.beginMailboxLongPoll(w, capabilityHash)
	if !ok {
		return
	}
	defer release()
	pollCtx, cancel := s.mailboxLongPollContext(r.Context())
	defer cancel()
	mb := s.newMailbox(req.Role, req.Code, capabilityHash)
	msg, err := mb.Receive(pollCtx)
	if err != nil {
		if writeMailboxLongPollError(w, r, err) {
			return
		}
		writeMailboxOperationError(w, err)
		return
	}
	var body string
	if len(msg.Body) > 0 {
		body = base64.StdEncoding.EncodeToString(msg.Body)
	}
	writeHTTPJSON(w, map[string]string{
		"type": msg.Type,
		"body": body,
	})
}

func (s *MailboxHTTPServer) handleStats(w http.ResponseWriter, r *http.Request) {
	capabilityHash, ok := s.authorizeMailboxOperation(w, r)
	if !ok {
		return
	}
	var req struct {
		Role           string `json:"role"`
		Code           string `json:"code"`
		Mode           string `json:"mode"`
		Transport      string `json:"transport"`
		Candidate      string `json:"candidate"`
		DirectOutcome  string `json:"direct_outcome,omitempty"`
		DirectSummary  string `json:"direct_summary,omitempty"`
		Bytes          int64  `json:"bytes,omitempty"`
		DurationMillis int64  `json:"duration_ms,omitempty"`
		Completed      bool   `json:"completed"`
		Error          string `json:"error,omitempty"`
	}
	if err := decodeStrictBoundedHTTPJSON(r.Body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	if !s.authorizeMailboxIdentity(w, r, req.Role, req.Code, capabilityHash) {
		return
	}
	mb := s.newMailbox(req.Role, req.Code, capabilityHash)
	stats := transferStats{
		Mode:           req.Mode,
		Transport:      req.Transport,
		Candidate:      req.Candidate,
		DirectOutcome:  req.DirectOutcome,
		DirectSummary:  req.DirectSummary,
		Bytes:          req.Bytes,
		DurationMillis: req.DurationMillis,
		Completed:      req.Completed,
		Error:          req.Error,
	}
	if err := mb.ReportStats(r.Context(), stats); err != nil {
		writeMailboxOperationError(w, err)
		return
	}
	writeHTTPJSON(w, map[string]string{"status": "ok"})
}

func writeHTTPError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeHTTPJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// authorizeMailboxRequest derives a verifier from a strict Bearer capability.
func authorizeMailboxRequest(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", errMailboxAuthentication
	}
	return mailboxAuthorizationVerifier(values[0])
}

// requireMailboxJSONPost enforces the method and media type for mailbox JSON endpoints.
func requireMailboxJSONPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeHTTPError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeHTTPError(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return false
	}
	return true
}

// writeMailboxOperationError returns only generic authentication or availability failures.
func writeMailboxOperationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errMailboxAuthentication),
		errors.Is(err, errSessionNotFound),
		errors.Is(err, errSenderMissing),
		errors.Is(err, errReceiverMissing),
		errors.Is(err, errInvalidRole):
		writeHTTPError(w, http.StatusUnauthorized, errMailboxAuthentication)
	default:
		writeHTTPError(w, http.StatusServiceUnavailable, errMailboxUnavailable)
	}
}

// decodeStrictBoundedHTTPJSON decodes one JSON value within the mailbox payload limit.
func decodeStrictBoundedHTTPJSON(body io.Reader, dst any) error {
	limited := &io.LimitedReader{R: body, N: maxHTTPMailboxPayloadSize + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		if limited.N == 0 {
			return fmt.Errorf("mailbox HTTP JSON exceeds %d bytes", maxHTTPMailboxPayloadSize)
		}
		return err
	}
	trailingErr := decoder.Decode(&struct{}{})
	if limited.N == 0 {
		return fmt.Errorf("mailbox HTTP JSON exceeds %d bytes", maxHTTPMailboxPayloadSize)
	}
	if !errors.Is(trailingErr, io.EOF) {
		if trailingErr == nil {
			return errors.New("mailbox HTTP JSON contains trailing data")
		}
		return fmt.Errorf("mailbox HTTP JSON contains trailing data: %w", trailingErr)
	}
	return nil
}

func (s *MailboxHTTPServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.client.Ping(r.Context()).Err(); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, errors.New("mailbox storage unavailable"))
		return
	}
	writeHTTPJSON(w, map[string]string{"status": "ok"})
}
