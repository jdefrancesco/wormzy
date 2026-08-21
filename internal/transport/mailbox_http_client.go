package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	maxHTTPMailboxPayloadSize = 128 << 10
	mailboxHTTPClaimPath      = "/v2/claim"
	mailboxHTTPSelfPath       = "/v2/self"
	mailboxHTTPWaitPeerPath   = "/v2/wait-peer" // #nosec G101 -- HTTP route, not a credential.
	mailboxHTTPSendPath       = "/v2/send"
	mailboxHTTPReceivePath    = "/v2/receive"
	mailboxHTTPStatsPath      = "/v2/stats"
)

var errInvalidMailboxEndpoint = errors.New("invalid mailbox endpoint")

type httpMailbox struct {
	client         *http.Client
	base           *url.URL
	role           string
	code           string
	capability     string
	capabilityHash string
	capabilityErr  error
}

// newHTTPMailbox creates an unpinned HTTP mailbox client for existing callers.
func newHTTPMailbox(addr, role string, timeout time.Duration) mailbox {
	return newHTTPMailboxWithClient(addr, role, newHTTPClient(timeout))
}

// newHTTPMailboxWithPin creates an HTTPS mailbox with additive SPKI pinning.
func newHTTPMailboxWithPin(addr, role string, timeout time.Duration, encodedPin string) (mailbox, error) {
	digest, err := decodeRelaySPKIPin(encodedPin)
	if err != nil {
		return nil, err
	}
	if err := validateRelayPinEndpoint(addr, digest); err != nil {
		return nil, err
	}
	if len(digest) == 0 {
		return newHTTPMailbox(addr, role, timeout), nil
	}
	return newHTTPMailboxWithClient(addr, role, newRelayPinnedHTTPClient(timeout, digest)), nil
}

// newHTTPMailboxWithClient constructs a mailbox around a configured HTTP client.
func newHTTPMailboxWithClient(addr, role string, client *http.Client) mailbox {
	u, endpointErr := parseHTTPMailboxBase(addr)
	if endpointErr != nil {
		// Keep the object structurally safe; every network operation returns
		// capabilityErr before this placeholder can be used.
		u = &url.URL{Scheme: "http", Host: "invalid.invalid"}
	}
	capability, capabilityHash, capabilityErr := generateMailboxCapability(nil)
	if endpointErr != nil {
		capabilityErr = endpointErr
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &httpMailbox{
		client:         client,
		base:           u,
		role:           role,
		capability:     capability,
		capabilityHash: capabilityHash,
		capabilityErr:  capabilityErr,
	}
}

// parseHTTPMailboxBase validates the absolute HTTP(S) base used for mailbox requests.
func parseHTTPMailboxBase(addr string) (*url.URL, error) {
	u, err := url.Parse(addr)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errInvalidMailboxEndpoint
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errInvalidMailboxEndpoint
	}
	if u.Scheme == "http" && !isLoopbackMailboxHost(u.Hostname()) {
		return nil, errInvalidMailboxEndpoint
	}
	return u, nil
}

// isLoopbackMailboxHost permits plaintext HTTP only for local development.
func isLoopbackMailboxHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (m *httpMailbox) endpoint(p string) string {
	u := *m.base
	u.Path = path.Join(u.Path, p)
	return u.String()
}

func (m *httpMailbox) Claim(ctx context.Context, requested string) (string, error) {
	if m.capabilityErr != nil {
		return "", m.capabilityErr
	}
	if !validMailboxSessionID(requested) {
		return "", errMailboxUnavailable
	}
	req := struct {
		Role           string `json:"role"`
		Requested      string `json:"requested,omitempty"`
		CapabilityHash string `json:"capability_hash"`
	}{
		Role:           m.role,
		Requested:      requested,
		CapabilityHash: m.capabilityHash,
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := m.doLongPollJSON(ctx, mailboxHTTPClaimPath, req, &resp); err != nil {
		return "", err
	}
	if !validMailboxSessionID(resp.Code) {
		return "", errMailboxUnavailable
	}
	m.code = resp.Code
	return resp.Code, nil
}

func (m *httpMailbox) StoreSelf(ctx context.Context, info rendezvous.SelfInfo) error {
	req := struct {
		Role string              `json:"role"`
		Code string              `json:"code"`
		Info rendezvous.SelfInfo `json:"info"`
	}{
		Role: m.role,
		Code: m.code,
		Info: info,
	}
	return m.doJSON(ctx, http.MethodPost, mailboxHTTPSelfPath, req, nil)
}

func (m *httpMailbox) WaitPeer(ctx context.Context) (*rendezvous.SelfInfo, error) {
	req := struct {
		Role string `json:"role"`
		Code string `json:"code"`
	}{
		Role: m.role,
		Code: m.code,
	}
	var resp struct {
		Info rendezvous.SelfInfo `json:"info"`
	}
	if err := m.doLongPollJSON(ctx, mailboxHTTPWaitPeerPath, req, &resp); err != nil {
		return nil, err
	}
	return &resp.Info, nil
}

func (m *httpMailbox) Send(ctx context.Context, typ string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err := validateMailboxMessage(mailboxMessage{Type: typ, Body: raw}); err != nil {
		return err
	}
	req := struct {
		Role string `json:"role"`
		Code string `json:"code"`
		Type string `json:"type"`
		Body string `json:"body"`
	}{
		Role: m.role,
		Code: m.code,
		Type: typ,
		Body: base64.StdEncoding.EncodeToString(raw),
	}
	return m.doJSON(ctx, http.MethodPost, mailboxHTTPSendPath, req, nil)
}

func (m *httpMailbox) Receive(ctx context.Context) (mailboxMessage, error) {
	req := struct {
		Role string `json:"role"`
		Code string `json:"code"`
	}{
		Role: m.role,
		Code: m.code,
	}
	var resp struct {
		Type string `json:"type"`
		Body string `json:"body"`
	}
	if err := m.doLongPollJSON(ctx, mailboxHTTPReceivePath, req, &resp); err != nil {
		return mailboxMessage{}, err
	}
	var msg mailboxMessage
	msg.Type = resp.Type
	if resp.Body != "" {
		data, err := base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			return mailboxMessage{}, errMailboxUnavailable
		}
		msg.Body = data
	}
	if err := validateMailboxMessage(msg); err != nil {
		return mailboxMessage{}, errMailboxUnavailable
	}
	if !json.Valid(msg.Body) {
		return mailboxMessage{}, errMailboxUnavailable
	}
	return msg, nil
}

// Close releases idle connections owned by a custom mailbox transport.
func (m *httpMailbox) Close() error {
	if closer, ok := m.client.Transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	return nil
}

func (m *httpMailbox) ReportStats(ctx context.Context, stats transferStats) error {
	req := struct {
		Role           string `json:"role"`
		Code           string `json:"code"`
		Mode           string `json:"mode"`
		Transport      string `json:"transport"`
		Candidate      string `json:"candidate"`
		DirectOutcome  string `json:"direct_outcome,omitempty"`
		DirectSummary  string `json:"direct_summary,omitempty"`
		Bytes          int64  `json:"bytes,omitempty"`
		DurationMillis int64  `json:"duration_ms,omitempty"`
		Completed      bool   `json:"completed"`
		Error          string `json:"error,omitempty"`
	}{
		Role:           m.role,
		Code:           m.code,
		Mode:           stats.Mode,
		Transport:      stats.Transport,
		Candidate:      stats.Candidate,
		DirectOutcome:  stats.DirectOutcome,
		DirectSummary:  stats.DirectSummary,
		Bytes:          stats.Bytes,
		DurationMillis: stats.DurationMillis,
		Completed:      stats.Completed,
		Error:          stats.Error,
	}
	return m.doJSON(ctx, http.MethodPost, mailboxHTTPStatsPath, req, nil)
}

func (m *httpMailbox) doJSON(ctx context.Context, method, endpoint string, reqBody any, respBody any) error {
	if m.capabilityErr != nil {
		return m.capabilityErr
	}
	var buf bytes.Buffer
	if reqBody != nil {
		if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
			return err
		}
		if buf.Len() > maxHTTPMailboxPayloadSize {
			return fmt.Errorf("mailbox HTTP request exceeds %d bytes", maxHTTPMailboxPayloadSize)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, m.endpoint(endpoint), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if endpoint != mailboxHTTPClaimPath {
		if m.capability == "" {
			return errMailboxAuthentication
		}
		req.Header.Set("Authorization", "Bearer "+m.capability)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errRelayPinMismatch) {
			return errRelayPinMismatch
		}
		return errMailboxUnavailable
	}
	defer resp.Body.Close()
	raw, err := readBoundedHTTPBody(resp.Body)
	if err != nil {
		return errMailboxUnavailable
	}
	if resp.StatusCode == http.StatusNoContent {
		if endpoint == mailboxHTTPClaimPath || endpoint == mailboxHTTPWaitPeerPath || endpoint == mailboxHTTPReceivePath {
			return errMailboxPollAgain
		}
		return errMailboxUnavailable
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = decodeStrictBoundedHTTPJSON(bytes.NewReader(raw), &apiErr)
		if resp.StatusCode == http.StatusUnauthorized {
			return errMailboxAuthentication
		}
		return errMailboxUnavailable
	}
	decodeTarget := respBody
	if decodeTarget == nil {
		decodeTarget = new(json.RawMessage)
	}
	if err := decodeStrictBoundedHTTPJSON(bytes.NewReader(raw), decodeTarget); err != nil {
		return errMailboxUnavailable
	}
	return nil
}

// doLongPollJSON transparently resumes bounded server polls until completion or cancellation.
func (m *httpMailbox) doLongPollJSON(ctx context.Context, endpoint string, reqBody any, respBody any) error {
	for {
		err := m.doJSON(ctx, http.MethodPost, endpoint, reqBody, respBody)
		if !errors.Is(err, errMailboxPollAgain) {
			return err
		}
		timer := time.NewTimer(mailboxPollRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// readBoundedHTTPBody reads a mailbox response without allowing unbounded allocation.
func readBoundedHTTPBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxHTTPMailboxPayloadSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxHTTPMailboxPayloadSize {
		return nil, fmt.Errorf("mailbox HTTP response exceeds %d bytes", maxHTTPMailboxPayloadSize)
	}
	return raw, nil
}
