package transport

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testRelayPin returns the canonical SPKI pin for cert.
func testRelayPin(cert *x509.Certificate) string {
	digest := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(digest[:])
}

// trustRelayTestCertificate adds cert as a trust anchor for mailbox.
func trustRelayTestCertificate(t *testing.T, mailbox mailbox, cert *x509.Certificate) {
	t.Helper()
	httpMailbox, ok := mailbox.(*httpMailbox)
	if !ok {
		t.Fatalf("mailbox type = %T; want *httpMailbox", mailbox)
	}
	transport, ok := httpMailbox.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("HTTP transport type = %T; want *http.Transport", httpMailbox.client.Transport)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	transport.TLSClientConfig.RootCAs = roots
	t.Cleanup(transport.CloseIdleConnections)
}

// TestDecodeRelaySPKIPin verifies pins use canonical standard base64 and one SHA-256 digest.
func TestDecodeRelaySPKIPin(t *testing.T) {
	digest := sha256.Sum256([]byte("wormzy relay pin"))
	valid := base64.StdEncoding.EncodeToString(digest[:])
	tests := []struct {
		name    string
		pin     string
		wantPin bool
		wantErr bool
	}{
		{name: "disabled", pin: ""},
		{name: "valid", pin: valid, wantPin: true},
		{name: "wrong length", pin: base64.StdEncoding.EncodeToString(digest[:sha256.Size-1]), wantErr: true},
		{name: "missing padding", pin: strings.TrimSuffix(valid, "="), wantErr: true},
		{name: "URL alphabet", pin: base64.RawURLEncoding.EncodeToString(digest[:]), wantErr: true},
		{name: "leading whitespace", pin: " " + valid, wantErr: true},
		{name: "trailing data", pin: valid + "x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeRelaySPKIPin(tt.pin)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decode error = %v; wantErr %t", err, tt.wantErr)
			}
			if tt.wantPin && base64.StdEncoding.EncodeToString(got) != valid {
				t.Fatalf("decoded pin = %q; want %q", base64.StdEncoding.EncodeToString(got), valid)
			}
			if !tt.wantPin && !tt.wantErr && got != nil {
				t.Fatalf("decoded disabled pin = %x; want nil", got)
			}
		})
	}
}

// TestNewMailboxRejectsUnsafeRelayPinConfiguration verifies invalid pins fail before any dial.
func TestNewMailboxRejectsUnsafeRelayPinConfiguration(t *testing.T) {
	digest := sha256.Sum256([]byte("wormzy relay pin"))
	valid := base64.StdEncoding.EncodeToString(digest[:])
	tests := []struct {
		name     string
		endpoint string
		pin      string
	}{
		{name: "malformed pin", endpoint: "https://relay.example.test", pin: "not-base64"},
		{name: "HTTP endpoint", endpoint: "http://relay.example.test", pin: valid},
		{name: "Redis endpoint", endpoint: "redis://relay.example.test:6379", pin: valid},
		{name: "bare endpoint", endpoint: "relay.example.test:6379", pin: valid},
		{name: "HTTPS missing host", endpoint: "https:///mailbox", pin: valid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := newMailbox(ctx, Config{
				Mode:             "recv",
				RelayAddr:        tt.endpoint,
				RelayPin:         tt.pin,
				HandshakeTimeout: time.Second,
			}); err == nil {
				t.Fatal("newMailbox accepted unsafe relay pin configuration")
			}
		})
	}
}

// TestHTTPMailboxRelayPin verifies SPKI pinning supplements normal TLS verification.
func TestHTTPMailboxRelayPin(t *testing.T) {
	requested := sessionStoreTestID(0x71)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"` + requested + `"}`))
	}))
	defer server.Close()
	pin := testRelayPin(server.Certificate())

	t.Run("matching pin and valid PKI", func(t *testing.T) {
		mailbox, err := newMailbox(context.Background(), Config{
			Mode:             "send",
			RelayAddr:        server.URL,
			RelayPin:         pin,
			HandshakeTimeout: time.Second,
		})
		if err != nil {
			t.Fatalf("new pinned mailbox: %v", err)
		}
		trustRelayTestCertificate(t, mailbox, server.Certificate())
		if code, err := mailbox.Claim(context.Background(), requested); err != nil || code != requested {
			t.Fatalf("claim = %q, %v; want %q", code, err, requested)
		}
	})

	t.Run("matching pin does not bypass unknown CA", func(t *testing.T) {
		mailbox, err := newMailbox(context.Background(), Config{
			Mode:             "send",
			RelayAddr:        server.URL,
			RelayPin:         pin,
			HandshakeTimeout: time.Second,
		})
		if err != nil {
			t.Fatalf("new pinned mailbox: %v", err)
		}
		if _, err := mailbox.Claim(context.Background(), requested); !errors.Is(err, errMailboxUnavailable) {
			t.Fatalf("claim error = %v; want %v", err, errMailboxUnavailable)
		}
	})

	t.Run("wrong pin rejects trusted certificate", func(t *testing.T) {
		wrongDigest := sha256.Sum256([]byte("different relay key"))
		wrongPin := base64.StdEncoding.EncodeToString(wrongDigest[:])
		mailbox, err := newMailbox(context.Background(), Config{
			Mode:             "send",
			RelayAddr:        server.URL,
			RelayPin:         wrongPin,
			HandshakeTimeout: time.Second,
		})
		if err != nil {
			t.Fatalf("new pinned mailbox: %v", err)
		}
		trustRelayTestCertificate(t, mailbox, server.Certificate())
		if _, err := mailbox.Claim(context.Background(), requested); !errors.Is(err, errRelayPinMismatch) {
			t.Fatalf("claim error = %v; want %v", err, errRelayPinMismatch)
		}
	})

	t.Run("matching pin does not bypass hostname", func(t *testing.T) {
		relayURL, err := url.Parse(server.URL)
		if err != nil {
			t.Fatalf("parse test URL: %v", err)
		}
		relayURL.Host = "localhost:" + relayURL.Port()
		mailbox, err := newMailbox(context.Background(), Config{
			Mode:             "send",
			RelayAddr:        relayURL.String(),
			RelayPin:         pin,
			HandshakeTimeout: time.Second,
		})
		if err != nil {
			t.Fatalf("new pinned mailbox: %v", err)
		}
		trustRelayTestCertificate(t, mailbox, server.Certificate())
		if _, err := mailbox.Claim(context.Background(), requested); !errors.Is(err, errMailboxUnavailable) {
			t.Fatalf("claim error = %v; want %v", err, errMailboxUnavailable)
		}
	})

	if got := requests.Load(); got != 1 {
		t.Fatalf("server received %d application requests; want only the successful pinned request", got)
	}
}
