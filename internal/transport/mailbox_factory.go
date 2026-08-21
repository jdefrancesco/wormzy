package transport

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type mailboxEndpointKind uint8

const (
	mailboxEndpointHTTP mailboxEndpointKind = iota
	mailboxEndpointLocalRedis
)

// newMailbox selects the configured HTTPS or local-development Redis mailbox client.
func newMailbox(ctx context.Context, cfg Config) (mailbox, error) {
	kind, err := mailboxEndpointType(cfg.RelayAddr)
	if err != nil {
		return nil, err
	}
	ttl := cfg.sessionTTL()
	if cfg.RelayPin != "" {
		if kind != mailboxEndpointHTTP {
			return nil, errInvalidMailboxEndpoint
		}
		return newHTTPMailboxWithPin(cfg.RelayAddr, cfg.Mode, cfg.HandshakeTimeout, cfg.RelayPin)
	}
	if kind == mailboxEndpointHTTP {
		if _, err := parseHTTPMailboxBase(cfg.RelayAddr); err != nil {
			return nil, err
		}
		return newHTTPMailbox(cfg.RelayAddr, cfg.Mode, cfg.HandshakeTimeout), nil
	}
	return newRedisMailbox(ctx, cfg.RelayAddr, ttl, cfg.Mode)
}

// ValidateMailboxEndpoint verifies that a mailbox uses HTTPS or an explicitly local development transport.
func ValidateMailboxEndpoint(endpoint string) error {
	_, err := mailboxEndpointType(endpoint)
	return err
}

// mailboxEndpointType classifies a validated HTTP or local Redis mailbox endpoint.
func mailboxEndpointType(endpoint string) (mailboxEndpointKind, error) {
	if endpoint == "" {
		return mailboxEndpointLocalRedis, nil
	}
	if strings.TrimSpace(endpoint) != endpoint {
		return 0, errInvalidMailboxEndpoint
	}
	if strings.Contains(endpoint, "://") {
		schemeText, _, _ := strings.Cut(endpoint, "://")
		if schemeText != strings.ToLower(schemeText) {
			return 0, errInvalidMailboxEndpoint
		}
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed == nil {
			return 0, errInvalidMailboxEndpoint
		}
		switch parsed.Scheme {
		case "http", "https":
			if _, err := parseHTTPMailboxBase(endpoint); err != nil {
				return 0, err
			}
			return mailboxEndpointHTTP, nil
		case "redis", "rediss":
			if err := validateLocalRedisURL(parsed); err != nil {
				return 0, err
			}
			return mailboxEndpointLocalRedis, nil
		case "unix":
			if parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" || !filepath.IsAbs(parsed.Path) {
				return 0, errInvalidMailboxEndpoint
			}
			return mailboxEndpointLocalRedis, nil
		default:
			return 0, errInvalidMailboxEndpoint
		}
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || !isLoopbackMailboxHost(host) || !validMailboxPort(port) {
		return 0, errInvalidMailboxEndpoint
	}
	return mailboxEndpointLocalRedis, nil
}

// validateLocalRedisURL accepts Redis URLs only when their literal host is loopback.
func validateLocalRedisURL(endpoint *url.URL) error {
	if endpoint == nil || endpoint.Host == "" || endpoint.Fragment != "" || !isLoopbackMailboxHost(endpoint.Hostname()) {
		return errInvalidMailboxEndpoint
	}
	if port := endpoint.Port(); port != "" && !validMailboxPort(port) {
		return errInvalidMailboxEndpoint
	}
	return nil
}

// validMailboxPort reports whether a port is an explicit usable TCP port.
func validMailboxPort(port string) bool {
	value, err := strconv.Atoi(port)
	return err == nil && value > 0 && value <= 65535
}

// newHTTPClient returns a mailbox client with a bounded request lifetime.
func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
	}
}
