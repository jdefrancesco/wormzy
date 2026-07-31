package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
	"github.com/redis/go-redis/v9"
)

type MailboxHTTPServer struct {
	client    *redis.Client
	ttl       time.Duration
	store     *sessionStore
	telemetry *ServiceTelemetry
}

var errServiceDraining = errors.New("wormzy is draining; new sessions are temporarily disabled")

func NewMailboxHTTPServer(redisURL string, ttl time.Duration) (*MailboxHTTPServer, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		opts = &redis.Options{Addr: redisURL}
	}
	client := redis.NewClient(opts)
	server := &MailboxHTTPServer{
		client: client,
		ttl:    ttl,
		store:  newSessionStore(client, ttl, "wormzy"),
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
	switch r.URL.Path {
	case "/v1/claim":
		s.handleClaim(w, r)
	case "/v1/self":
		s.handleStoreSelf(w, r)
	case "/v1/wait-peer":
		s.handleWaitPeer(w, r)
	case "/v1/send":
		s.handleSend(w, r)
	case "/v1/receive":
		s.handleReceive(w, r)
	case "/v1/stats":
		s.handleStats(w, r)
	case "/healthz":
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

func (s *MailboxHTTPServer) newMailbox(role, code string) *redisMailbox {
	return &redisMailbox{
		client: s.client,
		ttl:    s.ttl,
		prefix: "wormzy",
		role:   role,
		code:   code,
		store:  s.store,
	}
}

func (s *MailboxHTTPServer) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role      string `json:"role"`
		Requested string `json:"requested"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	mb := s.newMailbox(req.Role, "")
	code, err := mb.Claim(r.Context(), req.Requested)
	if err != nil {
		if errors.Is(err, errServiceDraining) {
			writeHTTPError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	writeHTTPJSON(w, map[string]string{"code": code})
}

func (s *MailboxHTTPServer) handleStoreSelf(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string              `json:"role"`
		Code string              `json:"code"`
		Info rendezvous.SelfInfo `json:"info"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	mb := s.newMailbox(req.Role, req.Code)
	if err := mb.StoreSelf(r.Context(), req.Info); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	writeHTTPJSON(w, map[string]string{"status": "ok"})
}

func (s *MailboxHTTPServer) handleWaitPeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string `json:"role"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	mb := s.newMailbox(req.Role, req.Code)
	info, err := mb.WaitPeer(r.Context())
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	writeHTTPJSON(w, map[string]any{"info": info})
}

func (s *MailboxHTTPServer) handleSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string `json:"role"`
		Code string `json:"code"`
		Type string `json:"type"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Body)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	var raw json.RawMessage = json.RawMessage(data)
	mb := s.newMailbox(req.Role, req.Code)
	if err := mb.Send(r.Context(), req.Type, raw); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	writeHTTPJSON(w, map[string]string{"status": "ok"})
}

func (s *MailboxHTTPServer) handleReceive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string `json:"role"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	mb := s.newMailbox(req.Role, req.Code)
	msg, err := mb.Receive(r.Context())
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	mb := s.newMailbox(req.Role, req.Code)
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
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	writeHTTPJSON(w, map[string]string{"status": "ok"})
}

func writeHTTPError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeHTTPJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *MailboxHTTPServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.client.Ping(r.Context()).Err(); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeHTTPJSON(w, map[string]string{"status": "ok"})
}
