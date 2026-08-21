package transport

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestValidateMailboxEndpointEnforcesRemoteHTTPS verifies direct Redis remains local-development only.
func TestValidateMailboxEndpointEnforcesRemoteHTTPS(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{name: "remote HTTPS mailbox", endpoint: "https://relay.example.test"},
		{name: "loopback HTTP mailbox", endpoint: "http://127.0.0.1:8080"},
		{name: "localhost HTTP mailbox", endpoint: "http://localhost:8080"},
		{name: "loopback bare Redis", endpoint: "127.0.0.1:6379"},
		{name: "localhost Redis URL", endpoint: "redis://localhost:6379/0"},
		{name: "IPv6 loopback Redis URL", endpoint: "rediss://[::1]:6379"},
		{name: "local Unix Redis", endpoint: "unix:///tmp/wormzy-redis.sock"},
		{name: "remote HTTP mailbox", endpoint: "http://relay.example.test", wantErr: true},
		{name: "localhost subdomain HTTP mailbox", endpoint: "http://mailbox.localhost:8080", wantErr: true},
		{name: "uppercase localhost HTTP mailbox", endpoint: "http://LOCALHOST:8080", wantErr: true},
		{name: "remote bare Redis", endpoint: "redis.example.test:6379", wantErr: true},
		{name: "remote plaintext Redis URL", endpoint: "redis://redis.example.test:6379", wantErr: true},
		{name: "remote TLS Redis URL", endpoint: "rediss://redis.example.test:6379", wantErr: true},
		{name: "LAN Redis", endpoint: "192.168.1.20:6379", wantErr: true},
		{name: "ambiguous local address", endpoint: ":6379", wantErr: true},
		{name: "relative Unix Redis", endpoint: "unix://tmp/redis.sock", wantErr: true},
		{name: "mixed-case Redis scheme", endpoint: "Redis://localhost:6379", wantErr: true},
		{name: "mixed-case Unix scheme", endpoint: "UNIX:///tmp/wormzy-redis.sock", wantErr: true},
		{name: "unsupported scheme", endpoint: "tcp://127.0.0.1:6379", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMailboxEndpoint(tt.endpoint)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateMailboxEndpoint(%q) error = %v; wantErr %t", tt.endpoint, err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, errInvalidMailboxEndpoint) {
				t.Fatalf("error = %v; want %v", err, errInvalidMailboxEndpoint)
			}
		})
	}
}

// TestNewMailboxRejectsRemoteRedisBeforeDial verifies transfer setup fails closed without network access.
func TestNewMailboxRejectsRemoteRedisBeforeDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := newMailbox(ctx, Config{
		Mode:             "send",
		RelayAddr:        "redis://198.51.100.20:6379",
		HandshakeTimeout: time.Second,
	})
	if !errors.Is(err, errInvalidMailboxEndpoint) {
		t.Fatalf("newMailbox error = %v; want %v", err, errInvalidMailboxEndpoint)
	}
}
