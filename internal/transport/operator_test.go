package transport

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestMetricsCollector_ServiceTelemetryAndDrainControl(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()

	telemetry := newServiceTelemetryWithClient(client, "wormzy", "relay")
	telemetry.ConnectionOpened()
	telemetry.SetWaitingPeers(1)
	telemetry.PairStarted()
	telemetry.AddRelayBytes(8192)
	telemetry.RecordError(errors.New("test relay error"))
	if err := telemetry.Publish(ctx); err != nil {
		t.Fatalf("publish telemetry: %v", err)
	}

	collector, err := NewMetricsCollector("redis://"+mini.Addr(), "wormzy")
	if err != nil {
		t.Fatalf("NewMetricsCollector: %v", err)
	}
	defer collector.Close()

	metrics, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	relay := findServiceSnapshot(t, metrics.Services, "relay")
	if !relay.Online {
		t.Fatal("relay should be online")
	}
	if relay.ActiveConnections != 1 || relay.WaitingPeers != 1 || relay.ActivePairs != 1 {
		t.Fatalf("unexpected relay activity: %+v", relay)
	}
	if relay.BytesRelayed != 8192 || relay.Errors != 1 {
		t.Fatalf("unexpected relay counters: %+v", relay)
	}
	mailbox := findServiceSnapshot(t, metrics.Services, "mailbox")
	if mailbox.Online {
		t.Fatal("mailbox should be offline without a heartbeat")
	}
	if metrics.RedisLatency <= 0 {
		t.Fatalf("redis latency not recorded: %s", metrics.RedisLatency)
	}

	if err := collector.SetDraining(ctx, true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	if err := telemetry.RefreshControl(ctx); err != nil {
		t.Fatalf("RefreshControl: %v", err)
	}
	if !telemetry.Draining() {
		t.Fatal("telemetry did not observe drain control")
	}

	metrics, err = collector.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect draining: %v", err)
	}
	if !metrics.Control.Draining || metrics.Control.UpdatedBy != "dashboard" {
		t.Fatalf("unexpected control state: %+v", metrics.Control)
	}
}

// TestMetricsCollector_TerminateSession verifies operator deletion releases
// both session storage and global admission capacity.
func TestMetricsCollector_TerminateSession(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	store := newSessionStore(client, 10*time.Minute, mailboxV2StorePrefix)
	sessionID := sessionStoreTestID(0x54)
	if _, err := store.registerSender(ctx, sessionID, sessionStoreTestCapability(t, 0x33)); err != nil {
		t.Fatalf("register sender: %v", err)
	}

	collector, err := NewMetricsCollector("redis://"+mini.Addr(), "wormzy")
	if err != nil {
		t.Fatalf("NewMetricsCollector: %v", err)
	}
	defer collector.Close()
	if err := collector.TerminateSession(ctx, sessionID); err != nil {
		t.Fatalf("TerminateSession: %v", err)
	}
	if _, err := store.load(ctx, sessionID); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("load after terminate = %v; want %v", err, errSessionNotFound)
	}
	if _, err := client.ZScore(ctx, store.activeSessionsKey(), sessionID).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("active-session index after terminate = %v; want %v", err, redis.Nil)
	}
}

func TestRedisMailbox_DrainRejectsDirectSender(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	collector, err := NewMetricsCollector("redis://"+mini.Addr(), "wormzy")
	if err != nil {
		t.Fatalf("NewMetricsCollector: %v", err)
	}
	defer collector.Close()
	if err := collector.SetDraining(ctx, true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}

	mailbox, err := newRedisMailboxWithClient(client, time.Minute, "send", nil)
	if err != nil {
		t.Fatalf("newRedisMailboxWithClient: %v", err)
	}
	if _, err := mailbox.Claim(ctx, sessionStoreTestID(0x55)); !errors.Is(err, errServiceDraining) {
		t.Fatalf("direct sender claim = %v; want %v", err, errServiceDraining)
	}
}

func TestMetricsCollector_UsesRedisSessionTTL(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	sessionID := sessionStoreTestID(0x56)
	sess := newSession(sessionID, 10*time.Minute)
	sess.CreatedUnix = time.Now().Add(-9 * time.Minute).Unix()
	sess.Sender = &sessionPeer{Role: "send"}
	payload, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := client.Set(ctx, mailboxV2StorePrefix+":sessions:"+sessionID, payload, 10*time.Minute).Err(); err != nil {
		t.Fatalf("set session: %v", err)
	}

	collector, err := NewMetricsCollector("redis://"+mini.Addr(), "wormzy")
	if err != nil {
		t.Fatalf("NewMetricsCollector: %v", err)
	}
	defer collector.Close()
	metrics, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(metrics.Active) != 1 {
		t.Fatalf("active sessions = %d; want 1", len(metrics.Active))
	}
	if metrics.Active[0].TTLRemaining < 9*time.Minute {
		t.Fatalf("TTL remaining = %s; want Redis TTL near 10m", metrics.Active[0].TTLRemaining)
	}
	if metrics.Active[0].ID != sessionID || metrics.Active[0].Code != mailboxSessionAlias(sessionID) {
		t.Fatalf("session identity leaked or lost: %+v", metrics.Active[0])
	}
}

func TestServiceTelemetry_ConcurrentCounters(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	telemetry := newServiceTelemetryWithClient(client, "wormzy", "relay")

	const workers = 32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			telemetry.ConnectionOpened()
			telemetry.PairStarted()
			telemetry.AddRelayBytes(1024)
			telemetry.PairFinished()
			telemetry.ConnectionClosed()
		}()
	}
	wg.Wait()
	if err := telemetry.Publish(ctx); err != nil {
		t.Fatalf("publish telemetry: %v", err)
	}

	collector, err := NewMetricsCollector("redis://"+mini.Addr(), "wormzy")
	if err != nil {
		t.Fatalf("NewMetricsCollector: %v", err)
	}
	defer collector.Close()
	metrics, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	relay := findServiceSnapshot(t, metrics.Services, "relay")
	if relay.Connections != workers || relay.ActiveConnections != 0 {
		t.Fatalf("unexpected connection counters: %+v", relay)
	}
	if relay.CompletedPairs != workers || relay.ActivePairs != 0 || relay.BytesRelayed != workers*1024 {
		t.Fatalf("unexpected pair counters: %+v", relay)
	}
}

// TestMetricsAggregationSaturates verifies malformed persisted counters cannot
// wrap dashboard byte or duration totals into negative values.
func TestMetricsAggregationSaturates(t *testing.T) {
	if got := saturatingMetricsAdd(math.MaxInt64-2, 10); got != math.MaxInt64 {
		t.Fatalf("saturating add = %d; want %d", got, int64(math.MaxInt64))
	}
	if got := saturatingMetricsAdd(12, -1); got != 12 {
		t.Fatalf("negative addition changed total to %d", got)
	}
	if got := metricsDurationFromMillis(math.MaxInt64); got != time.Duration(math.MaxInt64) {
		t.Fatalf("duration conversion = %s; want saturation", got)
	}
}

// findServiceSnapshot returns the named service or fails the calling test.
func findServiceSnapshot(t *testing.T, services []ServiceSnapshot, name string) ServiceSnapshot {
	t.Helper()
	for _, service := range services {
		if service.Name == name {
			return service
		}
	}
	t.Fatalf("service %q not found in %+v", name, services)
	return ServiceSnapshot{}
}
