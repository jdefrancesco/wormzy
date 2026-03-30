//	Wormzy is a super easy. hassle free secure tool for sending and receiving.
//
// This is the main entry point for the CLI/TUI application.
// If you want to know how wormzy works in detail, please refer to the documentation.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"

	"github.com/jdefrancesco/internal/transport"
	"github.com/jdefrancesco/internal/ui"
)

const version = "0.1.2-dev"

func boldCodeStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFD75F"))
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#90cc8918"))
	subtitleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#727f79"))

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#afcfaa43")).
			Padding(1, 3)
)

func ShowHeader() string {

	logo := `
██╗    ██╗ ██████╗ ██████╗ ███╗   ███╗███████╗██╗   ██╗
██║    ██║██╔═══██╗██╔══██╗████╗ ████║╚══███╔╝╚██╗ ██╔╝
██║ █╗ ██║██║   ██║██████╔╝██╔████╔██║  ███╔╝  ╚████╔╝
██║███╗██║██║   ██║██╔══██╗██║╚██╔╝██║ ███╔╝    ╚██╔╝
╚███╔███╔╝╚██████╔╝██║  ██║██║ ╚═╝ ██║███████╗   ██║
 ╚══╝╚══╝  ╚═════╝ ╚═╝  ╚═╝╚═╝     ╚═╝╚══════╝   ╚═╝
`

	title := titleStyle.Render(logo)
	subtitle := subtitleStyle.Render("secure peer-to-peer file transfer • noise + quic")

	return boxStyle.Render(title + "\n" + subtitle)
}

type options struct {
	Mode        string
	File        string
	Code        string
	DownloadDir string
	Relay       string
	RelayPin    string
	Timeout     time.Duration
	IdleTimeout time.Duration
	DevLoopback bool
	ShowNetwork bool
	LogFile     string
}

var (
	errShowHelp       = errors.New("usage shown")
	usageHeadingStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFD75F"))
	usageCommandStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#5DFF8D"))
	usageFlagStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD75F"))
	usageDescStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#CACACA"))
)

func main() {
	fmt.Println(ShowHeader())
	fmt.Println()

	opts, err := parseCLI(os.Args[1:])
	switch {
	case errors.Is(err, errShowHelp):
		return
	case err != nil:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := execute(opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func execute(opt options) error {
	if runningUnderGoTest() {
		return nil
	}

	if opt.Mode == "info" {
		return runInfo(opt)
	}

	mode := opt.Mode
	file := opt.File
	code := opt.Code
	downloadDir := opt.DownloadDir

	if mode == "recv" {
		if code == "" {
			entered, err := promptForCode()
			if err != nil {
				return fmt.Errorf("error reading code: %w", err)
			}
			code = entered
		}
		if downloadDir == "" {
			downloadDir = "."
		}
		absDir, err := filepath.Abs(downloadDir)
		if err != nil {
			return fmt.Errorf("invalid download directory: %w", err)
		}
		if err := ensureDownloadDir(absDir); err != nil {
			return fmt.Errorf("download directory error: %w", err)
		}
		downloadDir = absDir
	} else {
		downloadDir = ""
	}

	if err := validateArgs(mode, file); err != nil {
		return err
	}

	var (
		logCloser    io.Closer
		fileReporter transport.Reporter
	)
	if opt.LogFile != "" {
		f, err := os.OpenFile(opt.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("failed to open log file: %w", err)
		}
		logCloser = f
		fileReporter = newFileReporter(f)
		fmt.Fprintf(os.Stderr, "wormzy: logging detailed output to %s\n", opt.LogFile)
		defer logCloser.Close()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	relayAddr := resolveRelay(opt.Relay)
	cfg := transport.Config{
		Mode:             mode,
		FilePath:         file,
		Code:             code,
		RelayAddr:        relayAddr,
		RelayPin:         opt.RelayPin,
		HandshakeTimeout: opt.Timeout,
		IdleTimeout:      opt.IdleTimeout,
		Loopback:         opt.DevLoopback,
		DownloadDir:      downloadDir,
	}

	if !hasTTY() {
		runHeadless(ctx, cfg, fileReporter)
		return nil
	}

	session := ui.Session{
		Mode:        strings.ToUpper(mode),
		File:        displayFile(mode, file),
		Relay:       relayAddr,
		Code:        code,
		ShowNetwork: opt.ShowNetwork,
		DownloadDir: downloadDir,
	}
	model := ui.NewModel(session)
	prog := tea.NewProgram(model, tea.WithAltScreen())

	reporter := combineReporters(ui.NewReporter(prog), fileReporter)
	done := make(chan error, 1)

	go func() {
		result, err := transport.Run(ctx, cfg, reporter)
		done <- err
		prog.Send(ui.DoneMsg{Result: result, Err: err})
	}()

	if _, err := prog.Run(); err != nil {
		return fmt.Errorf("ui error: %w", err)
	}

	stop()

	if err := <-done; err != nil {
		return err
	}
	return nil
}

func parseCLI(args []string) (options, error) {
	if len(args) == 0 {
		printGeneralUsage()
		return options{}, errShowHelp
	}
	switch args[0] {
	case "send":
		return parseSend(args[1:])
	case "recv":
		return parseRecv(args[1:])
	case "info":
		return parseInfo(args[1:])
	case "help", "-h", "--help":
		printGeneralUsage()
		return options{}, errShowHelp
	default:
		printGeneralUsage()
		return options{}, fmt.Errorf("unknown command %q", args[0])
	}
}

func parseSend(args []string) (options, error) {
	opt := options{Mode: "send"}
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opt.File, "file", "", "file to send (optional if provided positionally)")
	registerSharedFlags(fs, &opt)
	fs.Usage = printSendUsage

	// Allow the file argument in any position by plucking the first non-flag token
	// before parsing flags.
	var positional string
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if positional == "" && !strings.HasPrefix(a, "-") {
			positional = a
			continue
		}
		filtered = append(filtered, a)
	}

	if err := fs.Parse(filtered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printSendUsage()
			return options{}, errShowHelp
		}
		printSendUsage()
		return options{}, err
	}
	rest := fs.Args()
	if opt.File == "" && positional != "" {
		opt.File = positional
	}
	if opt.File == "" && len(rest) > 0 {
		opt.File = rest[0]
		rest = rest[1:]
	}
	if len(rest) > 0 {
		printSendUsage()
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(rest, " "))
	}
	if opt.File == "" {
		printSendUsage()
		return options{}, fmt.Errorf("send requires a file (wormzy send <file>)")
	}
	return opt, nil
}

func parseRecv(args []string) (options, error) {
	opt := options{Mode: "recv", DownloadDir: "."}
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opt.Code, "code", "", "wormzy pairing code")
	fs.StringVar(&opt.DownloadDir, "download-dir", ".", "directory to store received files")
	registerSharedFlags(fs, &opt)
	fs.Usage = printRecvUsage
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printRecvUsage()
			return options{}, errShowHelp
		}
		printRecvUsage()
		return options{}, err
	}
	rest := fs.Args()
	if opt.Code == "" && len(rest) > 0 {
		opt.Code = rest[0]
		rest = rest[1:]
	}
	if len(rest) > 0 {
		printRecvUsage()
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(rest, " "))
	}
	return opt, nil
}

func parseInfo(args []string) (options, error) {
	opt := options{Mode: "info"}
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opt.Relay, "relay", "", "override relay URL/address to check")
	fs.Usage = printInfoUsage
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printInfoUsage()
			return options{}, errShowHelp
		}
		printInfoUsage()
		return options{}, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		printInfoUsage()
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(extra, " "))
	}
	return opt, nil
}

func registerSharedFlags(fs *flag.FlagSet, opt *options) {
	fs.StringVar(&opt.Relay, "relay", "", "redis address/URL for rendezvous (defaults to WORMZY_RELAY_URL or 127.0.0.1:6379)")
	fs.StringVar(&opt.RelayPin, "relay-pin", "", "base64(SHA256(SPKI)) pin for rendezvous TLS")
	fs.DurationVar(&opt.Timeout, "timeout", 90*time.Second, "handshake timeout before giving up on pairing")
	fs.DurationVar(&opt.IdleTimeout, "idle-timeout", 5*time.Minute, "max idle time after pairing before aborting a stalled transfer")
	fs.BoolVar(&opt.DevLoopback, "dev-loopback", false, "use local addresses for testing")
	fs.BoolVar(&opt.ShowNetwork, "show-network", false, "display relay/STUN diagnostics in the UI")
	fs.StringVar(&opt.LogFile, "log-file", "", "append detailed session logs to the given file")
}

func printGeneralUsage() {
	fmt.Println(usageHeadingStyle.Render("Usage"))
	fmt.Println(usageCommandStyle.Render("  wormzy send <file> [flags]"))
	fmt.Println(usageCommandStyle.Render("  wormzy recv [code] [flags]"))
	fmt.Println(usageCommandStyle.Render("  wormzy info [flags]"))
	fmt.Println()
	fmt.Println(usageDescStyle.Render("Use `wormzy <command> -h` for command-specific flags."))
}

func printSendUsage() {
	fmt.Println(usageHeadingStyle.Render("wormzy send"))
	fmt.Println(usageDescStyle.Render("Send a file to your peer. Provide the file as a positional argument or with --file."))
	fmt.Println()
	fmt.Println(usageHeadingStyle.Render("Flags"))
	fmt.Println(formatFlagLine("--file", "file to send (optional if provided positionally)"))
	printSharedFlags()
}

func printRecvUsage() {
	fmt.Println(usageHeadingStyle.Render("wormzy recv"))
	fmt.Println(usageDescStyle.Render("Receive a file from your peer. Leave the code blank to be prompted interactively."))
	fmt.Println()
	fmt.Println(usageHeadingStyle.Render("Flags"))
	fmt.Println(formatFlagLine("--code", "pairing code provided by the sender"))
	fmt.Println(formatFlagLine("--download-dir", "directory to store received files (default .)"))
	printSharedFlags()
}

func printSharedFlags() {
	fmt.Println(formatFlagLine("--relay", "redis address/URL for rendezvous (defaults to env WORMZY_RELAY_URL)"))
	fmt.Println(formatFlagLine("--relay-pin", "base64(SHA256(SPKI)) pin for rendezvous TLS"))
	fmt.Println(formatFlagLine("--timeout", "handshake timeout before giving up on pairing (default 1m30s)"))
	fmt.Println(formatFlagLine("--idle-timeout", "max idle time after pairing before aborting (default 5m0s)"))
	fmt.Println(formatFlagLine("--dev-loopback", "keep traffic on localhost for demos"))
	fmt.Println(formatFlagLine("--show-network", "display relay/STUN diagnostics in the UI"))
	fmt.Println(formatFlagLine("--log-file", "append detailed session logs to the given file"))
}

func printInfoUsage() {
	fmt.Println(usageHeadingStyle.Render("wormzy info"))
	fmt.Println(usageDescStyle.Render("Check which relay will be used and whether it is reachable."))
	fmt.Println()
	fmt.Println(usageHeadingStyle.Render("Flags"))
	fmt.Println(formatFlagLine("--relay", "override relay URL/address to probe"))
}

func formatFlagLine(name, desc string) string {
	return fmt.Sprintf("  %s %s", usageFlagStyle.Render(name), usageDescStyle.Render(desc))
}

func runInfo(opt options) error {
	relay := resolveRelay(opt.Relay)
	fmt.Println(usageHeadingStyle.Render("Relay probe"))
	env := os.Getenv("WORMZY_RELAY_URL")
	if env == "" {
		env = "(not set)"
	}
	fmt.Println(formatFlagLine("Resolved relay", relay))
	fmt.Println(formatFlagLine("WORMZY_RELAY_URL", env))
	if err := probeRelay(relay); err != nil {
		fmt.Println(formatFlagLine("Status", fmt.Sprintf("unreachable (%v)", err)))
		return err
	}
	fmt.Println(formatFlagLine("Status", "reachable"))
	return nil
}

func probeRelay(relay string) error {
	if strings.HasPrefix(relay, "http://") || strings.HasPrefix(relay, "https://") {
		return probeHTTPRelay(relay)
	}
	target, err := relayDialTarget(relay)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func probeHTTPRelay(relay string) error {
	u, err := url.Parse(relay)
	if err != nil {
		return err
	}
	u.Path = path.Join(u.Path, "/healthz")
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}

func relayDialTarget(relay string) (string, error) {
	if strings.Contains(relay, "://") {
		u, err := url.Parse(relay)
		if err != nil {
			return "", err
		}
		host := u.Host
		if host == "" {
			host = u.Path
		}
		if !strings.Contains(host, ":") {
			switch u.Scheme {
			case "https", "wss":
				host += ":443"
			case "redis", "rediss":
				host += ":6379"
			default:
				host += ":80"
			}
		}
		return host, nil
	}
	return relay, nil
}

// validateArgs checks required arguments for the chosen mode.
func validateArgs(mode, file string) error {
	if mode != "send" && mode != "recv" {
		if mode == "" {
			return fmt.Errorf("missing command; run `wormzy send <file>` or `wormzy recv`")
		}
		return fmt.Errorf("unknown mode %q; use send or recv", mode)
	}
	if mode == "send" && file == "" {
		return fmt.Errorf("send requires a file (wormzy send <file>)")
	}
	return nil
}

// displayFile returns a UI-friendly label for the file being transferred.
func displayFile(mode, file string) string {
	if mode != "send" || file == "" {
		return "waiting for manifest"
	}
	return filepath.Base(file)
}

// runningUnderGoTest reports whether we're running under `go test`.
func runningUnderGoTest() bool {
	return flag.Lookup("test.v") != nil
}

// hasTTY reports whether stdin/stdout are interactive terminals.
func hasTTY() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) && isatty.IsTerminal(os.Stdin.Fd())
}

// runHeadless runs the transport with console output when no TTY is available.
func runHeadless(ctx context.Context, cfg transport.Config, extra transport.Reporter) {
	fmt.Println("wormzy: TTY not detected, running TUI")
	consoleReporter := transport.ReporterFunc(func(format string, args ...any) {
		fmt.Printf("[wormzy] "+format+"\n", args...)
	})
	reporter := combineReporters(consoleReporter, extra)
	result, err := transport.Run(ctx, cfg, reporter)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if result != nil {
		if result.Code != "" {
			fmt.Printf("Pairing code: %s\n", boldCodeStyle().Render(result.Code))
		}
		if result.FilePath != "" {
			fmt.Printf("File: %s\n", result.FilePath)
		}
		if result.FileSize > 0 {
			fmt.Printf("Size: %d bytes\n", result.FileSize)
		}
		if result.FileHash != "" {
			fmt.Printf("BLAKE3-256: %s\n", result.FileHash)
		}
		if result.Transport != "" {
			fmt.Printf("Path: %s (%s)\n", strings.ToUpper(result.Transport), result.Candidate)
		}
	}
}

// promptForCode reads a pairing code from stdin.
func promptForCode() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	prompt := lipgloss.NewStyle().Foreground(lipgloss.Color("#777777")).Render("Pairing code → ")
	fmt.Print(prompt)
	code, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(code), nil
}

// resolveRelay determines the rendezvous relay address.
func resolveRelay(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("WORMZY_RELAY_URL"); env != "" {
		return env
	}
	if env := os.Getenv("WORMZY_RELAY"); env != "" {
		return env
	}
	if cfg, ok := relayFromConfig(); ok {
		return cfg
	}
	return transport.DefaultRelay()
}

// relayFromConfig reads an optional relay override from config files.
func relayFromConfig() (string, bool) {
	paths := []string{
		filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "wormzy", "relay"),
		filepath.Join(os.Getenv("HOME"), ".config", "wormzy", "relay"),
		"/etc/wormzy/relay",
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(string(data)); v != "" {
			return v, true
		}
	}
	return "", false
}

func ensureDownloadDir(path string) error {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	case os.IsNotExist(err):
		return os.MkdirAll(path, 0o750)
	default:
		return err
	}
}

// combineReporters fans out reporting to all non-nil reporters.
func combineReporters(reporters ...transport.Reporter) transport.Reporter {
	var active []transport.Reporter
	for _, r := range reporters {
		if r != nil {
			active = append(active, r)
		}
	}
	if len(active) == 0 {
		return nil
	}
	return reporterMux{reporters: active}
}

type reporterMux struct {
	reporters []transport.Reporter
}

func (m reporterMux) Logf(format string, args ...any) {
	for _, r := range m.reporters {
		if r != nil {
			r.Logf(format, args...)
		}
	}
}

func (m reporterMux) Stage(stage transport.Stage, state transport.StageState, detail string) {
	for _, r := range m.reporters {
		if r != nil {
			r.Stage(stage, state, detail)
		}
	}
}

// newFileReporter creates a reporter that appends detailed logs to w.
func newFileReporter(w io.Writer) transport.Reporter {
	if w == nil {
		return nil
	}
	return &fileReporter{out: w}
}

type fileReporter struct {
	mu  sync.Mutex
	out io.Writer
}

func (f *fileReporter) Logf(format string, args ...any) {
	if f == nil || f.out == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintf(f.out, "[%s] LOG %s\n", time.Now().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
}

func (f *fileReporter) Stage(stage transport.Stage, state transport.StageState, detail string) {
	if f == nil || f.out == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintf(
		f.out,
		"[%s] STAGE %s %s %s\n",
		time.Now().Format(time.RFC3339Nano),
		stage,
		stageStateString(state),
		detail,
	)
}

// stageStateString provides stable strings for stage state logs.
func stageStateString(state transport.StageState) string {
	switch state {
	case transport.StageStatePending:
		return "pending"
	case transport.StageStateRunning:
		return "running"
	case transport.StageStateDone:
		return "done"
	case transport.StageStateError:
		return "error"
	default:
		return fmt.Sprintf("state(%d)", state)
	}
}
