package transport

import (
	"strings"
	"testing"
)

const testPairingCode = "gayt-emzu-gu3d-oobz-mfra"

// TestDeriveMailboxSessionIDHidesPairingCode verifies routing is stable,
// domain-separated, and does not carry the human secret verbatim.
func TestDeriveMailboxSessionIDHidesPairingCode(t *testing.T) {
	first, err := deriveMailboxSessionID(testPairingCode)
	if err != nil {
		t.Fatalf("derive mailbox session ID: %v", err)
	}
	second, err := deriveMailboxSessionID(strings.ToUpper(testPairingCode))
	if err != nil {
		t.Fatalf("derive normalized mailbox session ID: %v", err)
	}
	if first != second {
		t.Fatalf("normalized IDs differ: %q != %q", first, second)
	}
	if !validMailboxSessionID(first) {
		t.Fatalf("derived ID %q is invalid", first)
	}
	if strings.Contains(first, strings.ReplaceAll(testPairingCode, "-", "")) {
		t.Fatalf("derived ID exposes pairing code: %q", first)
	}
	if alias := mailboxSessionAlias(first); !strings.HasPrefix(alias, "m-") || len(alias) != 10 {
		t.Fatalf("session alias = %q; want m- plus eight characters", alias)
	}
}

// TestValidMailboxSessionIDRejectsNoncanonicalEncoding verifies alternate base64 spellings cannot select a key.
func TestValidMailboxSessionIDRejectsNoncanonicalEncoding(t *testing.T) {
	canonical := mailboxSessionIDPrefix + strings.Repeat("A", 43)
	if !validMailboxSessionID(canonical) {
		t.Fatalf("canonical zero identifier %q was rejected", canonical)
	}
	noncanonical := canonical[:len(canonical)-1] + "B"
	if validMailboxSessionID(noncanonical) {
		t.Fatalf("noncanonical identifier %q was accepted", noncanonical)
	}
}

// TestNormalizeConfiguredPairingCodeRequiresCurrentFormat verifies weak legacy
// codes are rejected before any value reaches the mailbox.
func TestNormalizeConfiguredPairingCodeRequiresCurrentFormat(t *testing.T) {
	if _, err := normalizeConfiguredPairingCode("recv", "quick-code"); err == nil {
		t.Fatal("accepted legacy low-entropy pairing code")
	}
	if _, err := normalizeConfiguredPairingCode("recv", ""); err == nil {
		t.Fatal("accepted empty receiver pairing code")
	}
	if got, err := normalizeConfiguredPairingCode("recv", strings.ToUpper(testPairingCode)); err != nil || got != testPairingCode {
		t.Fatalf("normalized code = %q, %v; want %q", got, err, testPairingCode)
	}
	generated, err := normalizeConfiguredPairingCode("send", "")
	if err != nil {
		t.Fatalf("generate sender code: %v", err)
	}
	if _, err := deriveMailboxSessionID(generated); err != nil {
		t.Fatalf("generated code is unusable: %v", err)
	}
}
