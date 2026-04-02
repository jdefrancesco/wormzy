package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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
}

func TestResolveTURNServers_FlagOverridesEnv(t *testing.T) {
	t.Setenv("WORMZY_TURN_URLS", "turn:env.example.com:3478?transport=udp")
	got := resolveTURNServers("turn:flag.example.com:3478?transport=udp")
	if len(got) != 1 || got[0] != "turn:flag.example.com:3478?transport=udp" {
		t.Fatalf("unexpected turn servers from flag: %#v", got)
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

func TestEffectiveTURNServers_UsesDefaultFromRelay(t *testing.T) {
	t.Setenv("WORMZY_TURN_URLS", "")
	got := effectiveTURNServers("", "https://relay.example.com")
	if len(got) != 1 || got[0] != "relay.example.com:3479" {
		t.Fatalf("unexpected effective turn defaults: %#v", got)
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
