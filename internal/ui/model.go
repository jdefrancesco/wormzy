package ui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jdefrancesco/wormzy/internal/transport"
)

// Session holds the static information about the current workflow.
type Session struct {
	Mode        string
	File        string
	Relay       string
	Code        string
	DownloadDir string
	ShowNetwork bool
	ShowLogs    bool
	AutoExit    bool
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
	result   *transport.Result
}

type step struct {
	Title  string
	Detail string
	Stage  transport.Stage
	State  transport.StageState
}

func (st step) renderDetail() string {
	return st.Detail
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
	// QUIC and Noise are presented as one combined step in the UI.
	index[transport.StageQUIC] = index[transport.StageNoise]
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
		if msg.stage == transport.StageRendezvous && strings.HasPrefix(msg.detail, "code ") {
			m.session.Code = strings.TrimSpace(strings.TrimPrefix(msg.detail, "code "))
		}
		m.progress = progressFromSteps(m.steps)
	case DoneMsg:
		m.done = true
		if msg.Result != nil {
			m.session.Code = msg.Result.Code
			m.result = msg.Result
		}
		switch {
		case msg.Err == nil:
			m.err = nil
			if m.session.AutoExit {
				return m, tea.Quit
			}
			return m, nil
		case errors.Is(msg.Err, context.Canceled):
			m.err = nil
			return m, tea.Quit
		default:
			m.err = msg.Err
		}
	}
	return m, nil
}

func (m Model) View() string {
	var b strings.Builder
	panelWidth := uniformPanelWidth(m.width)
	b.WriteString(renderHeader())
	b.WriteString("\n")
	b.WriteString(renderSession(m.session, panelWidth))
	b.WriteString("\n")
	if strings.EqualFold(m.session.Mode, "RECV") && !m.done {
		b.WriteString(renderReceivePanel(m.session, panelWidth))
		b.WriteString("\n")
	}
	b.WriteString(renderSteps(m.steps, panelWidth))
	b.WriteString("\n")
	b.WriteString(renderProgress(m.progress, panelWidth))
	if m.session.ShowLogs && len(m.logs) > 0 {
		b.WriteString("\n")
		b.WriteString(renderLogs(m.logs, panelWidth))
	}
	b.WriteString("\n")
	if m.err != nil {
		b.WriteString(renderIssuePanel(m.err, m.session.ShowLogs, panelWidth))
		b.WriteString("\n")
	} else if m.done && m.result != nil {
		b.WriteString(renderSuccessPanel(m.result, panelWidth))
		b.WriteString("\n")
	}
	b.WriteString(renderFooter(m.done, m.err))
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

func renderSession(s Session, width int) string {
	rows := []string{
		fmt.Sprintf("Mode   %s", highlightText.Render(s.Mode)),
		fmt.Sprintf("File   %s", highlightText.Render(orDash(s.File))),
	}
	if strings.EqualFold(s.Mode, "RECV") && s.DownloadDir != "" {
		rows = append(rows, fmt.Sprintf("Dest   %s", highlightText.Render(s.DownloadDir)))
	}
	if s.ShowNetwork {
		rows = append(rows, fmt.Sprintf("Relay  %s", highlightText.Render(orDash(s.Relay))))
	}
	if s.Code != "" {
		rows = append(rows, fmt.Sprintf("Code   %s", codeStyle.Render(s.Code)))
	}
	return renderPanel(boxStyle, width, strings.Join(rows, "\n"))
}

func renderSteps(steps []step, width int) string {
	var lines []string
	for _, st := range steps {
		lines = append(lines, fmt.Sprintf("%s %s", stepIcon(st.State), stepTitleStyle.Render(st.Title)))
		if st.Detail != "" {
			lines = append(lines, "   "+subtleStyle.Render(st.renderDetail()))
		}
	}
	return renderPanel(boxStyle, width, strings.Join(lines, "\n"))
}

func renderLogs(logs []string, width int) string {
	if len(logs) == 0 {
		return ""
	}
	return renderPanel(boxStyle, width, "Logs\n"+subtleStyle.Render(strings.Join(logs, "\n")))
}

func renderIssuePanel(err error, showLogs bool, width int) string {
	lines := []string{
		issueTitleStyle.Render("Something went wrong"),
		highlightText.Render(err.Error()),
	}
	if tips := suggestionsForError(err, showLogs); len(tips) > 0 {
		lines = append(lines, "")
		lines = append(lines, subtleStyle.Render("Next steps"))
		for _, tip := range tips {
			lines = append(lines, " • "+tip)
		}
	}
	lines = append(lines, "")
	lines = append(lines, subtleStyle.Render("Press q to exit"))
	return renderPanel(issueBoxStyle, width, strings.Join(lines, "\n"))
}

func renderSuccessPanel(res *transport.Result, width int) string {
	if res == nil {
		return ""
	}
	lines := []string{
		successTitleStyle.Render("Transfer complete"),
		fmt.Sprintf("File   %s", highlightText.Render(orDash(filepath.Base(res.FilePath)))),
	}
	if res.FileSize > 0 {
		lines = append(lines, fmt.Sprintf("Size   %s", highlightText.Render(formatSize(res.FileSize))))
	}
	if res.FileHash != "" {
		lines = append(lines, fmt.Sprintf("Hash   %s", highlightText.Render(res.FileHash)))
	}
	if res.Transport != "" {
		lines = append(lines, fmt.Sprintf("Path   %s (%s)", highlightText.Render(strings.ToUpper(res.Transport)), highlightText.Render(orDash(res.Candidate))))
	}
	lines = append(lines, fmt.Sprintf("Code   %s", codeStyle.Render(orDash(res.Code))))
	lines = append(lines, "")
	lines = append(lines, subtleStyle.Render("Press q to exit"))
	return renderPanel(successBoxStyle, width, strings.Join(lines, "\n"))
}

func renderReceivePanel(s Session, width int) string {
	if !strings.EqualFold(s.Mode, "RECV") {
		return ""
	}
	dest := s.DownloadDir
	if dest == "" {
		dest = "."
	}
	lines := []string{
		bubblegumTitleStyle.Render("Ready to receive"),
		fmt.Sprintf("Saving to %s", bubblegumAccentStyle.Render(dest)),
		"",
		bubblegumSubtleStyle.Render("Next up"),
		" • " + bubblegumAccentStyle.Render("Waiting for the manifest from your peer."),
		" • " + bubblegumAccentStyle.Render("Encrypted channel locks in once the sender connects."),
		" • " + bubblegumAccentStyle.Render("Transfer auto-verifies hashes before finishing."),
		" • " + bubblegumAccentStyle.Render("Sessions idle out after a few minutes to keep things tidy."),
	}
	return renderPanel(bubblegumBoxStyle, width, strings.Join(lines, "\n"))
}

func suggestionsForError(err error, showLogs bool) []string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "network is unreachable"):
		return []string{
			"Host networking blocked UDP; run wormzy outside the sandbox or on a machine with internet access.",
			"Use `-dev-loopback` to simulate transfers on localhost.",
		}
	case strings.Contains(msg, "permission denied"), strings.Contains(msg, "operation not permitted"):
		return []string{
			"OS refused to bind UDP; request the necessary privileges or try again locally.",
			"`-dev-loopback` keeps traffic on 127.0.0.1 for demos.",
		}
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return []string{
			"Timed out waiting for the relay; confirm the `-relay` address and your upstream connectivity.",
		}
	default:
		if !showLogs {
			return []string{
				"Retry with `--logs` to display STUN / relay diagnostics.",
			}
		}
		return []string{
			"Review the log panel above for STUN / relay output before retrying.",
		}
	}
}

func renderFooter(done bool, err error) string {
	switch {
	case err != nil:
		return subtleStyle.Render("Press q to exit once you've captured the issue")
	case done:
		return successStyle.Render("Transfer complete — press q to exit")
	default:
		return subtleStyle.Render("Press q to quit")
	}
}

func renderProgress(p float64, width int) string {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	barWidth := 40
	filled := int(p * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	return renderPanel(boxStyle, width, fmt.Sprintf("Progress\n%s %3.0f%%", accentStyle.Render(bar), p*100))
}

const defaultPanelWidth = 76

func uniformPanelWidth(terminalWidth int) int {
	if terminalWidth <= 0 {
		return defaultPanelWidth
	}
	available := terminalWidth - 2
	if available < 1 {
		return 1
	}
	if available < defaultPanelWidth {
		return available
	}
	return defaultPanelWidth
}

func renderPanel(style lipgloss.Style, width int, content string) string {
	contentWidth := width - style.GetHorizontalFrameSize()
	if contentWidth < 1 {
		contentWidth = 1
	}
	return style.Width(contentWidth).Render(content)
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

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func renderHeader() string {
	return headerStyle.Render("WORMZY • user console")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

var (
	headerStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF5FD2")).MarginBottom(1)
	boxStyle          = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).BorderForeground(lipgloss.Color("#555555"))
	issueBoxStyle     = lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(0, 1).BorderForeground(lipgloss.Color("#FF5F87"))
	successBoxStyle   = lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(0, 1).BorderForeground(lipgloss.Color("#5DFF8D"))
	stepTitleStyle    = lipgloss.NewStyle().Bold(true)
	highlightText     = lipgloss.NewStyle().Foreground(lipgloss.Color("#00D7FF"))
	subtleStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#777777"))
	accentStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#5DFFB4"))
	successStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#5DFF8D"))
	successTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5DFF8D"))
	errorStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87"))
	issueTitleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF5F87"))
	codeStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFD75F"))
	bubblegumBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.DoubleBorder()).
				Padding(0, 1).
				BorderForeground(lipgloss.Color("#FF9EC4")).
				Background(lipgloss.Color("#2B1223"))
	bubblegumTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFB3C6"))
	bubblegumAccentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD1DC"))
	bubblegumSubtleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#E0A4C2"))
)
