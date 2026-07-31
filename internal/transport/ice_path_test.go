package transport

import (
	"fmt"
	"strings"
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
		[]string{"turn:user:pass@turn.example.com:3478?transport=udp"},
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
	turn := set.urls[1]
	if turn.Username != "user" || turn.Password != "pass" {
		t.Fatalf("unexpected TURN credentials: username=%q password=%q", turn.Username, turn.Password)
	}
}

func TestBuildICEURLs_SkipsTURNWithoutCredentials(t *testing.T) {
	set := buildICEURLs(nil, []string{"turn.example.com:3478"}, nil)
	if set.hasTURN {
		t.Fatalf("unexpected TURN support without credentials")
	}
	if len(set.urls) != 0 {
		t.Fatalf("expected credential-less TURN URL to be skipped, got %d url(s)", len(set.urls))
	}
}

func TestBuildICEURLs_NormalizesAuthenticatedTURN(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		username string
		password string
		wantURI  string
	}{
		{
			name:     "opaque URI",
			raw:      "turn:user:pass@turn.example.com:3478?transport=udp",
			username: "user",
			password: "pass",
			wantURI:  "turn:turn.example.com:3478?transport=udp",
		},
		{
			name:     "double slash URI",
			raw:      "turn://user:pass@turn.example.com:3478?transport=udp",
			username: "user",
			password: "pass",
			wantURI:  "turn:turn.example.com:3478?transport=udp",
		},
		{
			name:     "escaped credentials",
			raw:      "turn:user%40example.com:p%3Aass@turn.example.com:3478?transport=udp",
			username: "user@example.com",
			password: "p:ass",
			wantURI:  "turn:turn.example.com:3478?transport=udp",
		},
		{
			name:     "secure TURN",
			raw:      "turns:user:pass@turn.example.com:5349?transport=tcp",
			username: "user",
			password: "pass",
			wantURI:  "turns:turn.example.com:5349?transport=tcp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := buildICEURLs(nil, []string{tt.raw}, nil)
			if !set.hasTURN || len(set.urls) != 1 {
				t.Fatalf("expected one usable TURN URL, got hasTURN=%t urls=%d", set.hasTURN, len(set.urls))
			}
			u := set.urls[0]
			if u.Username != tt.username || u.Password != tt.password {
				t.Fatalf("unexpected TURN credentials: username=%q password=%q", u.Username, u.Password)
			}
			if got := u.String(); got != tt.wantURI {
				t.Fatalf("unexpected normalized TURN URL: %s", got)
			}
		})
	}
}

func TestBuildICEURLs_RedactsSkippedTURNCredentials(t *testing.T) {
	var logLine string
	reporter := ReporterFunc(func(format string, args ...interface{}) {
		logLine = fmt.Sprintf(format, args...)
	})

	set := buildICEURLs(nil, []string{"turn:private-user@turn.example.com:3478"}, reporter)
	if set.hasTURN || len(set.urls) != 0 {
		t.Fatalf("expected invalid TURN URL to be skipped")
	}
	if strings.Contains(logLine, "private-user") {
		t.Fatalf("TURN username leaked in log: %q", logLine)
	}
	if !strings.Contains(logLine, "turn:***@turn.example.com:3478") {
		t.Fatalf("expected redacted TURN endpoint in log, got %q", logLine)
	}
}

func TestRedactICEEndpoint(t *testing.T) {
	in := "turn:user:pass@turn.example.com:3478?transport=udp"
	want := "turn:***@turn.example.com:3478?transport=udp"
	if got := redactICEEndpoint(in); got != want {
		t.Fatalf("redaction mismatch: got %q want %q", got, want)
	}
}
