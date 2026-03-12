package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/jdefrancesco/internal/rendezvous"
)

type httpMailbox struct {
	client *http.Client
	base   *url.URL
	role   string
	code   string
}

func newHTTPMailbox(addr, role string, timeout time.Duration) mailbox {
	u, _ := url.Parse(addr)
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	return &httpMailbox{
		client: newHTTPClient(timeout),
		base:   u,
		role:   role,
	}
}

func (m *httpMailbox) endpoint(p string) string {
	u := *m.base
	u.Path = path.Join(u.Path, p)
	return u.String()
}

func (m *httpMailbox) Claim(ctx context.Context, requested string) (string, error) {
	req := struct {
		Role      string `json:"role"`
		Requested string `json:"requested,omitempty"`
	}{
		Role:      m.role,
		Requested: requested,
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := m.doJSON(ctx, http.MethodPost, "/v1/claim", req, &resp); err != nil {
		return "", err
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
	return m.doJSON(ctx, http.MethodPost, "/v1/self", req, nil)
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
	if err := m.doJSON(ctx, http.MethodPost, "/v1/wait-peer", req, &resp); err != nil {
		return nil, err
	}
	return &resp.Info, nil
}

func (m *httpMailbox) Send(ctx context.Context, typ string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
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
	return m.doJSON(ctx, http.MethodPost, "/v1/send", req, nil)
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
	if err := m.doJSON(ctx, http.MethodPost, "/v1/receive", req, &resp); err != nil {
		return mailboxMessage{}, err
	}
	var msg mailboxMessage
	msg.Type = resp.Type
	if resp.Body != "" {
		data, err := base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			return mailboxMessage{}, err
		}
		msg.Body = data
	}
	return msg, nil
}

func (m *httpMailbox) Close() error { return nil }

func (m *httpMailbox) ReportStats(ctx context.Context, stats transferStats) error {
	req := struct {
		Role      string `json:"role"`
		Code      string `json:"code"`
		Mode      string `json:"mode"`
		Transport string `json:"transport"`
		Candidate string `json:"candidate"`
		Completed bool   `json:"completed"`
		Error     string `json:"error,omitempty"`
	}{
		Role:      m.role,
		Code:      m.code,
		Mode:      stats.Mode,
		Transport: stats.Transport,
		Candidate: stats.Candidate,
		Completed: stats.Completed,
		Error:     stats.Error,
	}
	return m.doJSON(ctx, http.MethodPost, "/v1/stats", req, nil)
}

func (m *httpMailbox) doJSON(ctx context.Context, method, endpoint string, reqBody any, respBody any) error {
	var buf bytes.Buffer
	if reqBody != nil {
		if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, m.endpoint(endpoint), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&apiErr); err == nil && apiErr.Error != "" {
			return fmt.Errorf("%s", apiErr.Error)
		}
		return fmt.Errorf("relay http error: %s", resp.Status)
	}
	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}
