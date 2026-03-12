package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/internal/rendezvous"
)

var (
	errSessionNotFound  = errors.New("session not found")
	errSenderInUse      = errors.New("sender already registered for pairing code")
	errReceiverInUse    = errors.New("receiver already registered for pairing code")
	errSenderMissing    = errors.New("sender not registered yet")
	errReceiverMissing  = errors.New("receiver not registered yet")
	errInvalidRole      = errors.New("invalid role")
	errNoPendingMessage = errors.New("no pending mailbox message")
)

type sessionStore struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
}

type rendezvousSession struct {
	Code        string             `json:"code"`
	CreatedUnix int64              `json:"created_unix"`
	TTLSeconds  int64              `json:"ttl_seconds"`
	Sender      *sessionPeer       `json:"sender,omitempty"`
	Receiver    *sessionPeer       `json:"receiver,omitempty"`
	Pending     map[string][]msgPt `json:"pending,omitempty"`
	NextSideID  uint32             `json:"next_side_id"`
	Alias       string             `json:"alias,omitempty"`
	Stats       *transferStats     `json:"stats,omitempty"`
}

type sessionPeer struct {
	ID           uint32               `json:"id"`
	Role         string               `json:"role"`
	Info         *rendezvous.SelfInfo `json:"info,omitempty"`
	RegisteredAt int64                `json:"registered_unix"`
	LastUpdate   int64                `json:"last_update_unix"`
}

type msgPt struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

type transferStats struct {
	Mode        string `json:"mode,omitempty"`
	Transport   string `json:"transport,omitempty"`
	Candidate   string `json:"candidate,omitempty"`
	Completed   bool   `json:"completed"`
	Error       string `json:"error,omitempty"`
	UpdatedUnix int64  `json:"updated_unix"`
}

func newSessionStore(client *redis.Client, ttl time.Duration, prefix string) *sessionStore {
	return &sessionStore{client: client, prefix: prefix, ttl: ttl}
}

func newSession(code string, ttl time.Duration) *rendezvousSession {
	return &rendezvousSession{
		Code:        code,
		CreatedUnix: time.Now().Unix(),
		TTLSeconds:  int64(ttl / time.Second),
		Pending:     make(map[string][]msgPt),
		NextSideID:  1,
	}
}

func (st *sessionStore) key(code string) string {
	return fmt.Sprintf("%s:sessions:%s", st.prefix, code)
}

func (st *sessionStore) registerSender(ctx context.Context, code string) (*rendezvousSession, error) {
	return st.modify(ctx, code, true, func(sess *rendezvousSession) error {
		if sess.Sender != nil {
			return errSenderInUse
		}
		sess.Sender = &sessionPeer{
			ID:           sess.NextSideID,
			Role:         "send",
			RegisteredAt: time.Now().Unix(),
			LastUpdate:   time.Now().Unix(),
		}
		sess.NextSideID++
		return nil
	})
}

func (st *sessionStore) registerReceiver(ctx context.Context, code string) (*rendezvousSession, error) {
	return st.modify(ctx, code, false, func(sess *rendezvousSession) error {
		if sess == nil {
			return errSenderMissing
		}
		if sess.Receiver != nil {
			return errReceiverInUse
		}
		if sess.Sender == nil {
			return errSenderMissing
		}
		sess.Receiver = &sessionPeer{
			ID:           sess.NextSideID,
			Role:         "recv",
			RegisteredAt: time.Now().Unix(),
			LastUpdate:   time.Now().Unix(),
		}
		sess.NextSideID++
		return nil
	})
}

func (st *sessionStore) updatePeerInfo(ctx context.Context, code, role string, info rendezvous.SelfInfo) error {
	_, err := st.modify(ctx, code, false, func(sess *rendezvousSession) error {
		peer := st.peerForRole(sess, role)
		if peer == nil {
			if role == "send" {
				return errSenderMissing
			}
			return errReceiverMissing
		}
		cpy := info
		peer.Info = &cpy
		peer.LastUpdate = time.Now().Unix()
		return nil
	})
	return err
}

func (st *sessionStore) peerForRole(sess *rendezvousSession, role string) *sessionPeer {
	switch role {
	case "send":
		return sess.Sender
	case "recv":
		return sess.Receiver
	default:
		return nil
	}
}

func (st *sessionStore) waitForPeer(ctx context.Context, code, role string) (*rendezvous.SelfInfo, error) {
	peerRole := oppositeRole(role)
	for {
		sess, err := st.load(ctx, code)
		if err != nil {
			if errors.Is(err, errSessionNotFound) {
				return nil, err
			}
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

func oppositeRole(role string) string {
	if role == "send" {
		return "recv"
	}
	return "send"
}

func (st *sessionStore) enqueue(ctx context.Context, code, destRole string, msg mailboxMessage) error {
	_, err := st.modify(ctx, code, false, func(sess *rendezvousSession) error {
		queueRole := destRole
		if queueRole != "send" && queueRole != "recv" {
			return errInvalidRole
		}
		if sess.Pending == nil {
			sess.Pending = make(map[string][]msgPt)
		}
		raw := msgPt{Type: msg.Type, Body: msg.Body}
		sess.Pending[queueRole] = append(sess.Pending[queueRole], raw)
		return nil
	})
	return err
}

func (st *sessionStore) dequeue(ctx context.Context, code, role string) (mailboxMessage, error) {
	for {
		var out mailboxMessage
		var ok bool
		_, err := st.modify(ctx, code, false, func(sess *rendezvousSession) error {
			if sess == nil {
				return errSessionNotFound
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
			return mailboxMessage{}, err
		}
		if ok {
			return out, nil
		}
	}
}

func (st *sessionStore) delete(ctx context.Context, code string) error {
	return st.client.Del(ctx, st.key(code)).Err()
}

func (st *sessionStore) recordStats(ctx context.Context, code string, stats transferStats) error {
	stats.UpdatedUnix = time.Now().Unix()
	_, err := st.modify(ctx, code, false, func(sess *rendezvousSession) error {
		if sess == nil {
			return errSessionNotFound
		}
		sess.Stats = &stats
		return nil
	})
	return err
}

func (st *sessionStore) load(ctx context.Context, code string) (*rendezvousSession, error) {
	data, err := st.client.Get(ctx, st.key(code)).Bytes()
	if err == redis.Nil {
		return nil, errSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	var sess rendezvousSession
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	if sess.Pending == nil {
		sess.Pending = make(map[string][]msgPt)
	}
	return &sess, nil
}

func (st *sessionStore) modify(ctx context.Context, code string, create bool, mutate func(*rendezvousSession) error) (*rendezvousSession, error) {
	key := st.key(code)
	var result *rendezvousSession
	for {
		err := st.client.Watch(ctx, func(tx *redis.Tx) error {
			var sess *rendezvousSession
			data, err := tx.Get(ctx, key).Bytes()
			if err == redis.Nil {
				if !create {
					return errSessionNotFound
				}
				sess = newSession(code, st.ttl)
			} else if err != nil {
				return err
			} else {
				if err := json.Unmarshal(data, &sess); err != nil {
					return err
				}
			}
			if sess.Pending == nil {
				sess.Pending = make(map[string][]msgPt)
			}
			if err := mutate(sess); err != nil {
				return err
			}
			payload, err := json.Marshal(sess)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, payload, st.ttl)
				return nil
			})
			if err == redis.TxFailedErr {
				return err
			}
			result = sess
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
