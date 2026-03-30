package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jdefrancesco/wormzy/internal/transport"
)

func main() {
	var (
		redisURL = flag.String("redis", defaultRedisURL(), "redis URL (rediss://user:pass@host:port)")
		prefix   = flag.String("prefix", "wormzy", "redis key prefix")
		refresh  = flag.Duration("refresh", 5*time.Second, "refresh interval")
	)
	flag.Parse()

	if *redisURL == "" {
		fmt.Fprintln(os.Stderr, "error: redis URL required; pass -redis or set WORMZY_METRICS_REDIS")
		os.Exit(1)
	}

	collector, err := transport.NewMetricsCollector(*redisURL, *prefix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer collector.Close()

	model := newDashboardModel(collector, *refresh)
	if err := tea.NewProgram(model, tea.WithAltScreen()).Start(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultRedisURL() string {
	if v := os.Getenv("WORMZY_METRICS_REDIS"); v != "" {
		return v
	}
	if v := os.Getenv("WORMZY_REDIS_URL"); v != "" {
		return v
	}
	if v := os.Getenv("WORMZY_RELAY_URL"); strings.HasPrefix(strings.ToLower(v), "redis") {
		return v
	}
	return ""
}

type dashboardModel struct {
	collector *transport.MetricsCollector
	refresh   time.Duration

	metrics *transport.RelayMetrics
	err     error
	loading bool

	width  int
	height int
}

func newDashboardModel(collector *transport.MetricsCollector, refresh time.Duration) dashboardModel {
	if refresh <= 0 {
		refresh = 5 * time.Second
	}
	return dashboardModel{
		collector: collector,
		refresh:   refresh,
		loading:   true,
	}
}

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(fetchMetricsCmd(m.collector), tickCmd(m.refresh))
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "r":
			if m.loading {
				return m, nil
			}
			m.loading = true
			return m, fetchMetricsCmd(m.collector)
		}
	case metricsMsg:
		m.metrics = msg.metrics
		m.err = nil
		m.loading = false
	case errMsg:
		m.err = msg.err
		m.loading = false
	case tickMsg:
		cmds := []tea.Cmd{tickCmd(m.refresh)}
		if !m.loading {
			m.loading = true
			cmds = append(cmds, fetchMetricsCmd(m.collector))
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m dashboardModel) View() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("wormzy relay dashboard"))
	b.WriteString("\n")

	switch {
	case m.metrics == nil && m.err != nil:
		b.WriteString(renderErrorPanel(m.err))
	case m.metrics == nil && m.loading:
		b.WriteString(subtleStyle.Render("Collecting metrics from Redis…"))
	case m.metrics == nil:
		b.WriteString(subtleStyle.Render("No data yet. Press r to retry."))
	default:
		b.WriteString(renderSummary(m.metrics, m.loading))
		b.WriteString("\n\n")
		b.WriteString(renderSessionPanels(m.metrics))
		if m.err != nil {
			b.WriteString("\n\n")
			b.WriteString(renderErrorPanel(m.err))
		}
		b.WriteString("\n\n")
		b.WriteString(renderFooter(m.metrics.Generated, m.loading))
	}
	return b.String()
}

type metricsMsg struct {
	metrics *transport.RelayMetrics
}

type errMsg struct {
	err error
}

type tickMsg struct{}

func fetchMetricsCmd(mc *transport.MetricsCollector) tea.Cmd {
	return func() tea.Msg {
		if mc == nil {
			return errMsg{err: fmt.Errorf("metrics collector not configured")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		metrics, err := mc.Collect(ctx)
		if err != nil {
			return errMsg{err: err}
		}
		return metricsMsg{metrics: metrics}
	}
}

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func renderSummary(metrics *transport.RelayMetrics, loading bool) string {
	lines := []string{
		fmt.Sprintf("Total sessions    %s", summaryValue(metrics.TotalSessions)),
		fmt.Sprintf("Active sessions   %s", summaryValue(metrics.ActiveSessions)),
		fmt.Sprintf(" • waiting recv   %s", summaryValue(metrics.WaitingForReceiver)),
		fmt.Sprintf(" • waiting send   %s", summaryValue(metrics.WaitingForSender)),
		fmt.Sprintf("Completed         %s", summaryValue(metrics.CompletedSessions)),
		fmt.Sprintf("Failed            %s", summaryValue(metrics.FailedSessions)),
		fmt.Sprintf("P2P vs Relay      %s / %s", summaryValue(metrics.P2PTransfers), summaryValue(metrics.RelayTransfers)),
	}
	if metrics.TotalBytes > 0 {
		lines = append(lines, fmt.Sprintf("Data transferred  %s", summaryBytes(metrics.TotalBytes)))
	}
	if metrics.AvgDuration > 0 {
		lines = append(lines, fmt.Sprintf("Avg duration      %s", subtleStyle.Render(humanDuration(metrics.AvgDuration))))
	}
	if metrics.AvgThroughputMBps > 0 {
		lines = append(lines, fmt.Sprintf("Avg throughput    %s", subtleStyle.Render(fmt.Sprintf("%.1f MB/s", metrics.AvgThroughputMBps))))
	}
	if loading {
		lines = append(lines, "")
		lines = append(lines, warningStyle.Render("Refreshing…"))
	}
	return bubbleBoxStyle.Render(strings.Join(lines, "\n"))
}

func renderSessionPanels(metrics *transport.RelayMetrics) string {
	active := renderSessionList("Active sessions", metrics.Active, metrics.Generated, true)
	recent := renderSessionList("Recent transfers", metrics.Recent, metrics.Generated, false)
	return lipgloss.JoinVertical(lipgloss.Left, active, recent)
}

func renderSessionList(title string, sessions []transport.SessionSnapshot, ref time.Time, showTTL bool) string {
	rows := []string{
		fmt.Sprintf("%-12s %-10s %-10s %-10s %s", "Code", "State", "Size", "Duration", columnLabel(showTTL)),
	}
	if len(sessions) == 0 {
		rows = append(rows, subtleStyle.Render("no sessions to display"))
	} else {
		for _, sess := range sessions {
			rows = append(rows, renderSessionRow(sess, ref, showTTL))
		}
	}
	body := titleStyle.Render(title) + "\n" + strings.Join(rows, "\n")
	return bubbleBoxStyle.Render(body)
}

func columnLabel(showTTL bool) string {
	if showTTL {
		return "TTL left"
	}
	return "Updated"
}

func renderSessionRow(sess transport.SessionSnapshot, ref time.Time, showTTL bool) string {
	state := prettifyState(sess.State)
	trailing := humanDuration(sessionTrailing(sess, ref, showTTL))
	return fmt.Sprintf(
		"%-12s %-10s %-10s %-10s %s",
		sess.Code,
		stateStyle(state),
		formatBytes(sess.Bytes),
		humanDuration(sess.Duration),
		trailing,
	)
}

func sessionTrailing(sess transport.SessionSnapshot, ref time.Time, showTTL bool) time.Duration {
	if showTTL {
		return sess.TTLRemaining
	}
	if sess.UpdatedAt.IsZero() {
		return 0
	}
	if ref.IsZero() {
		return time.Since(sess.UpdatedAt)
	}
	return ref.Sub(sess.UpdatedAt)
}

func renderErrorPanel(err error) string {
	return errorBoxStyle.Render(errorStyle.Render("Relay metrics error") + "\n" + err.Error())
}

func renderFooter(updated time.Time, loading bool) string {
	status := fmt.Sprintf("Last updated %s", updated.Format(time.RFC3339))
	if loading {
		status += " • refreshing"
	}
	status += " • Press r to refresh, q to exit"
	return subtleStyle.Render(status)
}

func summaryValue(v int) string {
	return summaryValueStyle.Render(fmt.Sprintf("%d", v))
}

func summaryBytes(v int64) string {
	return summaryValueStyle.Render(formatBytes(v))
}

func stateStyle(state string) string {
	switch strings.ToLower(state) {
	case "p2p":
		return successStyle.Render("P2P")
	case "relay":
		return warningStyle.Render("Relay")
	case "failed":
		return errorStyle.Render("Failed")
	default:
		return subtleStyle.Render(state)
	}
}

func prettifyState(state string) string {
	switch strings.ToLower(state) {
	case "p2p":
		return "P2P"
	case "relay":
		return "Relay"
	case "failed":
		return "Failed"
	}
	parts := strings.Fields(state)
	for i, part := range parts {
		if len(part) == 0 {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		days := d / (24 * time.Hour)
		hours := (d % (24 * time.Hour)) / time.Hour
		return fmt.Sprintf("%dd%02dh", days, hours)
	case d >= time.Hour:
		hours := d / time.Hour
		minutes := (d % time.Hour) / time.Minute
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	case d >= time.Minute:
		minutes := d / time.Minute
		seconds := (d % time.Minute) / time.Second
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	default:
		seconds := d / time.Second
		return fmt.Sprintf("%ds", seconds)
	}
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	value := float64(b) / float64(div)
	return fmt.Sprintf("%.1f %ciB", value, "KMGTPE"[exp])
}

var (
	headerStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB3C6")).Bold(true)
	titleStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD1DC")).Bold(true)
	bubbleBoxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).BorderForeground(lipgloss.Color("#FF9EC4"))
	errorBoxStyle     = lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).BorderForeground(lipgloss.Color("#FF4F8B")).Padding(0, 1)
	summaryValueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#5DFFB4")).Bold(true)
	subtleStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#C8B5C9"))
	warningStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF8FA3")).Bold(true)
	errorStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4D6D")).Bold(true)
	successStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#5DFF8D")).Bold(true)
)
