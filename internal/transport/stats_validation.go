package transport

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxReportedTransferBytes          int64 = 1 << 50
	maxReportedTransferDurationMillis int64 = 30 * 24 * 60 * 60 * 1000
	maxReportedStatsTextBytes               = 2048
)

var (
	errInvalidTransferStats = errors.New("invalid transfer statistics")
	pairingCodeTextPattern  = regexp.MustCompile(`(?i)[a-z2-7]{4}(?:-[a-z2-7]{4}){4}`)
	mailboxIDTextPattern    = regexp.MustCompile(`mbx2-[A-Za-z0-9_-]{43}`)
)

// validateAndSanitizeTransferStats validates client-reported dimensions and
// bounds operator-visible text before it reaches shared Redis state.
func validateAndSanitizeTransferStats(role string, stats transferStats) (transferStats, error) {
	if err := validateMailboxRole(role); err != nil {
		return transferStats{}, fmt.Errorf("%w: role", errInvalidTransferStats)
	}
	if stats.Mode != role {
		return transferStats{}, fmt.Errorf("%w: mode does not match authenticated role", errInvalidTransferStats)
	}
	if !validReportedTransport(stats.Transport) {
		return transferStats{}, fmt.Errorf("%w: transport", errInvalidTransferStats)
	}
	if !validReportedCandidate(stats.Candidate) {
		return transferStats{}, fmt.Errorf("%w: candidate", errInvalidTransferStats)
	}
	if !validReportedDirectOutcome(stats.DirectOutcome) {
		return transferStats{}, fmt.Errorf("%w: direct outcome", errInvalidTransferStats)
	}
	if (stats.Transport == "") != (stats.Candidate == "") {
		return transferStats{}, fmt.Errorf("%w: incomplete path", errInvalidTransferStats)
	}
	if stats.Transport != "" && stats.Transport != transportLabelForReportedCandidate(stats.Candidate) {
		return transferStats{}, fmt.Errorf("%w: path mismatch", errInvalidTransferStats)
	}
	if stats.Bytes < 0 || stats.Bytes > maxReportedTransferBytes {
		return transferStats{}, fmt.Errorf("%w: byte count", errInvalidTransferStats)
	}
	if stats.DurationMillis < 0 || stats.DurationMillis > maxReportedTransferDurationMillis {
		return transferStats{}, fmt.Errorf("%w: duration", errInvalidTransferStats)
	}
	if !validReportedFailure(stats.Error) {
		return transferStats{}, fmt.Errorf("%w: failure category", errInvalidTransferStats)
	}

	stats.DirectSummary = sanitizeReportedStatsText(stats.DirectSummary)
	if stats.Completed && (stats.Transport == "" || stats.Candidate == "" || stats.Error != "") {
		return transferStats{}, fmt.Errorf("%w: inconsistent completion", errInvalidTransferStats)
	}
	return stats, nil
}

// validReportedFailure accepts only privacy-safe failure categories emitted by clients.
func validReportedFailure(category string) bool {
	switch category {
	case "", "canceled", "timeout", "mailbox", "authentication", "network", "storage", "transfer":
		return true
	default:
		return false
	}
}

// reportedFailureCategory maps detailed local errors to bounded operator telemetry.
func reportedFailureCategory(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, errMailboxAuthentication) || errors.Is(err, errMailboxUnavailable) ||
		errors.Is(err, errInvalidMailboxEndpoint) {
		return "mailbox"
	}
	if errors.Is(err, errPeerKeyConfirmation) || errors.Is(err, errRelayPinMismatch) {
		return "authentication"
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return "storage"
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return "network"
	}
	return "transfer"
}

// validReportedTransport reports whether a transport label is a known dashboard dimension.
func validReportedTransport(transport string) bool {
	switch transport {
	case "", "p2p", "relay":
		return true
	default:
		return false
	}
}

// validReportedCandidate reports whether a candidate label can be emitted by a current client.
func validReportedCandidate(candidate string) bool {
	switch candidate {
	case "", "loopback", "local", "reflexive", "upnp", "relay",
		"legacy-public", "legacy-local", "direct-unknown", "ice-p2p", "ice-relay":
		return true
	default:
		return false
	}
}

// validReportedDirectOutcome reports whether an outcome label is a known dashboard dimension.
func validReportedDirectOutcome(outcome string) bool {
	switch outcome {
	case "", "pending", "won", "quic-timeout", "no-response", "noise-failed":
		return true
	default:
		return false
	}
}

// transportLabelForReportedCandidate derives the only transport category valid for a candidate.
func transportLabelForReportedCandidate(candidate string) string {
	if candidate == "relay" || candidate == "ice-relay" {
		return "relay"
	}
	if candidate == "" {
		return ""
	}
	return "p2p"
}

// sanitizeReportedStatsText redacts secrets, removes terminal controls, and
// caps untrusted text without splitting a UTF-8 encoding.
func sanitizeReportedStatsText(value string) string {
	value = pairingCodeTextPattern.ReplaceAllString(value, "[redacted-code]")
	value = mailboxIDTextPattern.ReplaceAllString(value, "[redacted-session]")

	var out strings.Builder
	out.Grow(min(len(value), maxReportedStatsTextBytes))
	for _, r := range value {
		if unicode.Is(unicode.C, r) {
			r = ' '
		}
		size := utf8.RuneLen(r)
		if size < 0 || out.Len()+size > maxReportedStatsTextBytes {
			break
		}
		out.WriteRune(r)
	}
	return strings.TrimSpace(out.String())
}
