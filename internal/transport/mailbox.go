package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/internal/rendezvous"
)

type mailboxMessage struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

type redisMailbox struct {
	client *redis.Client
	code   string
	ttl    time.Duration
	prefix string
	role   string
	stop   func()
}

func newRedisMailbox(ctx context.Context, addr string, ttl time.Duration, role string) (*redisMailbox, error) {
	opts, err := redis.ParseURL(addr)
	if err != nil {
		opts = &redis.Options{Addr: addr}
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		useEmbedded := addr == "" || addr == defaultRelay || errors.Is(err, context.DeadlineExceeded)
		if !useEmbedded {
			return nil, fmt.Errorf("redis connection failed: %w", err)
		}
		mini, mErr := miniredis.Run()
		if mErr != nil {
			return nil, fmt.Errorf("redis connection failed: %v (fallback start error: %w)", err, mErr)
		}
		_ = client.Close()
		opts = &redis.Options{Addr: mini.Addr()}
		client = redis.NewClient(opts)
		if pingErr := client.Ping(ctx).Err(); pingErr != nil {
			mini.Close()
			return nil, fmt.Errorf("embedded redis unavailable: %w", pingErr)
		}
		return &redisMailbox{
			client: client,
			ttl:    ttl,
			prefix: "wormzy",
			role:   role,
			stop: func() {
				mini.Close()
			},
		}, nil
	}
	return &redisMailbox{
		client: client,
		ttl:    ttl,
		prefix: "wormzy",
		role:   role,
	}, nil
}

func (m *redisMailbox) Close() error {
	if m == nil || m.client == nil {
		return nil
	}
	if m.stop != nil {
		m.stop()
	}
	return m.client.Close()
}

func (m *redisMailbox) Claim(ctx context.Context, requested string) (string, error) {
	if m.role == "send" {
		if requested == "" {
			requested = rendezvous.GenerateCode()
		}
		if err := m.claimSender(ctx, requested); err != nil {
			return "", err
		}
	} else {
		if requested == "" {
			return "", fmt.Errorf("receiver requires a pairing code")
		}
		if err := m.claimReceiver(ctx, requested); err != nil {
			return "", err
		}
	}
	m.code = requested
	return requested, nil
}

func (m *redisMailbox) StoreSelf(ctx context.Context, info rendezvous.SelfInfo) error {
	if m.code == "" {
		return fmt.Errorf("mailbox code not set")
	}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	key := m.key("self", m.role)
	return m.client.Set(ctx, key, data, m.ttl).Err()
}

func (m *redisMailbox) WaitPeer(ctx context.Context) (*rendezvous.SelfInfo, error) {
	if m.code == "" {
		return nil, fmt.Errorf("mailbox code not set")
	}
	var peerRole string
	if m.role == "send" {
		peerRole = "recv"
	} else {
		peerRole = "send"
	}
	key := m.key("self", peerRole)
	for {
		data, err := m.client.Get(ctx, key).Bytes()
		if err == nil {
			var info rendezvous.SelfInfo
			if err := json.Unmarshal(data, &info); err != nil {
				return nil, err
			}
			return &info, nil
		}
		if err != redis.Nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (m *redisMailbox) Send(ctx context.Context, typ string, body any) error {
	if m.code == "" {
		return fmt.Errorf("mailbox code not set")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	env := mailboxMessage{Type: typ, Body: raw}
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	key := m.outboundKey()
	return m.client.RPush(ctx, key, payload).Err()
}

func (m *redisMailbox) Receive(ctx context.Context) (mailboxMessage, error) {
	if m.code == "" {
		return mailboxMessage{}, fmt.Errorf("mailbox code not set")
	}
	key := m.inboundKey()
	for {
		res, err := m.client.BLPop(ctx, m.ttl, key).Result()
		if err != nil {
			if err == redis.Nil {
				continue
			}
			return mailboxMessage{}, err
		}
		if len(res) < 2 {
			continue
		}
		var msg mailboxMessage
		if err := json.Unmarshal([]byte(res[1]), &msg); err != nil {
			return mailboxMessage{}, err
		}
		return msg, nil
	}
}

func (m *redisMailbox) Cleanup(ctx context.Context) {
	if m.code == "" {
		return
	}
	keys := []string{
		m.sessionKey(),
		m.key("self", "send"),
		m.key("self", "recv"),
		m.channelKey("send"),
		m.channelKey("recv"),
		m.key("recv", "lock"),
	}
	_ = m.client.Del(ctx, keys...).Err()
}

func (m *redisMailbox) claimSender(ctx context.Context, code string) error {
	session := m.sessionKeyFor(code)
	ok, err := m.client.SetNX(ctx, session, "active", m.ttl).Result()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("code %s already in use", code)
	}
	return nil
}

func (m *redisMailbox) claimReceiver(ctx context.Context, code string) error {
	session := m.sessionKeyFor(code)
	deadline := time.Now().Add(m.ttl)
	for {
		exists, err := m.client.Exists(ctx, session).Result()
		if err != nil {
			return err
		}
		if exists > 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("code %s not available", code)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	lockKey := m.keyFor(code, "recv", "lock")
	ok, err := m.client.SetNX(ctx, lockKey, "1", m.ttl).Result()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("receiver already connected for %s", code)
	}
	return nil
}

func (m *redisMailbox) outboundKey() string {
	if m.role == "send" {
		return m.channelKey("send")
	}
	return m.channelKey("recv")
}

func (m *redisMailbox) inboundKey() string {
	if m.role == "send" {
		return m.channelKey("recv")
	}
	return m.channelKey("send")
}

func (m *redisMailbox) sessionKey() string {
	return m.sessionKeyFor(m.code)
}

func (m *redisMailbox) sessionKeyFor(code string) string {
	return m.keyFor(code, "session")
}

func (m *redisMailbox) channelKey(direction string) string {
	return m.key(direction, "stream")
}

func (m *redisMailbox) key(suffix ...string) string {
	return m.keyFor(m.code, suffix...)
}

func (m *redisMailbox) keyFor(code string, suffix ...string) string {
	builder := strings.Builder{}
	builder.WriteString(m.prefix)
	builder.WriteString(":")
	builder.WriteString(code)
	for _, part := range suffix {
		builder.WriteString(":")
		builder.WriteString(part)
	}
	return builder.String()
}
