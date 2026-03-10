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
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/jdefrancesco/internal/transport"
	"github.com/jdefrancesco/internal/ui"
)

const version = "0.1.0-dev"

func main() {
	var (
		mode        = flag.String("mode", "", "send or recv")
		file        = flag.String("file", "", "file to send (send mode only)")
		code        = flag.String("code", "", "wormzy pairing code")
		relay       = flag.String("relay", "127.0.0.1:9999", "rendezvous address")
		relayPin    = flag.String("relay-pin", "", "base64(SHA256(SPKI)) pin for rendezvous TLS")
		timeout     = flag.Duration("timeout", 60*time.Second, "overall rendezvous timeout")
		loopback    = flag.Bool("dev-loopback", false, "use local addresses for testing")
		showNetwork = flag.Bool("show-network", false, "display relay/STUN diagnostics in the UI")
	)
	flag.Parse()

	if runningUnderGoTest() {
		return
	}

	if err := applyCommandArgs(mode, file, code); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if *mode == "recv" && *code == "" {
		entered, err := promptForCode()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error reading code:", err)
			os.Exit(1)
		}
		*code = entered
	}

	if err := validateArgs(*mode, *file); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := transport.Config{
		Mode:      *mode,
		FilePath:  *file,
		Code:      *code,
		RelayAddr: *relay,
		RelayPin:  *relayPin,
		Timeout:   *timeout,
		Loopback:  *loopback,
	}

	if !hasTTY() {
		runHeadless(ctx, cfg)
		return
	}

	session := ui.Session{
		Mode:        strings.ToUpper(*mode),
		File:        displayFile(*mode, *file),
		Relay:       *relay,
		ShowNetwork: *showNetwork,
	}
	model := ui.NewModel(session)
	prog := tea.NewProgram(model, tea.WithAltScreen())

	reporter := ui.NewReporter(prog)
	done := make(chan error, 1)

	go func() {
		result, err := transport.Run(ctx, cfg, reporter)
		done <- err
		prog.Send(ui.DoneMsg{Result: result, Err: err})
	}()

	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ui error:", err)
		os.Exit(1)
	}

	stop()

	if err := <-done; err != nil {
		os.Exit(1)
	}
}

func validateArgs(mode, file string) error {
	if mode != "send" && mode != "recv" {
		return fmt.Errorf("mode must be send or recv")
	}
	if mode == "send" && file == "" {
		return fmt.Errorf("send requires a file (wormzy send <file>)")
	}
	return nil
}

func displayFile(mode, file string) string {
	if mode != "send" || file == "" {
		return "waiting for manifest"
	}
	return filepath.Base(file)
}

func runningUnderGoTest() bool {
	return flag.Lookup("test.v") != nil
}

func hasTTY() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) && isatty.IsTerminal(os.Stdin.Fd())
}

func runHeadless(ctx context.Context, cfg transport.Config) {
	fmt.Println("wormzy: TTY not detected, running without Bubble Tea UI")
	reporter := transport.ReporterFunc(func(format string, args ...interface{}) {
		fmt.Printf("[wormzy] "+format+"\n", args...)
	})
	result, err := transport.Run(ctx, cfg, reporter)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if result != nil {
		fmt.Printf("Pairing code: %s\n", result.Code)
	}
}

func applyCommandArgs(mode, file, code *string) error {
	args := flag.Args()
	if len(args) == 0 {
		return nil
	}
	cmd := args[0]
	switch cmd {
	case "send":
		if *mode != "" && *mode != "send" {
			return fmt.Errorf("conflicting mode: %s (flag) vs send command", *mode)
		}
		*mode = "send"
		rest := args[1:]
		if *file == "" {
			if len(rest) == 0 {
				return fmt.Errorf("send requires a file argument (wormzy send <file>)")
			}
			*file = rest[0]
			rest = rest[1:]
		}
		if len(rest) > 0 {
			return fmt.Errorf("unexpected arguments for send: %s", strings.Join(rest, " "))
		}
	case "recv":
		if *mode != "" && *mode != "recv" {
			return fmt.Errorf("conflicting mode: %s (flag) vs recv command", *mode)
		}
		*mode = "recv"
		rest := args[1:]
		if *code == "" && len(rest) > 0 {
			*code = rest[0]
			rest = rest[1:]
		}
		if len(rest) > 0 {
			return fmt.Errorf("unexpected arguments for recv: %s", strings.Join(rest, " "))
		}
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

func promptForCode() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Enter wormzy pairing code: ")
	code, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(code), nil
}
