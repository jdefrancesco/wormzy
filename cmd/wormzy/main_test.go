package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
	"github.com/jdefrancesco/wormzy/internal/transport"
)

func TestProbeRelayHTTP_OK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	if err := probeRelay(ts.URL); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestProbeRelayHTTP_Fail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	if err := probeRelay(ts.URL); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestProbeRelayHTTPRejectsRedirect verifies health probes do not follow an endpoint redirect.
func TestProbeRelayHTTPRejectsRedirect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			http.Redirect(w, r, "/ready", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if err := probeRelay(ts.URL); err == nil {
		t.Fatal("probeRelay followed a health endpoint redirect")
	}
}

func TestParseCLI_SendHelp_PrintsUsageOnce(t *testing.T) {
	output := captureStdout(t, func() {
		_, err := parseCLI([]string{"send", "-h"})
		if !errors.Is(err, errShowHelp) {
			t.Fatalf("expected errShowHelp, got %v", err)
		}
	})

	if count := strings.Count(output, "wormzy send"); count != 1 {
		t.Fatalf("expected help to print once, got %d copies\noutput:\n%s", count, output)
	}
	if !strings.Contains(output, "--logs") {
		t.Fatalf("expected send help to document --logs\noutput:\n%s", output)
	}
}

func TestBannerColorsUseSixDigitHex(t *testing.T) {
	colors := map[string]string{
		"title":    bannerTitleColor,
		"subtitle": bannerSubtitleColor,
		"border":   bannerBorderColor,
	}

	for name, color := range colors {
		t.Run(name, func(t *testing.T) {
			if len(color) != 7 || color[0] != '#' {
				t.Fatalf("banner color %q must use #RRGGBB format", color)
			}
			if _, err := strconv.ParseUint(color[1:], 16, 24); err != nil {
				t.Fatalf("banner color %q is not valid hexadecimal: %v", color, err)
			}
		})
	}
}

func TestParseCLI_VersionCommands(t *testing.T) {
	for _, arg := range []string{"version", "-version", "--version"} {
		t.Run(arg, func(t *testing.T) {
			opt, err := parseCLI([]string{arg})
			if err != nil {
				t.Fatalf("parse %s: %v", arg, err)
			}
			if opt.Mode != "version" {
				t.Fatalf("mode = %q; want version", opt.Mode)
			}
		})
	}
}

// TestParseCLI_CodeCommand verifies the script-friendly pairing-code command.
func TestParseCLI_CodeCommand(t *testing.T) {
	opt, err := parseCLI([]string{"code"})
	if err != nil {
		t.Fatalf("parse code: %v", err)
	}
	if opt.Mode != "code" {
		t.Fatalf("mode = %q; want code", opt.Mode)
	}
	if _, err := parseCLI([]string{"code", "extra"}); err == nil {
		t.Fatal("accepted arguments after code command")
	}
}

// TestWriteGeneratedPairingCodeProducesCanonicalSecret verifies automation can
// request a strong code without parsing the banner.
func TestWriteGeneratedPairingCodeProducesCanonicalSecret(t *testing.T) {
	var output bytes.Buffer
	if err := writeGeneratedPairingCode(&output); err != nil {
		t.Fatalf("write generated pairing code: %v", err)
	}
	code := strings.TrimSpace(output.String())
	if _, err := rendezvous.NormalizeCode(code); err != nil {
		t.Fatalf("generated code %q is invalid: %v", code, err)
	}
}

func TestParseCLI_AutoExitSharedFlag(t *testing.T) {
	sendOpt, err := parseCLI([]string{"send", "payload.bin", "--auto-exit"})
	if err != nil {
		t.Fatalf("parse send: %v", err)
	}
	if !sendOpt.AutoExit {
		t.Fatalf("expected send --auto-exit to be enabled")
	}

	recvOpt, err := parseCLI([]string{"recv", "--auto-exit", "code-123"})
	if err != nil {
		t.Fatalf("parse recv: %v", err)
	}
	if !recvOpt.AutoExit {
		t.Fatalf("expected recv --auto-exit to be enabled")
	}
}

func TestParseCLI_NoUPnPSharedFlag(t *testing.T) {
	sendOpt, err := parseCLI([]string{"send", "payload.bin", "--no-upnp"})
	if err != nil {
		t.Fatalf("parse send: %v", err)
	}
	if !sendOpt.NoUPnP {
		t.Fatalf("expected send --no-upnp to be enabled")
	}

	recvOpt, err := parseCLI([]string{"recv", "--no-upnp", "code-123"})
	if err != nil {
		t.Fatalf("parse recv: %v", err)
	}
	if !recvOpt.NoUPnP {
		t.Fatalf("expected recv --no-upnp to be enabled")
	}
}

func TestParseCLI_LogsSharedFlag(t *testing.T) {
	defaultOpt, err := parseCLI([]string{"send", "payload.bin"})
	if err != nil {
		t.Fatalf("parse default send: %v", err)
	}
	if defaultOpt.ShowLogs {
		t.Fatal("expected logs to be disabled by default")
	}

	sendOpt, err := parseCLI([]string{"send", "payload.bin", "--logs"})
	if err != nil {
		t.Fatalf("parse send: %v", err)
	}
	if !sendOpt.ShowLogs {
		t.Fatal("expected send --logs to be enabled")
	}

	recvOpt, err := parseCLI([]string{"recv", "--logs", "code-123"})
	if err != nil {
		t.Fatalf("parse recv: %v", err)
	}
	if !recvOpt.ShowLogs {
		t.Fatal("expected recv --logs to be enabled")
	}
}

func TestParseCLI_SendProvidedCode(t *testing.T) {
	opt, err := parseCLI([]string{"send", "payload.bin", "--code", "gayt-emzu-gu3d-oobz-mfra"})
	if err != nil {
		t.Fatalf("parse send: %v", err)
	}
	if opt.Code != "gayt-emzu-gu3d-oobz-mfra" {
		t.Fatalf("send code = %q; want preselected A/B trial code", opt.Code)
	}
}

// TestValidateArgs_SendPath verifies that send inputs fail before transfer setup when they are not files.
func TestValidateArgs_SendPath(t *testing.T) {
	t.Run("directory requires archiving", func(t *testing.T) {
		err := validateArgs("send", t.TempDir())
		if err == nil {
			t.Fatal("validateArgs accepted a directory")
		}
		if !strings.Contains(err.Error(), "directory") || !strings.Contains(err.Error(), "archive or compress") {
			t.Fatalf("directory error = %q; want archive or compression guidance", err)
		}
	})

	t.Run("regular file is accepted", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "payload.bin")
		if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
			t.Fatalf("write payload: %v", err)
		}
		if err := validateArgs("send", file); err != nil {
			t.Fatalf("validateArgs rejected a regular file: %v", err)
		}
	})

	t.Run("missing file fails early", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "missing.bin")
		if err := validateArgs("send", file); err == nil {
			t.Fatal("validateArgs accepted a missing file")
		}
	})

	t.Run("special file fails early", func(t *testing.T) {
		info, err := os.Stat(os.DevNull)
		if err != nil || info.Mode().IsRegular() {
			t.Skip("platform does not expose a non-regular null device")
		}
		if err := validateArgs("send", os.DevNull); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("special-file error = %v; want regular-file guidance", err)
		}
	})
}

// TestHeadlessReporter_PairingCodeOnce verifies immediate headless code output without duplication.
func TestHeadlessReporter_PairingCodeOnce(t *testing.T) {
	var output bytes.Buffer
	reporter := newHeadlessReporter(&output)

	reporter.Stage(transport.StageRendezvous, transport.StageStateRunning, "code quick-code")
	reporter.PrintPairingCode("quick-code")

	if got := strings.Count(output.String(), "Pairing code: quick-code"); got != 1 {
		t.Fatalf("pairing code output count = %d; want 1\noutput:\n%s", got, output.String())
	}
}

// TestFileReporter_RedactsPairingCode verifies durable diagnostic logs never
// persist the PAKE secret displayed to the local user.
func TestFileReporter_RedactsPairingCode(t *testing.T) {
	var output bytes.Buffer
	reporter := newFileReporter(&output)
	reporter.Stage(transport.StageRendezvous, transport.StageStateRunning, "code "+"gayt-emzu-gu3d-oobz-mfra")

	if strings.Contains(output.String(), "gayt-emzu-gu3d-oobz-mfra") {
		t.Fatalf("file reporter exposed pairing code: %s", output.String())
	}
	if !strings.Contains(output.String(), "code [redacted]") {
		t.Fatalf("file reporter omitted redaction marker: %s", output.String())
	}
}

func TestResolveTURNServers_FlagOverridesEnv(t *testing.T) {
	t.Setenv("WORMZY_TURN_URLS", "turn:env.example.com:3478?transport=udp")
	got := resolveTURNServers("turn:flag.example.com:3478?transport=udp")
	if len(got) != 1 || got[0] != "turn:flag.example.com:3478?transport=udp" {
		t.Fatalf("unexpected turn servers from flag: %#v", got)
	}
}

func TestUPnPDisabledByEnv(t *testing.T) {
	t.Setenv("WORMZY_UPNP", "0")
	if !upnpDisabledByEnv() {
		t.Fatalf("expected WORMZY_UPNP=0 to disable UPnP")
	}

	t.Setenv("WORMZY_UPNP", "yes")
	if upnpDisabledByEnv() {
		t.Fatalf("expected WORMZY_UPNP=yes to keep UPnP enabled")
	}
}

func TestResolveTURNServers_EnvListDedupes(t *testing.T) {
	t.Setenv(
		"WORMZY_TURN_URLS",
		"turn:a.example.com:3478?transport=udp, turn:b.example.com:3478?transport=udp ; turn:a.example.com:3478?transport=udp",
	)
	got := resolveTURNServers("")
	if len(got) != 2 {
		t.Fatalf("expected 2 deduped servers, got %#v", got)
	}
	if got[0] != "turn:a.example.com:3478?transport=udp" || got[1] != "turn:b.example.com:3478?transport=udp" {
		t.Fatalf("unexpected parsed server list: %#v", got)
	}
}

func TestEffectiveTURNServers_DoesNotDeriveCredentialsFromRelay(t *testing.T) {
	t.Setenv("WORMZY_TURN_URLS", "")
	got := effectiveTURNServers("", "https://relay.example.com")
	if len(got) != 0 {
		t.Fatalf("expected no unauthenticated TURN defaults, got %#v", got)
	}
}

// TestFormatTURNServerSummaryRedactsAllUserInfo verifies diagnostic output never exposes TURN usernames or passwords.
func TestFormatTURNServerSummaryRedactsAllUserInfo(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unprefixed credentials", raw: "alice:swordfish@turn.example.test:3478", want: "***@turn.example.test:3478"},
		{name: "TURN opaque URL", raw: "turn:alice:swordfish@turn.example.test:3478?transport=udp", want: "turn:***@turn.example.test:3478?transport=udp"},
		{name: "TURN hierarchical URL", raw: "turn://alice:swordfish@turn.example.test:3478", want: "turn://***@turn.example.test:3478"},
		{name: "unprefixed username only", raw: "alice@turn.example.test:3478", want: "***@turn.example.test:3478"},
		{name: "credential free", raw: "turn:turn.example.test:3478", want: "turn:turn.example.test:3478"},
		{name: "credential query", raw: "turn:alice:swordfish@turn.example.test:3478?credential=query-secret", want: "(invalid endpoint redacted)"},
		{name: "malformed credential-like endpoint", raw: "turn:alice:swordfish", want: "(invalid endpoint redacted)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatTURNServerSummary([]string{tt.raw}); got != tt.want {
				t.Fatalf("summary = %q; want %q", got, tt.want)
			}
		})
	}
}

// TestFormatMailboxEndpointSummaryRedactsCredentials verifies info output does not expose mailbox secrets.
func TestFormatMailboxEndpointSummaryRedactsCredentials(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "Redis userinfo and query",
			raw:  "redis://alice:swordfish@localhost:6379/0?password=query-secret",
			want: "redis://localhost:6379/0",
		},
		{
			name: "HTTPS userinfo and query",
			raw:  "https://alice:swordfish@relay.example.test/mailbox?token=query-secret",
			want: "https://relay.example.test/mailbox",
		},
		{
			name: "opaque malformed URL",
			raw:  "https:alice:mailbox-secret@relay://example.test",
			want: "(invalid endpoint redacted)",
		},
		{name: "bare endpoint", raw: "127.0.0.1:6379", want: "127.0.0.1:6379"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatMailboxEndpointSummary(tt.raw); got != tt.want {
				t.Fatalf("summary = %q; want %q", got, tt.want)
			}
		})
	}
}

// TestRunInfoRedactsMailboxEnvironment verifies rejected endpoints are still safe to print.
func TestRunInfoRedactsMailboxEnvironment(t *testing.T) {
	const secret = "mailbox-secret"
	endpoint := "redis://alice:" + secret + "@198.51.100.20:6379/0?token=" + secret
	t.Setenv("WORMZY_RELAY_URL", endpoint)

	output := captureStdout(t, func() {
		if err := runInfo(options{}); err == nil {
			t.Fatal("runInfo accepted a remote Redis endpoint")
		}
	})
	if strings.Contains(output, "alice") || strings.Contains(output, secret) {
		t.Fatalf("info output exposed mailbox credentials: %q", output)
	}
	if !strings.Contains(output, "redis://198.51.100.20:6379/0") {
		t.Fatalf("info output omitted the redacted mailbox endpoint: %q", output)
	}
}

// TestProbeRelayRejectsRemoteDirectRedis verifies info mode follows the transfer endpoint policy.
func TestProbeRelayRejectsRemoteDirectRedis(t *testing.T) {
	err := probeRelay("redis://198.51.100.20:6379")
	if err == nil || err.Error() != "invalid mailbox endpoint" {
		t.Fatalf("probeRelay error = %v; want endpoint-policy rejection", err)
	}
}

// TestRelayDialTargetPreservesUnixSocket verifies local Redis probes use the configured socket path.
func TestRelayDialTargetPreservesUnixSocket(t *testing.T) {
	target, err := relayDialTarget("unix:///tmp/wormzy-redis.sock")
	if err != nil {
		t.Fatalf("relayDialTarget: %v", err)
	}
	if target != "/tmp/wormzy-redis.sock" {
		t.Fatalf("target = %q; want Unix socket path", target)
	}
}

// TestRelayDialTargetAddsIPv6RedisPort verifies default ports are joined safely for IPv6 literals.
func TestRelayDialTargetAddsIPv6RedisPort(t *testing.T) {
	target, err := relayDialTarget("redis://[::1]")
	if err != nil {
		t.Fatalf("relayDialTarget: %v", err)
	}
	if target != "[::1]:6379" {
		t.Fatalf("target = %q; want IPv6 Redis address", target)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	output := <-done
	if err := r.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	return output
}
