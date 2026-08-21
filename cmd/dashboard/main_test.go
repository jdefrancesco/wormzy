package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jdefrancesco/wormzy/internal/transport"
)

func TestRenderServicePanel_ShowsServerActivity(t *testing.T) {
	metrics := &transport.RelayMetrics{
		RedisLatency: 2 * time.Millisecond,
		Services: []transport.ServiceSnapshot{
			{Name: "mailbox", Online: true, Uptime: time.Hour, Requests: 42, ActiveRequests: 2, RequestErrors: 1},
			{Name: "relay", Online: true, Uptime: 2 * time.Hour, ActiveConnections: 3, WaitingPeers: 1, ActivePairs: 1, BytesRelayed: 8192},
		},
	}

	got := renderServicePanel(metrics)
	for _, want := range []string{"Server status", "mailbox", "relay", "42 requests", "3 connections", "8.0 KiB", "Redis"} {
		if !strings.Contains(got, want) {
			t.Fatalf("service panel missing %q:\n%s", want, got)
		}
	}
}

func TestDashboardModel_RequiresConfirmationForControls(t *testing.T) {
	model := dashboardModel{
		metrics: &transport.RelayMetrics{
			Active: []transport.SessionSnapshot{{ID: "internal-session-id", Code: "m-active01"}},
		},
	}

	updated, _ := model.Update(keyMsg("x"))
	afterTerminate := updated.(dashboardModel)
	if afterTerminate.pending == nil || afterTerminate.pending.kind != controlTerminate ||
		afterTerminate.pending.code != "internal-session-id" || afterTerminate.pending.label != "m-active01" {
		t.Fatalf("terminate action was not staged: %+v", afterTerminate.pending)
	}
	updated, _ = afterTerminate.Update(keyMsg("n"))
	if afterCancel := updated.(dashboardModel); afterCancel.pending != nil {
		t.Fatalf("cancel left pending action: %+v", afterCancel.pending)
	}

	updated, _ = model.Update(keyMsg("d"))
	afterDrain := updated.(dashboardModel)
	if afterDrain.pending == nil || afterDrain.pending.kind != controlDrain || !afterDrain.pending.draining {
		t.Fatalf("drain action was not staged: %+v", afterDrain.pending)
	}
}

func keyMsg(key string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

func TestSafeTerminalText_StripsControlAndFormatRunes(t *testing.T) {
	got := safeTerminalText("ok\x1b[31m\u202ebad")
	if got != "ok[31mbad" {
		t.Fatalf("safeTerminalText = %q; want %q", got, "ok[31mbad")
	}
}

// TestRenderHelpDescribesCurrentCandidates keeps operator guidance aligned with telemetry labels.
func TestRenderHelpDescribesCurrentCandidates(t *testing.T) {
	got := renderHelp()
	for _, want := range []string{"ice-p2p", "ice-relay", "upnp", "normally be P2P"} {
		if !strings.Contains(got, want) {
			t.Fatalf("help missing %q:\n%s", want, got)
		}
	}
}
