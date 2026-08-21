package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mailboxRoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip executes the test transport function.
func (fn mailboxRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

// mailboxTestResponse builds an in-memory HTTP response for client-bound tests.
func mailboxTestResponse(body string) *http.Response {
	return mailboxTestStatusResponse(http.StatusOK, body)
}

// mailboxTestStatusResponse builds an in-memory HTTP response with a chosen status.
func mailboxTestStatusResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// mailboxHTTPTestCapability returns deterministic raw and verifier capability material.
func mailboxHTTPTestCapability(t *testing.T, marker byte) (string, string) {
	t.Helper()
	raw, verifier, err := generateMailboxCapability(bytes.NewReader(bytes.Repeat([]byte{marker}, mailboxCapabilitySize)))
	if err != nil {
		t.Fatalf("generate capability: %v", err)
	}
	return raw, verifier
}

// TestMailboxHTTPServerRejectsInvalidClaimsGenerically verifies malformed claim identity never reaches Redis.
func TestMailboxHTTPServerRejectsInvalidClaimsGenerically(t *testing.T) {
	_, capabilityHash := mailboxHTTPTestCapability(t, 0x65)
	validID := sessionStoreTestID(0x65)
	tests := []struct {
		name string
		body string
	}{
		{name: "missing capability hash", body: `{"role":"send","requested":"` + validID + `"}`},
		{name: "malformed capability hash", body: `{"role":"send","requested":"` + validID + `","capability_hash":"bad"}`},
		{name: "invalid role", body: `{"role":"admin","requested":"` + validID + `","capability_hash":"` + capabilityHash + `"}`},
		{name: "invalid session", body: `{"role":"send","requested":"pairing-secret","capability_hash":"` + capabilityHash + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, mailboxHTTPClaimPath, strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			(&MailboxHTTPServer{}).ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want %d", response.Code, http.StatusBadRequest)
			}
			if !strings.Contains(response.Body.String(), errMailboxUnavailable.Error()) {
				t.Fatalf("response = %q; want generic unavailable error", response.Body.String())
			}
		})
	}
}

// TestMailboxHTTPServerRejectsOversizedRequests verifies every JSON endpoint reads through the shared request cap.
func TestMailboxHTTPServerRejectsOversizedRequests(t *testing.T) {
	server := &MailboxHTTPServer{}
	capability, _ := mailboxHTTPTestCapability(t, 0x61)
	paths := []string{
		mailboxHTTPClaimPath,
		mailboxHTTPSelfPath,
		mailboxHTTPWaitPeerPath,
		mailboxHTTPSendPath,
		mailboxHTTPReceivePath,
		mailboxHTTPStatsPath,
	}
	for _, endpoint := range paths {
		t.Run(endpoint, func(t *testing.T) {
			body := strings.NewReader(`{"oversized":"` + strings.Repeat("x", maxHTTPMailboxPayloadSize) + `"}`)
			request := httptest.NewRequest(http.MethodPost, endpoint, body)
			request.Header.Set("Content-Type", "application/json")
			if endpoint != mailboxHTTPClaimPath {
				request.Header.Set("Authorization", "Bearer "+capability)
			}
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want %d", response.Code, http.StatusBadRequest)
			}
			if !strings.Contains(response.Body.String(), "request body too large") {
				t.Fatalf("response = %q; want bounded-body error", response.Body.String())
			}
		})
	}
}

// TestMailboxHTTPServerRejectsTrailingJSON verifies all request decoders reject a second JSON value.
func TestMailboxHTTPServerRejectsTrailingJSON(t *testing.T) {
	server := &MailboxHTTPServer{}
	capability, _ := mailboxHTTPTestCapability(t, 0x62)
	paths := []string{
		mailboxHTTPClaimPath,
		mailboxHTTPSelfPath,
		mailboxHTTPWaitPeerPath,
		mailboxHTTPSendPath,
		mailboxHTTPReceivePath,
		mailboxHTTPStatsPath,
	}
	for _, endpoint := range paths {
		t.Run(endpoint, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{} {}`))
			request.Header.Set("Content-Type", "application/json")
			if endpoint != mailboxHTTPClaimPath {
				request.Header.Set("Authorization", "Bearer "+capability)
			}
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want %d", response.Code, http.StatusBadRequest)
			}
			if !strings.Contains(response.Body.String(), "trailing data") {
				t.Fatalf("response = %q; want trailing-data error", response.Body.String())
			}
		})
	}
}

// TestMailboxHTTPServerValidatesSendEnvelopeBeforeRedis verifies invalid decoded messages never reach storage.
func TestMailboxHTTPServerValidatesSendEnvelopeBeforeRedis(t *testing.T) {
	server := &MailboxHTTPServer{}
	capability, _ := mailboxHTTPTestCapability(t, 0x63)
	tests := []struct {
		name string
		typ  string
		body []byte
	}{
		{name: "empty type", body: []byte(`{}`)},
		{name: "invalid JSON body", typ: "test", body: []byte(`not-json`)},
		{name: "oversized decoded body", typ: "test", body: make([]byte, maxDeferredMailboxBodySize+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := `{"role":"send","code":"` + sessionStoreTestID(0x60) + `","type":"` + tt.typ + `","body":"` +
				base64.StdEncoding.EncodeToString(tt.body) + `"}`
			request := httptest.NewRequest(http.MethodPost, mailboxHTTPSendPath, strings.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+capability)
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want %d", response.Code, http.StatusBadRequest)
			}
		})
	}
}

// TestMailboxHTTPServerRejectsUnknownJSONFields verifies request schemas are strict.
func TestMailboxHTTPServerRejectsUnknownJSONFields(t *testing.T) {
	_, capabilityHash := mailboxHTTPTestCapability(t, 0x64)
	payload := `{"role":"send","requested":"` + sessionStoreTestID(0x61) + `","capability_hash":"` + capabilityHash + `","unknown":true}`
	request := httptest.NewRequest(http.MethodPost, mailboxHTTPClaimPath, strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	(&MailboxHTTPServer{}).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want %d", response.Code, http.StatusBadRequest)
	}
	if !strings.Contains(response.Body.String(), "unknown field") {
		t.Fatalf("response = %q; want unknown-field error", response.Body.String())
	}
}

// TestMailboxHTTPServerRequiresPostJSON verifies every JSON route rejects unsafe methods and media types.
func TestMailboxHTTPServerRequiresPostJSON(t *testing.T) {
	server := &MailboxHTTPServer{}
	paths := []string{
		mailboxHTTPClaimPath,
		mailboxHTTPSelfPath,
		mailboxHTTPWaitPeerPath,
		mailboxHTTPSendPath,
		mailboxHTTPReceivePath,
		mailboxHTTPStatsPath,
	}
	for _, endpoint := range paths {
		t.Run(endpoint+" method", func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, endpoint, nil)
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("status/allow = %d/%q; want %d/%q", response.Code, response.Header().Get("Allow"), http.StatusMethodNotAllowed, http.MethodPost)
			}
		})
		t.Run(endpoint+" media type", func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "text/plain")
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			if response.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d; want %d", response.Code, http.StatusUnsupportedMediaType)
			}
		})
	}

	request := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("health status/allow = %d/%q; want %d/%q", response.Code, response.Header().Get("Allow"), http.StatusMethodNotAllowed, http.MethodGet)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("legacy route status = %d; want %d", response.Code, http.StatusNotFound)
	}
}

// TestHTTPMailboxRejectsOversizedRequest verifies the client does not transmit an oversized envelope.
func TestHTTPMailboxRejectsOversizedRequest(t *testing.T) {
	var requests atomic.Int32
	mailbox := newHTTPMailbox("http://127.0.0.1", "send", time.Second).(*httpMailbox)
	mailbox.client = &http.Client{Transport: mailboxRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		requests.Add(1)
		return mailboxTestResponse(`{"status":"ok"}`), nil
	})}

	err := mailbox.doJSON(
		context.Background(),
		http.MethodPost,
		mailboxHTTPSendPath,
		map[string]string{"body": strings.Repeat("x", maxHTTPMailboxPayloadSize)},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "request exceeds") {
		t.Fatalf("error = %v; want oversized-request error", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("server received %d requests; want 0", got)
	}
}

// TestHTTPMailboxClaimSendsVerifierOnly verifies the raw capability never appears in claim transport.
func TestHTTPMailboxClaimSendsVerifierOnly(t *testing.T) {
	requested := sessionStoreTestID(0x66)
	mailbox := newHTTPMailbox("http://127.0.0.1", "send", time.Second).(*httpMailbox)
	mailbox.client = &http.Client{Transport: mailboxRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != mailboxHTTPClaimPath {
			t.Fatalf("claim path = %q; want %q", request.URL.Path, mailboxHTTPClaimPath)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Fatalf("claim exposed bearer authorization %q", authorization)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read claim: %v", err)
		}
		if bytes.Contains(body, []byte(mailbox.capability)) {
			t.Fatal("claim body contains raw capability")
		}
		if !bytes.Contains(body, []byte(mailbox.capabilityHash)) {
			t.Fatal("claim body does not contain capability verifier")
		}
		return mailboxTestResponse(`{"code":"` + requested + `"}`), nil
	})}

	if code, err := mailbox.Claim(context.Background(), requested); err != nil || code != requested {
		t.Fatalf("claim = %q, %v; want %q", code, err, requested)
	}
}

// TestHTTPMailboxRejectsInvalidClaimBeforeRequest verifies unsafe storage identifiers stay local.
func TestHTTPMailboxRejectsInvalidClaimBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	mailbox := newHTTPMailbox("http://127.0.0.1", "send", time.Second).(*httpMailbox)
	mailbox.client = &http.Client{Transport: mailboxRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		requests.Add(1)
		return mailboxTestResponse(`{}`), nil
	})}

	if _, err := mailbox.Claim(context.Background(), "pairing-secret"); !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("error = %v; want %v", err, errMailboxUnavailable)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("server received %d requests; want 0", got)
	}
}

// TestHTTPMailboxRejectsMalformedEndpoint verifies invalid URLs fail without a nil-pointer panic.
func TestHTTPMailboxRejectsMalformedEndpoint(t *testing.T) {
	mailbox := newHTTPMailbox("http://%", "send", time.Second)
	_, err := mailbox.Claim(context.Background(), sessionStoreTestID(0x61))
	if !errors.Is(err, errInvalidMailboxEndpoint) {
		t.Fatalf("malformed endpoint error = %v; want %v", err, errInvalidMailboxEndpoint)
	}
}

// TestMailboxEndpointRequiresHTTPSOffHost verifies capabilities and candidate
// metadata cannot be sent over plaintext networks by configuration mistake.
func TestMailboxEndpointRequiresHTTPSOffHost(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{name: "remote HTTPS", endpoint: "https://relay.example.test"},
		{name: "IPv4 loopback HTTP", endpoint: "http://127.0.0.1:8080"},
		{name: "IPv6 loopback HTTP", endpoint: "http://[::1]:8080"},
		{name: "localhost HTTP", endpoint: "http://localhost:8080"},
		{name: "localhost subdomain HTTP", endpoint: "http://mailbox.localhost:8080", wantErr: true},
		{name: "uppercase localhost HTTP", endpoint: "http://LOCALHOST:8080", wantErr: true},
		{name: "remote HTTP", endpoint: "http://relay.example.test", wantErr: true},
		{name: "public IPv4 HTTP", endpoint: "http://8.8.8.8", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newMailbox(context.Background(), Config{
				Mode:             "send",
				RelayAddr:        tt.endpoint,
				HandshakeTimeout: time.Second,
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("newMailbox(%q) error = %v; wantErr %t", tt.endpoint, err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, errInvalidMailboxEndpoint) {
				t.Fatalf("newMailbox(%q) error = %v; want %v", tt.endpoint, err, errInvalidMailboxEndpoint)
			}
		})
	}
}

// TestHTTPMailboxRejectsOversizedResponse verifies response buffering stops at the protocol limit.
func TestHTTPMailboxRejectsOversizedResponse(t *testing.T) {
	mailbox := newHTTPMailbox("http://127.0.0.1", "send", time.Second)
	mailbox.(*httpMailbox).client = &http.Client{Transport: mailboxRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return mailboxTestResponse(strings.Repeat("x", maxHTTPMailboxPayloadSize+1)), nil
	})}
	_, err := mailbox.Claim(context.Background(), sessionStoreTestID(0x62))
	if !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("error = %v; want %v", err, errMailboxUnavailable)
	}
}

// TestHTTPMailboxRejectsTrailingResponseJSON verifies the client accepts exactly one response value.
func TestHTTPMailboxRejectsTrailingResponseJSON(t *testing.T) {
	mailbox := newHTTPMailbox("http://127.0.0.1", "send", time.Second)
	mailbox.(*httpMailbox).client = &http.Client{Transport: mailboxRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return mailboxTestResponse(`{"code":"` + sessionStoreTestID(0x63) + `"} {}`), nil
	})}
	_, err := mailbox.Claim(context.Background(), sessionStoreTestID(0x63))
	if !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("error = %v; want %v", err, errMailboxUnavailable)
	}
}

// TestHTTPMailboxRejectsInvalidMessageJSON verifies malformed remote envelopes fail generically.
func TestHTTPMailboxRejectsInvalidMessageJSON(t *testing.T) {
	mailbox := newHTTPMailbox("http://127.0.0.1", "recv", time.Second).(*httpMailbox)
	mailbox.code = sessionStoreTestID(0x67)
	mailbox.client = &http.Client{Transport: mailboxRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return mailboxTestResponse(`{"type":"test","body":"bm90LWpzb24="}`), nil
	})}

	if _, err := mailbox.Receive(context.Background()); !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("error = %v; want %v", err, errMailboxUnavailable)
	}
}

// TestHTTPMailboxSanitizesRemoteErrors verifies server-provided text cannot reach client logs.
func TestHTTPMailboxSanitizesRemoteErrors(t *testing.T) {
	const remoteSecret = "SECRET pairing value and internal Redis key"
	mailbox := newHTTPMailbox("http://127.0.0.1", "send", time.Second).(*httpMailbox)
	mailbox.client = &http.Client{Transport: mailboxRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return mailboxTestStatusResponse(http.StatusBadRequest, `{"error":"`+remoteSecret+`"}`), nil
	})}

	_, err := mailbox.Claim(context.Background(), sessionStoreTestID(0x64))
	if !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("error = %v; want %v", err, errMailboxUnavailable)
	}
	if strings.Contains(err.Error(), remoteSecret) {
		t.Fatalf("remote error text leaked: %v", err)
	}
}

// TestHTTPMailboxSanitizesTransportErrors verifies remote redirect or transport text cannot reach logs.
func TestHTTPMailboxSanitizesTransportErrors(t *testing.T) {
	const remoteSecret = "SECRET redirect location or transport detail"
	mailbox := newHTTPMailbox("http://127.0.0.1", "send", time.Second).(*httpMailbox)
	mailbox.client = &http.Client{Transport: mailboxRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New(remoteSecret)
	})}

	_, err := mailbox.Claim(context.Background(), sessionStoreTestID(0x68))
	if !errors.Is(err, errMailboxUnavailable) {
		t.Fatalf("error = %v; want %v", err, errMailboxUnavailable)
	}
	if strings.Contains(err.Error(), remoteSecret) {
		t.Fatalf("transport error text leaked: %v", err)
	}
}
