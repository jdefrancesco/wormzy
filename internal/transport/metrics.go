package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultMetricsPrefix = "wormzy"
	maxActiveSessions    = 12
	maxRecentSessions    = 12
	maxRecentFailures    = 8
)

// MetricsCollector provides aggregated relay/session metrics out of Redis.
type MetricsCollector struct {
	client *redis.Client
	prefix string
}

// RelayMetrics captures system-wide counters plus representative session slices.
type RelayMetrics struct {
	Generated          time.Time
	RedisLatency       time.Duration
	Control            OperatorControl
	Services           []ServiceSnapshot
	TotalSessions      int
	ActiveSessions     int
	WaitingForSender   int
	WaitingForReceiver int
	CompletedSessions  int
	FailedSessions     int
	P2PTransfers       int
	RelayTransfers     int
	TotalBytes         int64
	AvgDuration        time.Duration
	AvgThroughputMBps  float64
	DirectOutcomeCount map[string]int
	CandidateCount     map[string]int
	ErrorCount         map[string]int
	Active             []SessionSnapshot
	Recent             []SessionSnapshot
	RecentFailures     []SessionSnapshot
}

// OperatorControl is the global intake state shared by the mailbox, relay, and
// privileged dashboard.
type OperatorControl struct {
	Draining  bool
	UpdatedAt time.Time
	UpdatedBy string
}

// ServiceSnapshot is the last heartbeat and activity counters for one server
// process. Counters cover the current process lifetime.
type ServiceSnapshot struct {
	Name              string
	Online            bool
	Draining          bool
	StartedAt         time.Time
	UpdatedAt         time.Time
	Uptime            time.Duration
	Requests          uint64
	RequestErrors     uint64
	ActiveRequests    int64
	Connections       uint64
	ActiveConnections int64
	WaitingPeers      int
	ActivePairs       int64
	CompletedPairs    uint64
	BytesRelayed      int64
	Errors            uint64
	LastError         string
}

// SessionSnapshot summarizes a single rendezvous session for dashboards.
type SessionSnapshot struct {
	Code          string
	Mode          string
	State         string
	Transport     string
	Candidate     string
	DirectOutcome string
	DirectSummary string
	Bytes         int64
	Duration      time.Duration
	Completed     bool
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     time.Time
	TTLRemaining  time.Duration
	HasSender     bool
	HasReceiver   bool
}

// NewMetricsCollector connects to redisURL and prepares to scan the given prefix.
func NewMetricsCollector(redisURL, prefix string) (*MetricsCollector, error) {
	if redisURL == "" {
		return nil, fmt.Errorf("redis url required for metrics collection")
	}
	if prefix == "" {
		prefix = defaultMetricsPrefix
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		opts = &redis.Options{Addr: redisURL}
	}
	client := redis.NewClient(opts)
	return &MetricsCollector{client: client, prefix: prefix}, nil
}

// Close shuts down the underlying Redis client.
func (mc *MetricsCollector) Close() error {
	if mc == nil || mc.client == nil {
		return nil
	}
	return mc.client.Close()
}

// Collect fetches the latest relay metrics snapshot.
func (mc *MetricsCollector) Collect(ctx context.Context) (*RelayMetrics, error) {
	if mc == nil || mc.client == nil {
		return nil, fmt.Errorf("metrics collector not initialized")
	}
	report := &RelayMetrics{
		Generated:          time.Now(),
		DirectOutcomeCount: make(map[string]int),
		CandidateCount:     make(map[string]int),
		ErrorCount:         make(map[string]int),
	}
	pingStarted := time.Now()
	if err := mc.client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis health: %w", err)
	}
	report.RedisLatency = time.Since(pingStarted)
	control, err := mc.collectControl(ctx)
	if err != nil {
		return nil, err
	}
	report.Control = control
	services, err := mc.collectServices(ctx, report.Generated)
	if err != nil {
		return nil, err
	}
	report.Services = services
	var totalDuration time.Duration
	pattern := fmt.Sprintf("%s:sessions:*", mc.prefix)
	var cursor uint64
	for {
		keys, nextCursor, err := mc.client.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return nil, err
		}
		cursor = nextCursor
		if len(keys) > 0 {
			values, err := mc.client.MGet(ctx, keys...).Result()
			if err != nil {
				return nil, err
			}
			ttls, err := mc.sessionTTLs(ctx, keys)
			if err != nil {
				return nil, err
			}
			for i, raw := range values {
				if raw == nil {
					continue
				}
				data, err := bytesFromInterface(raw)
				if err != nil {
					continue
				}
				var sess rendezvousSession
				if err := json.Unmarshal(data, &sess); err != nil {
					continue
				}
				report.TotalSessions++
				snap := snapshotFromSession(&sess, report.Generated)
				if ttls[i] > 0 {
					snap.TTLRemaining = ttls[i]
					snap.ExpiresAt = report.Generated.Add(ttls[i])
				}
				if sess.Stats == nil {
					report.ActiveSessions++
					if snap.HasSender && !snap.HasReceiver {
						report.WaitingForReceiver++
					}
					if snap.HasReceiver && !snap.HasSender {
						report.WaitingForSender++
					}
					report.Active = append(report.Active, snap)
				} else {
					candidate := normalizeMetricsLabel(sess.Stats.Candidate, "unknown")
					report.CandidateCount[candidate]++
					outcome := normalizeMetricsLabel(sess.Stats.DirectOutcome, "unknown")
					report.DirectOutcomeCount[outcome]++

					if sess.Stats.Completed {
						report.CompletedSessions++
						if strings.EqualFold(sess.Stats.Transport, "relay") {
							report.RelayTransfers++
						} else {
							report.P2PTransfers++
						}
						report.TotalBytes += sess.Stats.Bytes
						totalDuration += time.Duration(sess.Stats.DurationMillis) * time.Millisecond
					} else {
						report.FailedSessions++
						errKey := normalizeMetricsError(sess.Stats.Error)
						report.ErrorCount[errKey]++
						report.RecentFailures = append(report.RecentFailures, snap)
					}
					report.Recent = append(report.Recent, snap)
				}
			}
		}
		if cursor == 0 {
			break
		}
	}
	sort.Slice(report.Active, func(i, j int) bool {
		return report.Active[i].CreatedAt.After(report.Active[j].CreatedAt)
	})
	if len(report.Active) > maxActiveSessions {
		report.Active = report.Active[:maxActiveSessions]
	}
	sort.Slice(report.Recent, func(i, j int) bool {
		return report.Recent[i].UpdatedAt.After(report.Recent[j].UpdatedAt)
	})
	if len(report.Recent) > maxRecentSessions {
		report.Recent = report.Recent[:maxRecentSessions]
	}
	sort.Slice(report.RecentFailures, func(i, j int) bool {
		return report.RecentFailures[i].UpdatedAt.After(report.RecentFailures[j].UpdatedAt)
	})
	if len(report.RecentFailures) > maxRecentFailures {
		report.RecentFailures = report.RecentFailures[:maxRecentFailures]
	}
	if report.CompletedSessions > 0 {
		report.AvgDuration = totalDuration / time.Duration(report.CompletedSessions)
		if report.AvgDuration > 0 {
			avgBytes := float64(report.TotalBytes) / float64(report.CompletedSessions)
			report.AvgThroughputMBps = (avgBytes / report.AvgDuration.Seconds()) / (1024 * 1024)
		}
	}
	return report, nil
}

func (mc *MetricsCollector) controlKey() string {
	return operatorControlKey(mc.prefix)
}

func (mc *MetricsCollector) serviceKey(name string) string {
	return fmt.Sprintf("%s:ops:services:%s", mc.prefix, name)
}

func (mc *MetricsCollector) collectControl(ctx context.Context) (OperatorControl, error) {
	record, err := readOperatorControl(ctx, mc.client, mc.prefix)
	if err != nil {
		return OperatorControl{}, err
	}
	control := OperatorControl{Draining: record.Draining, UpdatedBy: record.UpdatedBy}
	if record.UpdatedUnix > 0 {
		control.UpdatedAt = time.Unix(record.UpdatedUnix, 0)
	}
	return control, nil
}

func (mc *MetricsCollector) collectServices(ctx context.Context, now time.Time) ([]ServiceSnapshot, error) {
	names := []string{"mailbox", "relay"}
	keys := make([]string, 0, len(names))
	for _, name := range names {
		keys = append(keys, mc.serviceKey(name))
	}
	values, err := mc.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read service telemetry: %w", err)
	}
	services := make([]ServiceSnapshot, 0, len(names))
	for i, name := range names {
		snap := ServiceSnapshot{Name: name}
		if values[i] == nil {
			services = append(services, snap)
			continue
		}
		data, err := bytesFromInterface(values[i])
		if err != nil {
			return nil, fmt.Errorf("decode %s telemetry: %w", name, err)
		}
		var record serviceTelemetryRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("decode %s telemetry: %w", name, err)
		}
		snap = serviceSnapshotFromRecord(record, now)
		services = append(services, snap)
	}
	return services, nil
}

func serviceSnapshotFromRecord(record serviceTelemetryRecord, now time.Time) ServiceSnapshot {
	started := time.Unix(record.StartedUnix, 0)
	updated := time.Unix(record.UpdatedUnix, 0)
	uptime := now.Sub(started)
	if uptime < 0 {
		uptime = 0
	}
	return ServiceSnapshot{
		Name:              record.Name,
		Online:            !updated.IsZero() && now.Sub(updated) <= serviceHeartbeatTTL,
		Draining:          record.Draining,
		StartedAt:         started,
		UpdatedAt:         updated,
		Uptime:            uptime,
		Requests:          record.Requests,
		RequestErrors:     record.RequestErrors,
		ActiveRequests:    record.ActiveRequests,
		Connections:       record.Connections,
		ActiveConnections: record.ActiveConnections,
		WaitingPeers:      record.WaitingPeers,
		ActivePairs:       record.ActivePairs,
		CompletedPairs:    record.CompletedPairs,
		BytesRelayed:      record.BytesRelayed,
		Errors:            record.Errors,
		LastError:         record.LastError,
	}
}

func (mc *MetricsCollector) sessionTTLs(ctx context.Context, keys []string) ([]time.Duration, error) {
	pipe := mc.client.Pipeline()
	cmds := make([]*redis.DurationCmd, 0, len(keys))
	for _, key := range keys {
		cmds = append(cmds, pipe.PTTL(ctx, key))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("read session TTLs: %w", err)
	}
	ttls := make([]time.Duration, len(cmds))
	for i, cmd := range cmds {
		ttls[i] = cmd.Val()
	}
	return ttls, nil
}

// SetDraining enables or disables acceptance of new sessions. Redis access is
// the authorization boundary for this operator action.
func (mc *MetricsCollector) SetDraining(ctx context.Context, draining bool) error {
	record := operatorControlRecord{
		Draining:    draining,
		UpdatedUnix: time.Now().Unix(),
		UpdatedBy:   "dashboard",
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal operator control: %w", err)
	}
	if err := mc.client.Set(ctx, mc.controlKey(), payload, 0).Err(); err != nil {
		return fmt.Errorf("set drain state: %w", err)
	}
	return nil
}

// TerminateSession removes one rendezvous session. It cannot terminate an
// already-established direct P2P connection, which no longer traverses Redis.
func (mc *MetricsCollector) TerminateSession(ctx context.Context, code string) error {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 128 || strings.ContainsAny(code, "\r\n") {
		return fmt.Errorf("invalid session code")
	}
	key := fmt.Sprintf("%s:sessions:%s", mc.prefix, code)
	deleted, err := mc.client.Del(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("terminate session %s: %w", code, err)
	}
	if deleted == 0 {
		return fmt.Errorf("terminate session %s: %w", code, errSessionNotFound)
	}
	return nil
}

func normalizeMetricsLabel(v, fallback string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return fallback
	}
	return v
}

func normalizeMetricsError(errMsg string) string {
	msg := strings.TrimSpace(strings.ToLower(errMsg))
	if msg == "" {
		return "unknown"
	}
	switch {
	case strings.Contains(msg, "deadline exceeded"):
		return "deadline exceeded"
	case strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "noise"):
		return "noise error"
	case strings.Contains(msg, "relay"):
		return "relay error"
	case strings.Contains(msg, "no usable transport candidates"):
		return "no candidates"
	}
	if idx := strings.Index(msg, ":"); idx > 0 {
		msg = msg[:idx]
	}
	if len(msg) > 64 {
		msg = msg[:64] + "..."
	}
	return msg
}

func snapshotFromSession(sess *rendezvousSession, now time.Time) SessionSnapshot {
	var created time.Time
	if sess.CreatedUnix > 0 {
		created = time.Unix(sess.CreatedUnix, 0)
	} else {
		created = now
	}
	ttl := time.Duration(sess.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	expires := created.Add(ttl)
	remaining := expires.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	snap := SessionSnapshot{
		Code:         sess.Code,
		CreatedAt:    created,
		ExpiresAt:    expires,
		TTLRemaining: remaining,
		HasSender:    sess.Sender != nil,
		HasReceiver:  sess.Receiver != nil,
	}
	snap.State = sessionStateFromPeers(snap.HasSender, snap.HasReceiver)
	if sess.Stats != nil {
		snap.Mode = sess.Stats.Mode
		snap.Transport = sess.Stats.Transport
		snap.Candidate = sess.Stats.Candidate
		snap.DirectOutcome = sess.Stats.DirectOutcome
		snap.DirectSummary = sess.Stats.DirectSummary
		snap.Bytes = sess.Stats.Bytes
		snap.Duration = time.Duration(sess.Stats.DurationMillis) * time.Millisecond
		snap.Completed = sess.Stats.Completed
		snap.Error = sess.Stats.Error
		if sess.Stats.UpdatedUnix > 0 {
			snap.UpdatedAt = time.Unix(sess.Stats.UpdatedUnix, 0)
		} else {
			snap.UpdatedAt = now
		}
		if sess.Stats.Completed {
			if strings.EqualFold(sess.Stats.Transport, "relay") {
				snap.State = "relay"
			} else {
				snap.State = "p2p"
			}
		} else {
			snap.State = "failed"
		}
	} else {
		snap.UpdatedAt = created
	}
	return snap
}

func sessionStateFromPeers(hasSender, hasReceiver bool) string {
	switch {
	case hasSender && hasReceiver:
		return "negotiating"
	case hasSender:
		return "waiting receiver"
	case hasReceiver:
		return "waiting sender"
	default:
		return "unclaimed"
	}
}

func bytesFromInterface(v interface{}) ([]byte, error) {
	switch val := v.(type) {
	case string:
		return []byte(val), nil
	case []byte:
		return val, nil
	default:
		return nil, fmt.Errorf("unsupported redis type %T", v)
	}
}
