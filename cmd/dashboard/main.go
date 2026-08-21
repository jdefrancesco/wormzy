package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jdefrancesco/wormzy/internal/buildinfo"
	"github.com/jdefrancesco/wormzy/internal/transport"
)

func main() {
	var (
		redisURL = flag.String("redis", defaultRedisURL(), "redis URL (rediss://user:pass@host:port)")
		prefix   = flag.String("prefix", "wormzy", "redis key prefix")
		refresh  = flag.Duration("refresh", 5*time.Second, "refresh interval")
		version  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.Current().Format("dashboard"))
		return
	}

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
	if _, err := tea.NewProgram(model, tea.WithAltScreen()).Run(); err != nil {
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

	width    int
	height   int
	verbose  bool
	showHelp bool

	selectedSession int
	pending         *controlAction
	controlBusy     bool
	notice          string
}

type controlKind string

const (
	controlDrain     controlKind = "drain"
	controlTerminate controlKind = "terminate"
)

type controlAction struct {
	kind     controlKind
	code     string
	draining bool
}

func newDashboardModel(collector *transport.MetricsCollector, refresh time.Duration) dashboardModel {
	if refresh <= 0 {
		refresh = 5 * time.Second
	}
	return dashboardModel{
		collector: collector,
		refresh:   refresh,
		loading:   true,
		verbose:   true, // Start in verbose mode
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
		key := msg.String()
		if key == "ctrl+c" || key == "q" {
			return m, tea.Quit
		}
		if m.pending != nil {
			switch key {
			case "y":
				action := *m.pending
				m.pending = nil
				m.controlBusy = true
				m.notice = "Applying operator action…"
				return m, executeControlCmd(m.collector, action)
			case "n", "esc":
				m.pending = nil
				m.notice = "Operator action canceled"
			}
			return m, nil
		}
		if m.controlBusy {
			return m, nil
		}
		switch key {
		case "r":
			if m.loading {
				return m, nil
			}
			m.loading = true
			return m, fetchMetricsCmd(m.collector)
		case "v":
			m.verbose = !m.verbose
		case "h", "?":
			m.showHelp = !m.showHelp
		case "j", "down":
			if m.metrics != nil && m.selectedSession+1 < len(m.metrics.Active) {
				m.selectedSession++
			}
		case "k", "up":
			if m.selectedSession > 0 {
				m.selectedSession--
			}
		case "x":
			if m.metrics == nil || len(m.metrics.Active) == 0 {
				m.notice = "No unresolved session selected"
				break
			}
			m.pending = &controlAction{
				kind: controlTerminate,
				code: m.metrics.Active[m.selectedSession].Code,
			}
		case "d":
			if m.metrics == nil {
				m.notice = "Control state is not loaded"
				break
			}
			m.pending = &controlAction{
				kind:     controlDrain,
				draining: !m.metrics.Control.Draining,
			}
		}
	case metricsMsg:
		m.metrics = msg.metrics
		m.err = nil
		m.loading = false
		if len(m.metrics.Active) == 0 {
			m.selectedSession = 0
		} else if m.selectedSession >= len(m.metrics.Active) {
			m.selectedSession = len(m.metrics.Active) - 1
		}
	case errMsg:
		m.err = msg.err
		m.loading = false
	case controlMsg:
		m.controlBusy = false
		if msg.err != nil {
			m.err = msg.err
			m.notice = "Operator action failed"
			return m, nil
		}
		m.notice = msg.message
		m.loading = true
		return m, fetchMetricsCmd(m.collector)
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
	b.WriteString(headerStyle.Render("wormzy operator console"))
	b.WriteString("\n")

	switch {
	case m.metrics == nil && m.err != nil:
		b.WriteString(renderErrorPanel(m.err))
	case m.metrics == nil && m.loading:
		b.WriteString(subtleStyle.Render("Collecting metrics from Redis…"))
	case m.metrics == nil:
		b.WriteString(subtleStyle.Render("No data yet. Press r to retry."))
	default:
		if m.showHelp {
			b.WriteString(renderHelp())
		} else {
			b.WriteString(renderServicePanel(m.metrics))
			b.WriteString("\n\n")
			b.WriteString(renderSummary(m.metrics, m.loading, m.verbose))
			b.WriteString("\n\n")
			b.WriteString(renderSessionPanels(m.metrics, m.verbose, m.selectedSession))
			b.WriteString("\n\n")
			b.WriteString(renderControlPanel(m.metrics, m.pending, m.controlBusy, m.notice, m.selectedSession))
			if m.err != nil {
				b.WriteString("\n\n")
				b.WriteString(renderErrorPanel(m.err))
			}
			b.WriteString("\n\n")
			b.WriteString(renderFooter(m.metrics.Generated, m.loading, m.verbose))
		}
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

type controlMsg struct {
	message string
	err     error
}

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

func executeControlCmd(mc *transport.MetricsCollector, action controlAction) tea.Cmd {
	return func() tea.Msg {
		if mc == nil {
			return controlMsg{err: fmt.Errorf("operator controls are not configured")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		switch action.kind {
		case controlDrain:
			if err := mc.SetDraining(ctx, action.draining); err != nil {
				return controlMsg{err: err}
			}
			if action.draining {
				return controlMsg{message: "New sessions are now draining"}
			}
			return controlMsg{message: "New sessions are now accepted"}
		case controlTerminate:
			if err := mc.TerminateSession(ctx, action.code); err != nil {
				return controlMsg{err: err}
			}
			return controlMsg{message: fmt.Sprintf("Session %s removed", action.code)}
		default:
			return controlMsg{err: fmt.Errorf("unknown operator action %q", action.kind)}
		}
	}
}

func renderServicePanel(metrics *transport.RelayMetrics) string {
	intake := successStyle.Render("ACCEPTING")
	if metrics.Control.Draining {
		intake = warningStyle.Render("DRAINING")
	}
	lines := []string{
		titleStyle.Render("Server status"),
		fmt.Sprintf("Intake  %s    Redis  %s", intake, subtleStyle.Render(metrics.RedisLatency.Round(time.Microsecond).String())),
		"",
	}
	for _, service := range metrics.Services {
		serviceName := safeTerminalText(service.Name)
		status := errorStyle.Render("no heartbeat")
		if service.Online {
			status = successStyle.Render("online")
		}
		switch serviceName {
		case "mailbox":
			lines = append(lines, fmt.Sprintf(
				"%-8s %-14s uptime %s",
				serviceName,
				status,
				humanDuration(service.Uptime),
			))
			lines = append(lines, fmt.Sprintf(
				"  %d requests • %d active • %d errors",
				service.Requests,
				service.ActiveRequests,
				service.RequestErrors,
			))
		case "relay":
			lines = append(lines, fmt.Sprintf(
				"%-8s %-14s uptime %s",
				serviceName,
				status,
				humanDuration(service.Uptime),
			))
			lines = append(lines, fmt.Sprintf(
				"  %d connections active (%d total) • %d waiting • %d pairs active (%d done)",
				service.ActiveConnections,
				service.Connections,
				service.WaitingPeers,
				service.ActivePairs,
				service.CompletedPairs,
			))
			lines = append(lines, fmt.Sprintf(
				"  %s relayed • %d errors",
				formatBytes(service.BytesRelayed),
				service.Errors,
			))
		default:
			lines = append(lines, fmt.Sprintf("%-8s %-14s uptime %s", serviceName, status, humanDuration(service.Uptime)))
		}
		if service.LastError != "" {
			lines = append(lines, "         last error: "+errorStyle.Render(truncateMiddle(safeTerminalText(service.LastError), 96)))
		}
	}
	return bubbleBoxStyle.Render(strings.Join(lines, "\n"))
}

func renderSummary(metrics *transport.RelayMetrics, loading bool, verbose bool) string {
	var lines []string

	// Calculate P2P success rate
	totalCompleted := metrics.P2PTransfers + metrics.RelayTransfers
	var p2pRate float64
	if totalCompleted > 0 {
		p2pRate = float64(metrics.P2PTransfers) / float64(totalCompleted) * 100
	}

	// Header with P2P rate indicator
	p2pIndicator := renderP2PIndicator(p2pRate, totalCompleted)
	lines = append(lines, titleStyle.Render("Transfer telemetry (current TTL window)"))
	lines = append(lines, "")
	lines = append(lines, p2pIndicator)
	lines = append(lines, "")

	// Core metrics
	lines = append(lines, fmt.Sprintf("Total sessions    %s", summaryValue(metrics.TotalSessions)))
	lines = append(lines, fmt.Sprintf("Active sessions   %s", summaryValue(metrics.ActiveSessions)))

	if verbose {
		lines = append(lines, fmt.Sprintf(" • waiting recv   %s", summaryValue(metrics.WaitingForReceiver)))
		lines = append(lines, fmt.Sprintf(" • waiting send   %s", summaryValue(metrics.WaitingForSender)))
	}

	lines = append(lines, fmt.Sprintf("Completed         %s", summaryValue(metrics.CompletedSessions)))
	lines = append(lines, fmt.Sprintf("Failed            %s", summaryValue(metrics.FailedSessions)))

	// P2P vs Relay with percentage
	if totalCompleted > 0 {
		lines = append(lines, fmt.Sprintf("P2P transfers     %s (%.1f%%)",
			summaryValue(metrics.P2PTransfers), p2pRate))
		lines = append(lines, fmt.Sprintf("Relay transfers   %s (%.1f%%)",
			summaryValue(metrics.RelayTransfers), 100-p2pRate))
	} else {
		lines = append(lines, fmt.Sprintf("P2P vs Relay      %s / %s",
			summaryValue(metrics.P2PTransfers), summaryValue(metrics.RelayTransfers)))
	}

	if metrics.TotalBytes > 0 {
		lines = append(lines, fmt.Sprintf("Data transferred  %s", summaryBytes(metrics.TotalBytes)))
	}

	if verbose {
		if metrics.AvgDuration > 0 {
			lines = append(lines, fmt.Sprintf("Avg duration      %s", subtleStyle.Render(humanDuration(metrics.AvgDuration))))
		}
		if metrics.AvgThroughputMBps > 0 {
			lines = append(lines, fmt.Sprintf("Avg throughput    %s", subtleStyle.Render(fmt.Sprintf("%.1f MB/s", metrics.AvgThroughputMBps))))
		}
	}

	if loading {
		lines = append(lines, "")
		lines = append(lines, warningStyle.Render("Refreshing…"))
	}

	return bubbleBoxStyle.Render(strings.Join(lines, "\n"))
}

func renderP2PIndicator(rate float64, total int) string {
	if total == 0 {
		return subtleStyle.Render("No completed transfers yet")
	}

	var indicator, label, color string
	switch {
	case rate >= 80:
		indicator = "🟢 EXCELLENT"
		label = "P2P working great!"
		color = "#5DFF8D"
	case rate >= 70:
		indicator = "🟢 GOOD"
		label = "P2P success rate healthy"
		color = "#5DFF8D"
	case rate >= 50:
		indicator = "🟡 FAIR"
		label = "Room for P2P improvement"
		color = "#FF8FA3"
	case rate >= 30:
		indicator = "🟠 POOR"
		label = "P2P struggling - check logs"
		color = "#FF8FA3"
	default:
		indicator = "🔴 CRITICAL"
		label = "P2P mostly failing - investigate"
		color = "#FF4D6D"
	}

	styleColor := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true)
	return fmt.Sprintf("%s  %.1f%% P2P  %s",
		styleColor.Render(indicator),
		rate,
		subtleStyle.Render(fmt.Sprintf("(%s)", label)))
}

func renderSessionPanels(metrics *transport.RelayMetrics, verbose bool, selected int) string {
	active := renderSessionList("Live / unresolved sessions", metrics.Active, metrics.Generated, true, verbose, selected)
	recent := renderSessionList("Recent activity", metrics.Recent, metrics.Generated, false, verbose, -1)
	debug := renderDebugPanel(metrics, verbose)
	return lipgloss.JoinVertical(lipgloss.Left, active, recent, debug)
}

func renderSessionList(title string, sessions []transport.SessionSnapshot, ref time.Time, showTTL bool, verbose bool, selected int) string {
	var rows []string
	if verbose {
		rows = []string{
			"  " + fmt.Sprintf("%-12s %-12s %-10s %-10s %-12s %s",
				"Code", "State", "Size", "Duration", "Candidate", columnLabel(showTTL)),
		}
	} else {
		rows = []string{
			"  " + fmt.Sprintf("%-12s %-10s %-10s %-10s %s",
				"Code", "State", "Size", "Duration", columnLabel(showTTL)),
		}
	}

	if len(sessions) == 0 {
		rows = append(rows, subtleStyle.Render("no sessions to display"))
	} else {
		for i, sess := range sessions {
			marker := "  "
			if i == selected {
				marker = successStyle.Render("› ")
			}
			rows = append(rows, marker+renderSessionRow(sess, ref, showTTL, verbose))
		}
	}
	body := titleStyle.Render(title) + "\n" + strings.Join(rows, "\n")
	return bubbleBoxStyle.Render(body)
}

func renderControlPanel(metrics *transport.RelayMetrics, pending *controlAction, busy bool, notice string, selected int) string {
	intake := successStyle.Render("accepting new sessions")
	if metrics.Control.Draining {
		intake = warningStyle.Render("draining new sessions")
	}
	selectedCode := "none"
	if len(metrics.Active) > 0 && selected >= 0 && selected < len(metrics.Active) {
		selectedCode = truncateMiddle(safeTerminalText(metrics.Active[selected].Code), 32)
	}
	lines := []string{
		titleStyle.Render("Operator controls"),
		fmt.Sprintf("Intake: %s    Selected: %s", intake, selectedCode),
		"j/k select • x terminate selected session • d toggle drain mode",
	}
	if busy {
		lines = append(lines, warningStyle.Render("Applying operator action…"))
	} else if pending != nil {
		var prompt string
		switch pending.kind {
		case controlTerminate:
			prompt = fmt.Sprintf("Remove session %s? This cannot stop an established P2P connection.", truncateMiddle(safeTerminalText(pending.code), 32))
		case controlDrain:
			if pending.draining {
				prompt = "Stop accepting new sessions? Existing transfers are left alone."
			} else {
				prompt = "Resume accepting new sessions?"
			}
		}
		lines = append(lines, warningStyle.Render(prompt+"  y confirm • n cancel"))
	} else if notice != "" {
		lines = append(lines, subtleStyle.Render(safeTerminalText(notice)))
	}
	return bubbleBoxStyle.Render(strings.Join(lines, "\n"))
}

func columnLabel(showTTL bool) string {
	if showTTL {
		return "TTL left"
	}
	return "Updated"
}

func renderSessionRow(sess transport.SessionSnapshot, ref time.Time, showTTL bool, verbose bool) string {
	code := truncateMiddle(safeTerminalText(sess.Code), 24)
	state := prettifyState(safeTerminalText(sess.State))
	trailing := humanDuration(sessionTrailing(sess, ref, showTTL))

	if verbose {
		candidate := truncateMiddle(safeTerminalText(sess.Candidate), 24)
		if candidate == "" {
			candidate = "-"
		}
		return fmt.Sprintf(
			"%-12s %-12s %-10s %-10s %-12s %s",
			code,
			stateStyle(state),
			formatBytes(sess.Bytes),
			humanDuration(sess.Duration),
			subtleStyle.Render(candidate),
			trailing,
		)
	}

	return fmt.Sprintf(
		"%-12s %-10s %-10s %-10s %s",
		code,
		stateStyle(state),
		formatBytes(sess.Bytes),
		humanDuration(sess.Duration),
		trailing,
	)
}

func renderDebugPanel(metrics *transport.RelayMetrics, verbose bool) string {
	lines := []string{
		titleStyle.Render("🔍 Debug Information"),
		"",
	}

	// Direct outcomes with percentages
	totalOutcomes := 0
	for _, count := range metrics.DirectOutcomeCount {
		totalOutcomes += count
	}

	if verbose && totalOutcomes > 0 {
		lines = append(lines, "Direct outcomes:")
		for outcome, count := range metrics.DirectOutcomeCount {
			if count > 0 {
				pct := float64(count) / float64(totalOutcomes) * 100
				lines = append(lines, fmt.Sprintf("  %-14s %s (%s)",
					outcome,
					summaryValue(count),
					subtleStyle.Render(fmt.Sprintf("%.1f%%", pct))))
			}
		}
	} else {
		lines = append(lines, fmt.Sprintf("Direct outcomes  %s", summarizeCountMap(metrics.DirectOutcomeCount, 6)))
	}

	if verbose {
		lines = append(lines, "")
		lines = append(lines, "Candidates used:")
		for candidate, count := range metrics.CandidateCount {
			if count > 0 {
				lines = append(lines, fmt.Sprintf("  %-14s %s", candidate, summaryValue(count)))
			}
		}
	} else {
		lines = append(lines, fmt.Sprintf("Candidates used  %s", summarizeCountMap(metrics.CandidateCount, 6)))
	}

	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("Failure causes   %s", summarizeCountMap(metrics.ErrorCount, 6)))

	if len(metrics.RecentFailures) > 0 {
		lines = append(lines, "")
		lines = append(lines, subtleStyle.Render("Recent failures"))
		maxFailures := 4
		if verbose {
			maxFailures = 8
		}
		for i, sess := range metrics.RecentFailures {
			if i >= maxFailures {
				break
			}
			lines = append(lines, renderFailureRow(sess, metrics.Generated, verbose))
		}
	}

	body := strings.Join(lines, "\n")
	return bubbleBoxStyle.Render(body)
}

func summarizeCountMap(counts map[string]int, limit int) string {
	if len(counts) == 0 {
		return subtleStyle.Render("(none)")
	}
	type bucket struct {
		key   string
		count int
	}
	items := make([]bucket, 0, len(counts))
	for key, count := range counts {
		if count <= 0 {
			continue
		}
		items = append(items, bucket{key: truncateMiddle(safeTerminalText(key), 64), count: count})
	}
	if len(items) == 0 {
		return subtleStyle.Render("(none)")
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count == items[j].count {
			return items[i].key < items[j].key
		}
		return items[i].count > items[j].count
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s=%s", item.key, summaryValue(item.count)))
	}
	return strings.Join(parts, ", ")
}

func renderFailureRow(sess transport.SessionSnapshot, ref time.Time, verbose bool) string {
	when := humanDuration(sessionTrailing(sess, ref, false))
	code := truncateMiddle(safeTerminalText(sess.Code), 24)
	candidate := truncateMiddle(safeTerminalText(sess.Candidate), 24)
	if candidate == "" {
		candidate = "unknown"
	}
	outcome := truncateMiddle(safeTerminalText(sess.DirectOutcome), 24)
	if outcome == "" {
		outcome = "unknown"
	}
	errMsg := safeTerminalText(sess.Error)
	if errMsg == "" {
		errMsg = "n/a"
	}

	if verbose {
		errMsg = truncateMiddle(errMsg, 80)
		return fmt.Sprintf(
			"%s %s cand=%s outcome=%s\n    error: %s",
			code,
			subtleStyle.Render(when+" ago"),
			candidate,
			outcome,
			errorStyle.Render(errMsg),
		)
	}

	errMsg = truncateMiddle(errMsg, 56)
	return fmt.Sprintf(
		"%s %s cand=%s outcome=%s err=%s",
		code,
		subtleStyle.Render(when+" ago"),
		candidate,
		outcome,
		errMsg,
	)
}

func truncateMiddle(v string, max int) string {
	if max <= 0 || len(v) <= max {
		return v
	}
	if max < 8 {
		return v[:max]
	}
	head := (max - 3) / 2
	tail := max - 3 - head
	return v[:head] + "..." + v[len(v)-tail:]
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
	return errorBoxStyle.Render(errorStyle.Render("Relay metrics error") + "\n" + safeTerminalText(err.Error()))
}

func safeTerminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, value)
}

func renderFooter(updated time.Time, loading bool, verbose bool) string {
	status := fmt.Sprintf("Last updated %s", updated.Format(time.RFC3339))
	if loading {
		status += " • refreshing"
	}
	mode := "compact"
	if verbose {
		mode = "verbose"
	}
	status += fmt.Sprintf(" • Mode: %s • h help • r refresh • q exit", mode)
	return subtleStyle.Render(status)
}

func renderHelp() string {
	help := []string{
		headerStyle.Render("wormzy operator console - help"),
		"",
		titleStyle.Render("Keyboard Shortcuts"),
		"",
		"  r         Refresh metrics now",
		"  v         Toggle verbose/compact mode",
		"  j/k       Select a live or unresolved session",
		"  x         Terminate the selected rendezvous session",
		"  d         Toggle drain mode for new sessions",
		"  y/n       Confirm or cancel an operator action",
		"  h or ?    Toggle this help screen",
		"  q         Quit dashboard",
		"",
		warningStyle.Render("Control boundaries:"),
		"  • Controls require the same privileged Redis access as this console",
		"  • Drain mode leaves existing transfers alone",
		"  • Removing a session cannot stop an established direct P2P connection",
		"",
		titleStyle.Render("Understanding Metrics"),
		"",
		successStyle.Render("P2P Success Rates:"),
		"  🟢 80%+   Excellent - P2P working great",
		"  🟢 70-80% Good - Healthy P2P rate",
		"  🟡 50-70% Fair - Room for improvement",
		"  🟠 30-50% Poor - P2P struggling",
		"  🔴 <30%   Critical - Investigate immediately",
		"",
		warningStyle.Render("Direct Outcomes:"),
		"  won           Direct P2P connection succeeded",
		"  quic-timeout  Direct attempts timed out → relay fallback",
		"  no-response   No direct candidates available",
		"  noise-failed  QUIC connected but crypto handshake failed",
		"",
		warningStyle.Render("Candidate Types:"),
		"  reflexive     Public IP from STUN (typical NAT-to-NAT)",
		"  local         LAN address (same network)",
		"  relay         Fallback relay server",
		"  loopback      Local testing only",
		"",
		titleStyle.Render("Optimization Tips"),
		"",
		"  • Same-LAN transfers should ALWAYS be P2P via 'local'",
		"  • Target 70-80% P2P for typical home/mobile scenarios",
		"  • High 'quic-timeout' → increase relay fallback delay",
		"  • High 'no-response' → check STUN servers",
		"  • 'noise-failed' → crypto issue, not NAT problem",
		"",
		subtleStyle.Render("See docs/P2P-OPTIMIZATION-GUIDE.md for detailed tuning"),
		"",
		subtleStyle.Render("Press h or ? to return to dashboard"),
	}
	return bubbleBoxStyle.Render(strings.Join(help, "\n"))
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
