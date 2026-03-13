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
)

// MetricsCollector provides aggregated relay/session metrics out of Redis.
type MetricsCollector struct {
	client *redis.Client
	prefix string
}

// RelayMetrics captures system-wide counters plus representative session slices.
type RelayMetrics struct {
	Generated          time.Time
	TotalSessions      int
	ActiveSessions     int
	WaitingForSender   int
	WaitingForReceiver int
	CompletedSessions  int
	FailedSessions     int
	P2PTransfers       int
	RelayTransfers     int
	Active             []SessionSnapshot
	Recent             []SessionSnapshot
}

// SessionSnapshot summarizes a single rendezvous session for dashboards.
type SessionSnapshot struct {
	Code         string
	Mode         string
	State        string
	Transport    string
	Candidate    string
	Completed    bool
	Error        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ExpiresAt    time.Time
	TTLRemaining time.Duration
	HasSender    bool
	HasReceiver  bool
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
		Generated: time.Now(),
	}
	pattern := fmt.Sprintf("%s:sessions:*", mc.prefix)
	var cursor uint64
	for {
		keys, nextCursor, err := mc.client.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return nil, err
		}
		cursor = nextCursor
		if len(keys) > 0 {
			values, err := mc.client.MGet(ctx, stringSliceToAny(keys)...).Result()
			if err != nil {
				return nil, err
			}
			for _, raw := range values {
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
					if sess.Stats.Completed {
						report.CompletedSessions++
						if strings.EqualFold(sess.Stats.Transport, "relay") {
							report.RelayTransfers++
						} else {
							report.P2PTransfers++
						}
					} else {
						report.FailedSessions++
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
	return report, nil
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

func stringSliceToAny(values []string) []interface{} {
	out := make([]interface{}, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
