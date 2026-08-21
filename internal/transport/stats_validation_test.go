package transport

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// TestValidateAndSanitizeTransferStatsAcceptsKnownValues verifies legitimate
// final transfer telemetry remains available to the operator dashboard.
func TestValidateAndSanitizeTransferStatsAcceptsKnownValues(t *testing.T) {
	stats := transferStats{
		Mode:           "send",
		Transport:      "p2p",
		Candidate:      "ice-p2p",
		DirectOutcome:  "won",
		DirectSummary:  "ice-p2p@203.0.113.10:4242=won",
		Bytes:          6 << 20,
		DurationMillis: 4_000,
		Completed:      true,
	}

	got, err := validateAndSanitizeTransferStats("send", stats)
	if err != nil {
		t.Fatalf("validate stats: %v", err)
	}
	if got != stats {
		t.Fatalf("validated stats = %#v; want %#v", got, stats)
	}
}

// TestValidateAndSanitizeTransferStatsRejectsUntrustedValues verifies clients
// cannot inject invalid dimensions or corrupt aggregate counters.
func TestValidateAndSanitizeTransferStatsRejectsUntrustedValues(t *testing.T) {
	valid := transferStats{Mode: "recv", Transport: "p2p", Candidate: "ice-p2p"}
	tests := []struct {
		name   string
		mutate func(*transferStats)
	}{
		{name: "role mismatch", mutate: func(stats *transferStats) { stats.Mode = "send" }},
		{name: "unknown transport", mutate: func(stats *transferStats) { stats.Transport = "tunnel" }},
		{name: "unknown candidate", mutate: func(stats *transferStats) { stats.Candidate = "attacker" }},
		{name: "unknown direct outcome", mutate: func(stats *transferStats) { stats.DirectOutcome = "perfect" }},
		{name: "negative bytes", mutate: func(stats *transferStats) { stats.Bytes = -1 }},
		{name: "excessive bytes", mutate: func(stats *transferStats) { stats.Bytes = maxReportedTransferBytes + 1 }},
		{name: "negative duration", mutate: func(stats *transferStats) { stats.DurationMillis = -1 }},
		{name: "excessive duration", mutate: func(stats *transferStats) { stats.DurationMillis = maxReportedTransferDurationMillis + 1 }},
		{name: "unstructured failure", mutate: func(stats *transferStats) { stats.Error = "/private/secret/file.txt: permission denied" }},
		{name: "completed without path", mutate: func(stats *transferStats) { stats.Completed = true; stats.Transport = ""; stats.Candidate = "" }},
		{name: "completed with error", mutate: func(stats *transferStats) { stats.Completed = true; stats.Error = "network" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := valid
			tt.mutate(&stats)
			if _, err := validateAndSanitizeTransferStats("recv", stats); !errors.Is(err, errInvalidTransferStats) {
				t.Fatalf("error = %v; want %v", err, errInvalidTransferStats)
			}
		})
	}
}

// TestValidateAndSanitizeTransferStatsRedactsSecrets verifies dashboard text
// cannot disclose routing identifiers, pairing codes, or terminal controls.
func TestValidateAndSanitizeTransferStatsRedactsSecrets(t *testing.T) {
	sessionID, err := deriveMailboxSessionID(testPairingCode)
	if err != nil {
		t.Fatalf("derive session ID: %v", err)
	}
	stats := transferStats{
		Mode:          "send",
		DirectSummary: "code=" + strings.ToUpper(testPairingCode) + "\nroute=" + sessionID + "\x1b]2;forged\a",
		Error:         "network",
	}

	got, err := validateAndSanitizeTransferStats("send", stats)
	if err != nil {
		t.Fatalf("validate stats: %v", err)
	}
	for _, secret := range []string{testPairingCode, strings.ToUpper(testPairingCode), sessionID} {
		if strings.Contains(got.DirectSummary, secret) {
			t.Fatalf("sanitized summary contains secret %q: %q", secret, got.DirectSummary)
		}
	}
	if strings.ContainsAny(got.DirectSummary, "\n\r\a\x1b") {
		t.Fatalf("sanitized summary contains terminal controls: %q", got.DirectSummary)
	}
	if !strings.Contains(got.DirectSummary, "[redacted-code]") || !strings.Contains(got.DirectSummary, "[redacted-session]") {
		t.Fatalf("sanitized summary lacks redaction markers: %q", got.DirectSummary)
	}
	if got.Error != "network" {
		t.Fatalf("failure category = %q; want network", got.Error)
	}
}

// TestReportedFailureCategoryDoesNotExposeDetails verifies source paths and
// arbitrary peer-influenced error text never enter shared telemetry.
func TestReportedFailureCategoryDoesNotExposeDetails(t *testing.T) {
	const secretPath = "/private/secret/source-file.bin"
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", want: ""},
		{name: "canceled", err: context.Canceled, want: "canceled"},
		{name: "timeout", err: context.DeadlineExceeded, want: "timeout"},
		{name: "mailbox", err: errMailboxUnavailable, want: "mailbox"},
		{name: "authentication", err: errPeerKeyConfirmation, want: "authentication"},
		{name: "storage", err: &fs.PathError{Op: "open", Path: secretPath, Err: fs.ErrPermission}, want: "storage"},
		{name: "unclassified", err: errors.New("peer supplied " + secretPath), want: "transfer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reportedFailureCategory(tt.err)
			if got != tt.want {
				t.Fatalf("category = %q; want %q", got, tt.want)
			}
			if strings.Contains(got, secretPath) {
				t.Fatalf("category exposed local path: %q", got)
			}
		})
	}
}
