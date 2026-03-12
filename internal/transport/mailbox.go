package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jdefrancesco/internal/rendezvous"
)

type redisMailbox struct {
	client *redis.Client
	code   string
	ttl    time.Duration
	prefix string
	role   string
	stop   func()

	store *sessionStore
}

func newRedisMailbox(ctx context.Context, addr string, ttl time.Duration, role string) (*redisMailbox, error) {
	opts, err := redis.ParseURL(addr)
	if err != nil {
		opts = &redis.Options{Addr: addr}
	}
	if useEmbeddedByDefault(addr, opts) {
		return startEmbeddedMailbox(ttl, role)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		useEmbedded := addr == "" || addr == defaultRelay || errors.Is(err, context.DeadlineExceeded)
		if !useEmbedded {
			return nil, fmt.Errorf("redis connection failed: %w", err)
		}
		return startEmbeddedMailbox(ttl, role)
	}
	return newRedisMailboxWithClient(client, ttl, role, nil)
}

func newRedisMailboxWithClient(client *redis.Client, ttl time.Duration, role string, stop func()) (*redisMailbox, error) {
	return &redisMailbox{
		client: client,
		ttl:    ttl,
		prefix: "wormzy",
		role:   role,
		stop:   stop,
		store:  newSessionStore(client, ttl, "wormzy"),
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

func useEmbeddedByDefault(addr string, opts *redis.Options) bool {
	if addr != "" && addr != defaultRelay {
		return false
	}
	target := opts.Addr
	if target == "" {
		target = defaultRelay
	}
	conn, err := net.DialTimeout("tcp", target, 200*time.Millisecond)
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

func startEmbeddedMailbox(ttl time.Duration, role string) (*redisMailbox, error) {
	mini, err := miniredis.Run()
	if err != nil {
		return nil, fmt.Errorf("embedded redis unavailable: %w", err)
	}
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	return newRedisMailboxWithClient(client, ttl, role, mini.Close)
}

func (m *redisMailbox) Claim(ctx context.Context, requested string) (string, error) {
	if m.role == "send" {
		code := requested
		for {
			if code == "" {
				code = rendezvous.GenerateCode()
			}
			_, err := m.store.registerSender(ctx, code)
			if err == nil {
				m.code = code
				return code, nil
			}
			if !errors.Is(err, errSenderInUse) || requested != "" {
				return "", err
			}
			code = ""
		}
	}

	if requested == "" {
		return "", fmt.Errorf("receiver requires a pairing code")
	}
	if _, err := m.store.registerReceiver(ctx, requested); err != nil {
		return "", err
	}
	m.code = requested
	return requested, nil
}

func (m *redisMailbox) StoreSelf(ctx context.Context, info rendezvous.SelfInfo) error {
	if m.code == "" {
		return fmt.Errorf("mailbox code not set")
	}
	return m.store.updatePeerInfo(ctx, m.code, m.role, info)
}

func (m *redisMailbox) WaitPeer(ctx context.Context) (*rendezvous.SelfInfo, error) {
	if m.code == "" {
		return nil, fmt.Errorf("mailbox code not set")
	}
	return m.store.waitForPeer(ctx, m.code, m.role)
}

func (m *redisMailbox) Send(ctx context.Context, typ string, body any) error {
	if m.code == "" {
		return fmt.Errorf("mailbox code not set")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	msg := mailboxMessage{Type: typ, Body: raw}
	dest := oppositeRole(m.role)
	return m.store.enqueue(ctx, m.code, dest, msg)
}

func (m *redisMailbox) Receive(ctx context.Context) (mailboxMessage, error) {
	if m.code == "" {
		return mailboxMessage{}, fmt.Errorf("mailbox code not set")
	}
	return m.store.dequeue(ctx, m.code, m.role)
}

func (m *redisMailbox) ReportStats(ctx context.Context, stats transferStats) error {
	if m.code == "" {
		return fmt.Errorf("mailbox code not set")
	}
	if stats.Mode == "" {
		stats.Mode = m.role
	}
	return m.store.recordStats(ctx, m.code, stats)
}

func (m *redisMailbox) Cleanup(ctx context.Context) {
	if m.code == "" {
		return
	}
	_ = m.store.delete(ctx, m.code)
}
