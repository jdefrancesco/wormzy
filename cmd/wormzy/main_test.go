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
