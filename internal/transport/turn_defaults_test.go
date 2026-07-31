package transport

import "testing"

func TestDefaultTURNServers_DoesNotDeriveCredentials(t *testing.T) {
	got := DefaultTURNServers("https://relay.example.com")
	if len(got) != 0 {
		t.Fatalf("expected no unauthenticated TURN default, got %#v", got)
	}
}
