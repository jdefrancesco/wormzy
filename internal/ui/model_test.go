package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/jdefrancesco/wormzy/internal/transport"
)

func TestModel_LogsRequireOptIn(t *testing.T) {
	tests := []struct {
		name     string
		showLogs bool
		wantLogs bool
	}{
		{name: "hidden by default"},
		{name: "shown when enabled", showLogs: true, wantLogs: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewModel(Session{Mode: "SEND", File: "payload.bin", ShowLogs: tt.showLogs})
			model = updateModel(t, model, logMsg{text: "network detail"})
			got := ansi.Strip(model.View())
			hasLogs := strings.Contains(got, "│ Logs")
			if hasLogs != tt.wantLogs {
				t.Fatalf("log panel visibility = %t; want %t\nview:\n%s", hasLogs, tt.wantLogs, got)
			}
		})
	}
}

func TestModel_SendShowsCodeState(t *testing.T) {
	model := NewModel(Session{Mode: "SEND", File: "payload.bin"})
	initialView := ansi.Strip(model.View())
	if !strings.Contains(initialView, "Code   waiting for pairing code") {
		t.Fatalf("expected sender to show the pending code state\nview:\n%s", initialView)
	}

	model = updateModel(t, model, stageMsg{
		stage:  transport.StageRendezvous,
		state:  transport.StageStateRunning,
		detail: "code fresh-code",
	})
	updatedView := ansi.Strip(model.View())
	if !strings.Contains(updatedView, "Code   fresh-code") {
		t.Fatalf("expected sender to show the assigned code\nview:\n%s", updatedView)
	}
	if strings.Contains(updatedView, "waiting for pairing code") {
		t.Fatalf("expected assigned code to replace pending state\nview:\n%s", updatedView)
	}
}

func TestModel_AllPanelBordersHaveEqualWidth(t *testing.T) {
	tests := []struct {
		name  string
		model Model
	}{
		{
			name: "send success with logs",
			model: NewModel(Session{
				Mode:     "SEND",
				File:     "payload.bin",
				Code:     "test-code",
				ShowLogs: true,
			}),
		},
		{
			name: "receive waiting panel",
			model: NewModel(Session{
				Mode:        "RECV",
				File:        "waiting for manifest",
				DownloadDir: "/tmp/wormzy",
				Code:        "test-code",
			}),
		},
		{
			name:  "error panel",
			model: NewModel(Session{Mode: "SEND", File: "payload.bin"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := updateModel(t, tt.model, tea.WindowSizeMsg{Width: 90, Height: 40})
			switch tt.name {
			case "send success with logs":
				model = updateModel(t, model, logMsg{text: "selected candidate pair"})
				model = updateModel(t, model, DoneMsg{Result: &transport.Result{
					FilePath:  "payload.bin",
					FileSize:  1024,
					FileHash:  strings.Repeat("a", 64),
					Transport: "p2p",
					Candidate: "ice-p2p",
					Code:      "test-code",
				}})
			case "error panel":
				model = updateModel(t, model, DoneMsg{Err: errors.New("test failure")})
			}
			assertUniformPanelWidths(t, model.View())
		})
	}
}

func updateModel(t *testing.T, model Model, msg tea.Msg) Model {
	t.Helper()
	updated, _ := model.Update(msg)
	got, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update returned %T; want ui.Model", updated)
	}
	return got
}

func assertUniformPanelWidths(t *testing.T, view string) {
	t.Helper()
	var widths []int
	for _, line := range strings.Split(view, "\n") {
		plain := ansi.Strip(line)
		if plain == "" {
			continue
		}
		switch []rune(plain)[0] {
		case '╭', '╰', '╔', '╚':
			widths = append(widths, len([]rune(plain)))
		}
	}
	if len(widths) < 4 {
		t.Fatalf("found only %d panel border lines in view:\n%s", len(widths), ansi.Strip(view))
	}
	for _, width := range widths[1:] {
		if width != widths[0] {
			t.Fatalf("panel border widths differ: %v\nview:\n%s", widths, ansi.Strip(view))
		}
	}
}
