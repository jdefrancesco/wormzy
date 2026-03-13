package ui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jdefrancesco/internal/transport"
)

// Session holds the static information about the current workflow.
type Session struct {
	Mode        string
	File        string
	Relay       string
	Code        string
	DownloadDir string
	ShowNetwork bool
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
		if msg.Result != nil {
			m.session.Code = msg.Result.Code
			m.result = msg.Result
		}
		switch {
		case msg.Err == nil:
			m.err = nil
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
	b.WriteString(renderHeader())
	b.WriteString("\n")
	b.WriteString(renderSession(m.session))
	b.WriteString("\n")
	if strings.EqualFold(m.session.Mode, "RECV") && !m.done {
		b.WriteString(renderReceivePanel(m.session))
		b.WriteString("\n")
	}
	b.WriteString(renderSteps(m.steps))
	b.WriteString("\n")
	b.WriteString(renderProgress(m.progress))
	if len(m.logs) > 0 {
		b.WriteString("\n")
		b.WriteString(renderLogs(m.logs))
	}
	b.WriteString("\n")
	if m.err != nil {
		b.WriteString(renderIssuePanel(m.err))
		b.WriteString("\n")
	} else if m.done && m.result != nil {
		b.WriteString(renderSuccessPanel(m.result))
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

func renderSession(s Session) string {
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

func renderIssuePanel(err error) string {
	lines := []string{
		issueTitleStyle.Render("Something went wrong"),
		highlightText.Render(err.Error()),
	}
	if tips := suggestionsForError(err); len(tips) > 0 {
		lines = append(lines, "")
		lines = append(lines, subtleStyle.Render("Next steps"))
		for _, tip := range tips {
			lines = append(lines, " • "+tip)
		}
	}
	lines = append(lines, "")
	lines = append(lines, subtleStyle.Render("Press q to exit"))
	return issueBoxStyle.Render(strings.Join(lines, "\n"))
}

func renderSuccessPanel(res *transport.Result) string {
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
	lines = append(lines, fmt.Sprintf("Code   %s", highlightText.Render(orDash(res.Code))))
	lines = append(lines, "")
	lines = append(lines, subtleStyle.Render("Press q to exit"))
	return successBoxStyle.Render(strings.Join(lines, "\n"))
}

func renderReceivePanel(s Session) string {
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
	}
	return bubblegumBoxStyle.Render(strings.Join(lines, "\n"))
}

func suggestionsForError(err error) []string {
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
	bubblegumBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.DoubleBorder()).
				Padding(0, 1).
				BorderForeground(lipgloss.Color("#FF9EC4")).
				Background(lipgloss.Color("#2B1223"))
	bubblegumTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFB3C6"))
	bubblegumAccentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD1DC"))
	bubblegumSubtleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#E0A4C2"))
)
