package transport

import (
	"testing"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

func TestPeerSupportsFeatureCaseInsensitive(t *testing.T) {
	peer := rendezvous.SelfInfo{Features: []string{"ICE-V1", "foo"}}
	if !peerSupportsFeature(peer, "ice-v1") {
		t.Fatalf("expected feature match")
	}
	if peerSupportsFeature(peer, "missing") {
		t.Fatalf("unexpected match for missing feature")
	}
}

func TestBoundedDurationClamp(t *testing.T) {
	floor := 2 * time.Second
	ceil := 10 * time.Second
	if got := boundedDuration(0, floor, ceil); got != ceil {
		t.Fatalf("value=0 expected ceil %s got %s", ceil, got)
	}
	if got := boundedDuration(500*time.Millisecond, floor, ceil); got != floor {
		t.Fatalf("below floor expected %s got %s", floor, got)
	}
	if got := boundedDuration(15*time.Second, floor, ceil); got != ceil {
		t.Fatalf("above ceil expected %s got %s", ceil, got)
	}
	if got := boundedDuration(4*time.Second, floor, ceil); got != 4*time.Second {
		t.Fatalf("within bounds expected unchanged, got %s", got)
	}
}

func TestBuildICEURLs_STUNAndTURN(t *testing.T) {
	set := buildICEURLs(
		[]string{"stun.l.google.com:19302"},
		[]string{"turn:turn.example.com:3478?transport=udp"},
		nil,
	)
	if !set.hasSTUN {
		t.Fatalf("expected STUN support")
	}
	if !set.hasTURN {
		t.Fatalf("expected TURN support")
	}
	if len(set.urls) != 2 {
		t.Fatalf("expected 2 urls, got %d", len(set.urls))
	}
}

func TestBuildICEURLs_NormalizeTurnHostPort(t *testing.T) {
	set := buildICEURLs(nil, []string{"turn.example.com:3478"}, nil)
	if !set.hasTURN {
		t.Fatalf("expected TURN support")
	}
	if len(set.urls) != 1 {
		t.Fatalf("expected 1 url, got %d", len(set.urls))
	}
	if got := set.urls[0].String(); got != "turn:turn.example.com:3478?transport=udp" {
		t.Fatalf("unexpected normalized turn url: %s", got)
	}
}

func TestRedactICEEndpoint(t *testing.T) {
	in := "turn:user:pass@turn.example.com:3478?transport=udp"
	want := "turn:***@turn.example.com:3478?transport=udp"
	if got := redactICEEndpoint(in); got != want {
		t.Fatalf("redaction mismatch: got %q want %q", got, want)
	}
}
