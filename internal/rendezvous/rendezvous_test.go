package rendezvous

import (
	"encoding/base32"
	"errors"
	"net"
	"regexp"
	"strings"
	"testing"
)

// TestDefaultCodeFormat verifies generated codes preserve 64 bits in the
// copy-friendly grouped representation.
func TestDefaultCodeFormat(t *testing.T) {
	rx := regexp.MustCompile(`^[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{5}$`)
	for i := 0; i < 10; i++ {
		code, err := defaultCode()
		if err != nil {
			t.Fatalf("defaultCode: %v", err)
		}
		if !rx.MatchString(code) {
			t.Fatalf("code %q does not match expected pattern", code)
		}
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(
			strings.ToUpper(strings.ReplaceAll(code, "-", "")),
		)
		if err != nil {
			t.Fatalf("decode code %q: %v", code, err)
		}
		if bits := len(decoded) * 8; bits != 64 {
			t.Fatalf("decoded code has %d bits; want 64", bits)
		}
	}
}

type failingCodeReader struct{}

// Read always fails so code generation's fail-closed path can be tested.
func (failingCodeReader) Read([]byte) (int, error) {
	return 0, errors.New("random source unavailable")
}

// TestGenerateCodeFromFailsClosed verifies RNG failure never falls back to a
// predictable timestamp-derived pairing code.
func TestGenerateCodeFromFailsClosed(t *testing.T) {
	if code, err := generateCodeFrom(failingCodeReader{}); err == nil || code != "" {
		t.Fatalf("generateCodeFrom = %q, %v; want empty code and error", code, err)
	}
}

// TestNormalizeCodeRejectsWeakOrMalformedInput verifies custom codes cannot
// silently reduce the pairing secret's entropy.
func TestNormalizeCodeRejectsWeakOrMalformedInput(t *testing.T) {
	code, err := generateCodeFrom(strings.NewReader("01234567"))
	if err != nil {
		t.Fatalf("generate deterministic code: %v", err)
	}
	upper := strings.ToUpper(code)
	if got, err := NormalizeCode(upper); err != nil || got != code {
		t.Fatalf("NormalizeCode(%q) = %q, %v; want %q", upper, got, err, code)
	}
	for _, invalid := range []string{
		"abcd-ef",
		"abcd-efgh",
		"abcd-efgh-ijkl",
		"abcd-efgh-ijklm-nopq",
		"abcd-efgh-ijkl1",
		"mfrg-gzdf-mztwr",
	} {
		if _, err := NormalizeCode(invalid); err == nil {
			t.Fatalf("NormalizeCode accepted %q", invalid)
		}
	}
}

// TestNormalizeCodeExplainsVersionCutover gives mixed-version peers actionable
// guidance instead of reporting only a syntax mismatch.
func TestNormalizeCodeExplainsVersionCutover(t *testing.T) {
	_, err := NormalizeCode("abcd-efgh")
	if err == nil {
		t.Fatal("NormalizeCode accepted the prior two-group format")
	}
	for _, expected := range []string{generatedCodeFormat, "update both Wormzy clients"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("NormalizeCode error %q does not contain %q", err, expected)
		}
	}
}

func TestWaitingLifecycle(t *testing.T) {
	w := &waiting{}

	sender, recv := net.Pipe()
	defer sender.Close()
	defer recv.Close()

	if err := w.setSender(sender); err != nil {
		t.Fatalf("unexpected error setting sender: %v", err)
	}
	if err := w.setSender(sender); err == nil {
		t.Fatalf("expected error re-setting sender")
	}

	if err := w.setReceiver(recv); err != nil {
		t.Fatalf("unexpected error setting receiver: %v", err)
	}
	if err := w.setReceiver(recv); err == nil {
		t.Fatalf("expected error re-setting receiver")
	}

	info := &SelfInfo{Public: "1.2.3.4:1234", Local: "10.0.0.1:5678"}
	w.setSenderInfo(info)
	w.setReceiverInfo(info)

	sendConn, sendInfo, recvConn, recvInfo, ok := w.snapshot()
	if !ok || sendConn != sender || recvConn != recv {
		t.Fatalf("unexpected snapshot: %v %v", ok, w)
	}
	if sendInfo.Public != info.Public || recvInfo.Local != info.Local {
		t.Fatalf("snapshot missed self info")
	}

	w.clear()
	if !w.isClosed() {
		t.Fatalf("waiting should be closed after clear")
	}
}
