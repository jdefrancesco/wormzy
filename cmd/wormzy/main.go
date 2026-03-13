//	Wormzy is a super easy. hassle free secure tool for sending and receiving.
//
// This is the main entry point for the CLI/TUI application.
// If you want to know how wormzy works in detail, please refer to the documentation.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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

func main() {
	// Define command-line flags.
	rawCmd, preFile, preCode, stripped := normalizeArgs(os.Args[1:])
	os.Args = append([]string{os.Args[0]}, stripped...)
	var (
		modeFlag    = flag.String("mode", "", "send or recv (deprecated; use wormzy send/recv)")
		file        = flag.String("file", "", "file to send (send mode only)")
		code        = flag.String("code", "", "wormzy pairing code")
		relay       = flag.String("relay", "", "redis address/URL for rendezvous (defaults to WORMZY_RELAY_URL or 127.0.0.1:6379)")
		relayPin    = flag.String("relay-pin", "", "base64(SHA256(SPKI)) pin for rendezvous TLS")
		timeout     = flag.Duration("timeout", 90*time.Second, "handshake timeout before we give up on pairing")
		idleTO      = flag.Duration("idle-timeout", 5*time.Minute, "max idle time after pairing before aborting a stalled transfer")
		loopback    = flag.Bool("dev-loopback", false, "use local addresses for testing")
		showNetwork = flag.Bool("show-network", false, "display relay/STUN diagnostics in the UI")
		downloadDir = flag.String("download-dir", "", "directory to store received files (defaults to current directory)")
		logFile     = flag.String("log-file", "", "append detailed session logs to the given file")
	)
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		headingStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFD75F"))
		heading := headingStyle.Render("Usage\n")
		sendLine := lipgloss.NewStyle().Foreground(lipgloss.Color("#23a84b")).Render("  wormzy send <file> [flags]")
		recvLine := lipgloss.NewStyle().Foreground(lipgloss.Color("#23a84b")).Render("  wormzy recv [code] [flags]")
		flagHeading := headingStyle.Render("Flags\n")
		fmt.Fprintf(out, "%s\n%s\n%s\n\n%s\n", heading, sendLine, recvLine, flagHeading)
		flag.PrintDefaults()
	}
	flag.Parse()
	mode := strings.TrimSpace(*modeFlag)
	if rawCmd != "" {
		if mode != "" && mode != rawCmd {
			fmt.Fprintf(os.Stderr, "error: conflicting mode: %s (flag) vs %s command\n", mode, rawCmd)
			os.Exit(1)
		}
		mode = rawCmd
	}
	if *file == "" {
		*file = preFile
	}
	if *code == "" {
		*code = preCode
	}
	if extras := flag.Args(); len(extras) > 0 {
		fmt.Fprintf(os.Stderr, "error: unexpected arguments: %s\n", strings.Join(extras, " "))
		os.Exit(1)
	}

	// Avoid starting interactive UI/transfer work during `go test`.
	if runningUnderGoTest() {
		return
	}

	// If the pairing code isn't provided, prompt interactively in recv mode.
	if mode == "recv" && *code == "" {
		entered, err := promptForCode()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error reading code:", err)
			os.Exit(1)
		}
		*code = entered
	}

	// Validate after argument normalization and optional prompting.
	if err := validateArgs(mode, *file); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		flag.Usage()
		os.Exit(1)
	}

	if mode == "recv" {
		dir := *downloadDir
		if dir == "" {
			dir = "."
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid download directory:", err)
			os.Exit(1)
		}
		if err := ensureDownloadDir(absDir); err != nil {
			fmt.Fprintln(os.Stderr, "download directory error:", err)
			os.Exit(1)
		}
		*downloadDir = absDir
	} else {
		*downloadDir = ""
	}

	var (
		logCloser    io.Closer
		fileReporter transport.Reporter
	)

	// Logging if needed
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "failed to open log file:", err)
			os.Exit(1)
		}
		logCloser = f
		fileReporter = newFileReporter(f)
		fmt.Fprintf(os.Stderr, "wormzy: logging detailed output to %s\n", *logFile)
		defer logCloser.Close()
	}

	//  Here we setup context.Cancel the transfer on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Resolve relay address from flag/env/default.
	relayAddr := resolveRelay(*relay)

	// Transport config is the single source of truth for the session.
	cfg := transport.Config{
		Mode:             mode,
		FilePath:         *file,
		Code:             *code,
		RelayAddr:        relayAddr,
		RelayPin:         *relayPin,
		HandshakeTimeout: *timeout,
		IdleTimeout:      *idleTO,
		Loopback:         *loopback,
		DownloadDir:      *downloadDir,
	}

	// If we don't have an interactive terminal, run without the Bubble Tea UI.
	if !hasTTY() {
		runHeadless(ctx, cfg, fileReporter)
		return
	}

	// Start the TUI and run the transfer concurrently.
	session := ui.Session{
		Mode:        strings.ToUpper(mode),
		File:        displayFile(mode, *file),
		Relay:       relayAddr,
		Code:        *code,
		ShowNetwork: *showNetwork,
		DownloadDir: *downloadDir,
	}
	model := ui.NewModel(session)
	prog := tea.NewProgram(model, tea.WithAltScreen())

	// Fan out transport reporting to the UI and the optional log file.
	reporter := combineReporters(ui.NewReporter(prog), fileReporter)
	done := make(chan error, 1)

	go func() {
		// Run the transport in the background and notify the UI when it finishes.
		result, err := transport.Run(ctx, cfg, reporter)
		done <- err
		prog.Send(ui.DoneMsg{Result: result, Err: err})
	}()

	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ui error:", err)
		os.Exit(1)
	}

	// Ensure the background transfer is canceled even if the UI exits first.
	stop()

	if err := <-done; err != nil {
		os.Exit(1)
	}
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

func normalizeArgs(args []string) (command, fileArg, codeArg string, remaining []string) {
	if len(args) == 0 {
		return "", "", "", args
	}
	first := args[0]
	if first != "send" && first != "recv" {
		return "", "", "", args
	}
	command = first
	copyArgs := append([]string(nil), args[1:]...)
	if len(copyArgs) > 0 && !strings.HasPrefix(copyArgs[0], "-") {
		switch command {
		case "send":
			fileArg = copyArgs[0]
		case "recv":
			codeArg = copyArgs[0]
		}
		copyArgs = copyArgs[1:]
	}
	return command, fileArg, codeArg, copyArgs
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
	return transport.DefaultRelay()
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
