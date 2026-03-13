package transport

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func newMailbox(ctx context.Context, cfg Config) (mailbox, error) {
	ttl := cfg.sessionTTL()
	if strings.HasPrefix(cfg.RelayAddr, "http://") || strings.HasPrefix(cfg.RelayAddr, "https://") {
		return newHTTPMailbox(cfg.RelayAddr, cfg.Mode, cfg.HandshakeTimeout), nil
	}
	return newRedisMailbox(ctx, cfg.RelayAddr, ttl, cfg.Mode)
}

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
	}
}
