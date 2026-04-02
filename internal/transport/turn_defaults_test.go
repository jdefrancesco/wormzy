package transport

import "testing"

func TestDefaultTURNServers_FromHTTPRelay(t *testing.T) {
	got := DefaultTURNServers("https://relay.example.com")
	if len(got) != 1 || got[0] != "relay.example.com:3479" {
		t.Fatalf("unexpected defaults: %#v", got)
	}
}

func TestDefaultTURNServers_FromBareHostPort(t *testing.T) {
	got := DefaultTURNServers("198.51.100.10:9200")
	if len(got) != 1 || got[0] != "198.51.100.10:3479" {
		t.Fatalf("unexpected defaults: %#v", got)
	}
}

func TestDefaultTURNServers_SkipRedisURL(t *testing.T) {
	got := DefaultTURNServers("redis://127.0.0.1:6379")
	if len(got) != 0 {
		t.Fatalf("expected no default turn for redis url, got %#v", got)
	}
}
