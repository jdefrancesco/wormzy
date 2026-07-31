package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultOperationsPrefix = "wormzy"
	serviceHeartbeatTTL     = 15 * time.Second
)

type operatorControlRecord struct {
	Draining    bool   `json:"draining"`
	UpdatedUnix int64  `json:"updated_unix"`
	UpdatedBy   string `json:"updated_by"`
}

type serviceTelemetryRecord struct {
	Name              string `json:"name"`
	StartedUnix       int64  `json:"started_unix"`
	UpdatedUnix       int64  `json:"updated_unix"`
	Draining          bool   `json:"draining"`
	Requests          uint64 `json:"requests,omitempty"`
	RequestErrors     uint64 `json:"request_errors,omitempty"`
	ActiveRequests    int64  `json:"active_requests,omitempty"`
	Connections       uint64 `json:"connections,omitempty"`
	ActiveConnections int64  `json:"active_connections,omitempty"`
	WaitingPeers      int    `json:"waiting_peers,omitempty"`
	ActivePairs       int64  `json:"active_pairs,omitempty"`
	CompletedPairs    uint64 `json:"completed_pairs,omitempty"`
	BytesRelayed      int64  `json:"bytes_relayed,omitempty"`
	Errors            uint64 `json:"errors,omitempty"`
	LastError         string `json:"last_error,omitempty"`
}

// ServiceTelemetry publishes short-lived service heartbeats and reads operator
// controls from the same Redis instance used by the dashboard.
type ServiceTelemetry struct {
	client     *redis.Client
	closeRedis bool
	prefix     string
	name       string

	mu       sync.RWMutex
	record   serviceTelemetryRecord
	draining bool
}

// NewServiceTelemetry creates telemetry for a standalone process such as the
// UDP relay. An empty Redis URL disables telemetry and returns nil.
func NewServiceTelemetry(redisURL, prefix, name string) (*ServiceTelemetry, error) {
	if strings.TrimSpace(redisURL) == "" {
		return nil, nil
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		opts = &redis.Options{Addr: redisURL}
	}
	client := redis.NewClient(opts)
	t := newServiceTelemetryWithClient(client, prefix, name)
	t.closeRedis = true
	return t, nil
}

func newServiceTelemetryWithClient(client *redis.Client, prefix, name string) *ServiceTelemetry {
	if prefix == "" {
		prefix = defaultOperationsPrefix
	}
	now := time.Now()
	return &ServiceTelemetry{
		client: client,
		prefix: prefix,
		name:   name,
		record: serviceTelemetryRecord{
			Name:        name,
			StartedUnix: now.Unix(),
			UpdatedUnix: now.Unix(),
		},
	}
}

func (t *ServiceTelemetry) serviceKey() string {
	return fmt.Sprintf("%s:ops:services:%s", t.prefix, t.name)
}

func operatorControlKey(prefix string) string {
	if prefix == "" {
		prefix = defaultOperationsPrefix
	}
	return fmt.Sprintf("%s:ops:control", prefix)
}

func readOperatorControl(ctx context.Context, client *redis.Client, prefix string) (operatorControlRecord, error) {
	data, err := client.Get(ctx, operatorControlKey(prefix)).Bytes()
	if err == redis.Nil {
		return operatorControlRecord{}, nil
	}
	if err != nil {
		return operatorControlRecord{}, fmt.Errorf("read operator control: %w", err)
	}
	var control operatorControlRecord
	if err := json.Unmarshal(data, &control); err != nil {
		return operatorControlRecord{}, fmt.Errorf("decode operator control: %w", err)
	}
	return control, nil
}

// Run publishes an immediate heartbeat, then refreshes telemetry until ctx is
// canceled. A missing heartbeat is treated as offline by the dashboard.
func (t *ServiceTelemetry) Run(ctx context.Context, interval time.Duration) {
	if t == nil || t.client == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	_ = t.Sync(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = t.client.Del(cleanupCtx, t.serviceKey()).Err()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = t.Sync(ctx)
		}
	}
}

// Sync refreshes operator controls and publishes the current service snapshot.
func (t *ServiceTelemetry) Sync(ctx context.Context) error {
	if t == nil || t.client == nil {
		return nil
	}
	if err := t.RefreshControl(ctx); err != nil {
		return err
	}
	return t.Publish(ctx)
}

// Publish writes a short-lived heartbeat containing counters since process start.
func (t *ServiceTelemetry) Publish(ctx context.Context) error {
	if t == nil || t.client == nil {
		return nil
	}
	t.mu.Lock()
	t.record.UpdatedUnix = time.Now().Unix()
	t.record.Draining = t.draining
	record := t.record
	t.mu.Unlock()
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal %s telemetry: %w", t.name, err)
	}
	if err := t.client.Set(ctx, t.serviceKey(), payload, serviceHeartbeatTTL).Err(); err != nil {
		return fmt.Errorf("publish %s telemetry: %w", t.name, err)
	}
	return nil
}

// RefreshControl loads the latest drain state written by an operator console.
func (t *ServiceTelemetry) RefreshControl(ctx context.Context) error {
	if t == nil || t.client == nil {
		return nil
	}
	control, err := readOperatorControl(ctx, t.client, t.prefix)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.draining = control.Draining
	t.mu.Unlock()
	return nil
}

// Draining reports whether the operator has disabled new sessions.
func (t *ServiceTelemetry) Draining() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.draining
}

func (t *ServiceTelemetry) BeginRequest() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.record.ActiveRequests++
	t.mu.Unlock()
}

func (t *ServiceTelemetry) EndRequest(status int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.record.ActiveRequests > 0 {
		t.record.ActiveRequests--
	}
	t.record.Requests++
	if status >= 400 {
		t.record.RequestErrors++
	}
	t.mu.Unlock()
}

func (t *ServiceTelemetry) ConnectionOpened() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.record.Connections++
	t.record.ActiveConnections++
	t.mu.Unlock()
}

func (t *ServiceTelemetry) ConnectionClosed() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.record.ActiveConnections > 0 {
		t.record.ActiveConnections--
	}
	t.mu.Unlock()
}

func (t *ServiceTelemetry) SetWaitingPeers(waiting int) {
	if t == nil {
		return
	}
	if waiting < 0 {
		waiting = 0
	}
	t.mu.Lock()
	t.record.WaitingPeers = waiting
	t.mu.Unlock()
}

func (t *ServiceTelemetry) PairStarted() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.record.ActivePairs++
	t.mu.Unlock()
}

func (t *ServiceTelemetry) PairFinished() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.record.ActivePairs > 0 {
		t.record.ActivePairs--
	}
	t.record.CompletedPairs++
	t.mu.Unlock()
}

func (t *ServiceTelemetry) AddRelayBytes(n int64) {
	if t == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	t.record.BytesRelayed += n
	t.mu.Unlock()
}

func (t *ServiceTelemetry) RecordError(err error) {
	if t == nil || err == nil {
		return
	}
	t.mu.Lock()
	t.record.Errors++
	message := err.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	t.record.LastError = message
	t.mu.Unlock()
}

// Close closes Redis only when telemetry created its own client.
func (t *ServiceTelemetry) Close() error {
	if t == nil || !t.closeRedis || t.client == nil {
		return nil
	}
	return t.client.Close()
}
