package transport

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jdefrancesco/internal/rendezvous"
	"github.com/redis/go-redis/v9"
)

type MailboxHTTPServer struct {
	client *redis.Client
	ttl    time.Duration
}

func NewMailboxHTTPServer(redisURL string, ttl time.Duration) (*MailboxHTTPServer, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		opts = &redis.Options{Addr: redisURL}
	}
	client := redis.NewClient(opts)
	return &MailboxHTTPServer{
		client: client,
		ttl:    ttl,
	}, nil
}

func (s *MailboxHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	default:
		http.NotFound(w, r)
	}
}

func (s *MailboxHTTPServer) newMailbox(role, code string) *redisMailbox {
	return &redisMailbox{
		client: s.client,
		ttl:    s.ttl,
		prefix: "wormzy",
		role:   role,
		code:   code,
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

func writeHTTPError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeHTTPJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
