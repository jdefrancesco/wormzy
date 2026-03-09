package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jdefrancesco/internal/transport"
)

// Session holds the static information about the current workflow.
type Session struct {
	Mode  string
	File  string
	Relay string
	Code  string
}

// DoneMsg notifies the UI that the transport run finished.
type DoneMsg struct {
	Result *transport.Result
	Err    error
}

type logMsg struct {
	text string
}

type stageMsg struct {
	stage  transport.Stage
	state  transport.StageState
	detail string
}

// Model implements the Bubble Tea UI.
type Model struct {
	session Session
	steps   []step
	index   map[transport.Stage]int
	logs    []string

	width    int
	height   int
	progress float64
	err      error
	done     bool
}

type step struct {
	Title  string
	Detail string
	Stage  transport.Stage
	State  transport.StageState
}

// NewModel returns a Bubble Tea model wired for the Wormzy workflow.
func NewModel(session Session) Model {
	steps := []step{
		{Title: "STUN discovery", Detail: "probing reflexive address", Stage: transport.StageSTUN},
		{Title: "Rendezvous", Detail: "dialing relay", Stage: transport.StageRendezvous},
		{Title: "Noise + QUIC", Detail: "spinning up tunnel", Stage: transport.StageNoise},
		{Title: "Transfer", Detail: "standing by", Stage: transport.StageTransfer},
	}
	index := make(map[transport.Stage]int)
	for i, st := range steps {
		index[st.Stage] = i
	}
	return Model{
		session:  session,
		steps:    steps,
		index:    index,
		logs:     []string{},
		progress: 0.05,
	}
}

func (m Model) Init() tea.Cmd {
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		}
	case logMsg:
		m.logs = append(m.logs, msg.text)
		if len(m.logs) > 6 {
			m.logs = m.logs[len(m.logs)-6:]
		}
	case stageMsg:
		if idx, ok := m.index[msg.stage]; ok {
			m.steps[idx].State = msg.state
			if msg.detail != "" {
				m.steps[idx].Detail = msg.detail
			}
		}
		m.progress = progressFromSteps(m.steps)
	case DoneMsg:
		m.done = true
		m.err = msg.Err
		if msg.Result != nil {
			m.session.Code = msg.Result.Code
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) View() string {
	var b strings.Builder
	b.WriteString(renderHeader())
	b.WriteString("\n")
	b.WriteString(renderSession(m.session))
	b.WriteString("\n")
	b.WriteString(renderSteps(m.steps))
	b.WriteString("\n")
	b.WriteString(renderProgress(m.progress))
	if len(m.logs) > 0 {
		b.WriteString("\n")
		b.WriteString(renderLogs(m.logs))
	}
	b.WriteString("\n")
	if m.err != nil {
		b.WriteString(errorStyle.Render("⚠ " + m.err.Error()))
	} else if m.done {
		b.WriteString(successStyle.Render("Transfer complete — press q to exit"))
	} else {
		b.WriteString(subtleStyle.Render("Press q to quit"))
	}
	b.WriteString("\n")
	return b.String()
}

func progressFromSteps(steps []step) float64 {
	if len(steps) == 0 {
		return 0
	}
	var score float64
	for _, step := range steps {
		switch step.State {
		case transport.StageStateDone:
			score += 1.0
		case transport.StageStateRunning:
			score += 0.6
		case transport.StageStatePending:
			score += 0.1
		case transport.StageStateError:
			return 0
		}
	}
	return score / float64(len(steps))
}

func renderSession(s Session) string {
	rows := []string{
		fmt.Sprintf("Mode   %s", highlightText.Render(s.Mode)),
		fmt.Sprintf("File   %s", highlightText.Render(orDash(s.File))),
		fmt.Sprintf("Relay  %s", highlightText.Render(s.Relay)),
	}
	if s.Code != "" {
		rows = append(rows, fmt.Sprintf("Code   %s", highlightText.Render(s.Code)))
	}
	return boxStyle.Render(strings.Join(rows, "\n"))
}

func renderSteps(steps []step) string {
	var lines []string
	for _, st := range steps {
		lines = append(lines, fmt.Sprintf("%s %s", stepIcon(st.State), stepTitleStyle.Render(st.Title)))
		if st.Detail != "" {
			lines = append(lines, "   "+subtleStyle.Render(st.Detail))
		}
	}
	return boxStyle.Render(strings.Join(lines, "\n"))
}

func renderLogs(logs []string) string {
	return boxStyle.Render("Logs\n" + subtleStyle.Render(strings.Join(logs, "\n")))
}

func renderProgress(p float64) string {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	width := 40
	filled := int(p * float64(width))
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return boxStyle.Render(fmt.Sprintf("Progress\n%s %3.0f%%", accentStyle.Render(bar), p*100))
}

func renderHeader() string {
	return headerStyle.Render("wormzy • bubbletea console")
}

func stepIcon(state transport.StageState) string {
	switch state {
	case transport.StageStateDone:
		return successStyle.Render("●")
	case transport.StageStateRunning:
		return accentStyle.Render("●")
	case transport.StageStateError:
		return errorStyle.Render("×")
	default:
		return subtleStyle.Render("○")
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

var (
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF5FD2")).MarginBottom(1)
	boxStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).BorderForeground(lipgloss.Color("#555555"))
	stepTitleStyle = lipgloss.NewStyle().Bold(true)
	highlightText = lipgloss.NewStyle().Foreground(lipgloss.Color("#00D7FF"))
	subtleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#777777"))
	accentStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#5DFFB4"))
	successStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#5DFF8D"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87"))
)
