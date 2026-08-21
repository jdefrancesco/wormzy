package transport

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// TestGenerateMailboxCapability verifies canonical, distinct, verifier-only capability material.
func TestGenerateMailboxCapability(t *testing.T) {
	rawOne, verifierOne, err := generateMailboxCapability(nil)
	if err != nil {
		t.Fatalf("generate first capability: %v", err)
	}
	rawTwo, verifierTwo, err := generateMailboxCapability(nil)
	if err != nil {
		t.Fatalf("generate second capability: %v", err)
	}
	if rawOne == rawTwo || verifierOne == verifierTwo {
		t.Fatal("independent mailboxes generated the same capability")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rawOne)
	if err != nil || len(decoded) != mailboxCapabilitySize {
		t.Fatalf("raw capability length = %d, error = %v", len(decoded), err)
	}
	if rawOne == verifierOne {
		t.Fatal("stored verifier exposes the raw capability")
	}
	if err := validateMailboxCapabilityVerifier(verifierOne); err != nil {
		t.Fatalf("validate verifier: %v", err)
	}
	if got, err := mailboxCapabilityVerifier(rawOne); err != nil || got != verifierOne {
		t.Fatalf("derived verifier = %q, %v; want %q", got, err, verifierOne)
	}
}

// TestGenerateMailboxCapabilityFailsClosed verifies entropy failures never yield partial credentials.
func TestGenerateMailboxCapabilityFailsClosed(t *testing.T) {
	raw, verifier, err := generateMailboxCapability(iotest.ErrReader(errors.New("entropy unavailable")))
	if err == nil {
		t.Fatal("expected entropy failure")
	}
	if raw != "" || verifier != "" {
		t.Fatalf("partial credential returned: raw=%q verifier=%q", raw, verifier)
	}
}

// TestMailboxConstructorsUseDistinctCapabilities verifies each client owns independent credentials.
func TestMailboxConstructorsUseDistinctCapabilities(t *testing.T) {
	directOne, err := newRedisMailboxWithClient(nil, time.Minute, "send", nil)
	if err != nil {
		t.Fatalf("first direct mailbox: %v", err)
	}
	directTwo, err := newRedisMailboxWithClient(nil, time.Minute, "send", nil)
	if err != nil {
		t.Fatalf("second direct mailbox: %v", err)
	}
	httpOne := newHTTPMailbox("http://127.0.0.1", "send", time.Second).(*httpMailbox)
	httpTwo := newHTTPMailbox("http://127.0.0.1", "send", time.Second).(*httpMailbox)
	for name, pair := range map[string][2]string{
		"direct raw":      {directOne.capability, directTwo.capability},
		"direct verifier": {directOne.capabilityHash, directTwo.capabilityHash},
		"HTTP raw":        {httpOne.capability, httpTwo.capability},
		"HTTP verifier":   {httpOne.capabilityHash, httpTwo.capabilityHash},
	} {
		if pair[0] == "" || pair[1] == "" || pair[0] == pair[1] {
			t.Fatalf("%s credentials are not distinct: %q / %q", name, pair[0], pair[1])
		}
	}
}

// TestMailboxCapabilityValidationRejectsMalformedValues verifies strict raw and verifier encodings.
func TestMailboxCapabilityValidationRejectsMalformedValues(t *testing.T) {
	raw, verifier, err := generateMailboxCapability(bytes.NewReader(bytes.Repeat([]byte{0x5a}, mailboxCapabilitySize)))
	if err != nil {
		t.Fatalf("generate capability: %v", err)
	}
	noncanonical := strings.Repeat("A", 42) + "B"
	for _, malformed := range []string{"", raw + "=", raw[:len(raw)-1], "not base64url!", noncanonical} {
		if _, err := mailboxCapabilityVerifier(malformed); !errors.Is(err, errMailboxAuthentication) {
			t.Fatalf("raw %q error = %v; want authentication failure", malformed, err)
		}
	}
	for _, malformed := range []string{"", verifier + "=", verifier[:len(verifier)-1], "not base64url!", noncanonical} {
		if err := validateMailboxCapabilityVerifier(malformed); !errors.Is(err, errMailboxUnavailable) {
			t.Fatalf("verifier %q error = %v; want unavailable", malformed, err)
		}
	}
	if mailboxCapabilityVerifierEqual(verifier, mailboxCapabilityVerifierBytes(bytes.Repeat([]byte{0x6b}, mailboxCapabilitySize))) {
		t.Fatal("different capability verifiers compared equal")
	}
	if !mailboxCapabilityVerifierEqual(verifier, verifier) {
		t.Fatal("identical capability verifiers did not compare equal")
	}
	if mailboxCapabilityVerifierEqual(strings.Repeat("A", 43), noncanonical) {
		t.Fatal("noncanonical verifier compared equal to its canonical encoding")
	}
}

// TestMailboxAuthorizationVerifier verifies strict Bearer syntax and verifier derivation.
func TestMailboxAuthorizationVerifier(t *testing.T) {
	raw, verifier, err := generateMailboxCapability(bytes.NewReader(bytes.Repeat([]byte{0x7c}, mailboxCapabilitySize)))
	if err != nil {
		t.Fatalf("generate capability: %v", err)
	}
	if got, err := mailboxAuthorizationVerifier("Bearer " + raw); err != nil || got != verifier {
		t.Fatalf("authorization verifier = %q, %v; want %q", got, err, verifier)
	}
	for _, header := range []string{"", raw, "bearer " + raw, "Bearer", "Bearer " + raw + " extra"} {
		if _, err := mailboxAuthorizationVerifier(header); !errors.Is(err, errMailboxAuthentication) {
			t.Fatalf("header %q error = %v; want authentication failure", header, err)
		}
	}
}
