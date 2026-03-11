package transport

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func newMailbox(ctx context.Context, cfg Config) (mailbox, error) {
	if strings.HasPrefix(cfg.RelayAddr, "http://") || strings.HasPrefix(cfg.RelayAddr, "https://") {
		return newHTTPMailbox(cfg.RelayAddr, cfg.Mode, cfg.Timeout), nil
	}
	return newRedisMailbox(ctx, cfg.RelayAddr, cfg.Timeout, cfg.Mode)
}

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
	}
}
