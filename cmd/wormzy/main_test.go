package main

import (
	"net/http"
	"net/http/httptest"
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
