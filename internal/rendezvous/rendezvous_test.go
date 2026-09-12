package rendezvous

import (
	"errors"
	"net"
	"regexp"
	"strings"
	"testing"
)

// TestDefaultCodeFormat verifies generated codes use six unambiguous symbols
// drawn uniformly from the 30-symbol human-oriented alphabet.
func TestDefaultCodeFormat(t *testing.T) {
	rx := regexp.MustCompile(`^[2-9a-hj-km-np-tv-z]{3}-[2-9a-hj-km-np-tv-z]{3}$`)
	for i := 0; i < 10; i++ {
		code, err := defaultCode()
		if err != nil {
			t.Fatalf("defaultCode: %v", err)
		}
		if !rx.MatchString(code) {
			t.Fatalf("code %q does not match expected pattern", code)
		}
		if symbols := len(strings.ReplaceAll(code, "-", "")); symbols != 6 {
			t.Fatalf("code contains %d symbols; want 6", symbols)
		}
	}
}

// TestGenerateCodeFromUsesRejectionSampling verifies deterministic generation
// discards bytes outside the unbiased sampling range.
func TestGenerateCodeFromUsesRejectionSampling(t *testing.T) {
	code, err := generateCodeFrom(strings.NewReader(string([]byte{240, 255, 0, 1, 16, 17, 18, 29})))
	if err != nil {
		t.Fatalf("generate deterministic code: %v", err)
	}
	if code != "23j-kmz" {
		t.Fatalf("generated code = %q; want %q", code, "23j-kmz")
	}
}

// TestGeneratedCodeAlphabetIsUnambiguous verifies the generator and redactor
// share a unique, lowercase ASCII alphabet without commonly confused glyphs.
func TestGeneratedCodeAlphabetIsUnambiguous(t *testing.T) {
	if len(generatedCodeAlphabet) != 30 {
		t.Fatalf("alphabet size = %d; want 30", len(generatedCodeAlphabet))
	}
	seen := make(map[byte]bool, len(generatedCodeAlphabet))
	for index := range len(generatedCodeAlphabet) {
		symbol := generatedCodeAlphabet[index]
		isDigit := symbol >= '2' && symbol <= '9'
		isLowercase := symbol >= 'a' && symbol <= 'z'
		if symbol > 0x7f || !isDigit && !isLowercase {
			t.Fatalf("alphabet contains noncanonical symbol %q", symbol)
		}
		if strings.ContainsRune("01ilou", rune(symbol)) {
			t.Fatalf("alphabet contains ambiguous symbol %q", symbol)
		}
		if seen[symbol] {
			t.Fatalf("alphabet contains duplicate symbol %q", symbol)
		}
		seen[symbol] = true

		random := strings.NewReader(strings.Repeat(string([]byte{byte(index)}), generatedCodeSymbols))
		code, err := generateCodeFrom(random)
		if err != nil {
			t.Fatalf("generate code for alphabet index %d: %v", index, err)
		}
		want := strings.Repeat(string(symbol), generatedCodeGroupSize) + "-" +
			strings.Repeat(string(symbol), generatedCodeFinalGroupSize)
		if code != want {
			t.Fatalf("generated code for alphabet index %d = %q; want %q", index, code, want)
		}
		if !CodeTextPattern.MatchString(code) {
			t.Fatalf("redactor pattern does not match generated symbol %q", symbol)
		}
	}
}

type failingCodeReader struct{}

// Read always fails so code generation's fail-closed path can be tested.
func (failingCodeReader) Read([]byte) (int, error) {
	return 0, errors.New("random source unavailable")
}

type rejectedCodeReader struct{}

// Read fills every requested byte with a value outside the unbiased sampling
// range so bounded rejection behavior can be tested.
func (rejectedCodeReader) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = 255
	}
	return len(p), nil
}

// TestGenerateCodeFromFailsClosed verifies RNG failure never falls back to a
// predictable timestamp-derived pairing code.
func TestGenerateCodeFromFailsClosed(t *testing.T) {
	if code, err := generateCodeFrom(failingCodeReader{}); err == nil || code != "" {
		t.Fatalf("generateCodeFrom = %q, %v; want empty code and error", code, err)
	}
}

// TestGenerateCodeFromBoundsRejectionSampling verifies a broken random source
// cannot leave code generation spinning forever.
func TestGenerateCodeFromBoundsRejectionSampling(t *testing.T) {
	if code, err := generateCodeFrom(rejectedCodeReader{}); err == nil || code != "" {
		t.Fatalf("generateCodeFrom = %q, %v; want empty code and error", code, err)
	}
}

// TestNormalizeCodeAcceptsConvenientInput verifies ASCII letter case does not
// prevent two users from pairing when the required grouping is present.
func TestNormalizeCodeAcceptsConvenientInput(t *testing.T) {
	for input, want := range map[string]string{
		"23j-kmz": "23j-kmz",
		"23J-KMZ": "23j-kmz",
	} {
		if got, err := NormalizeCode(input); err != nil || got != want {
			t.Errorf("NormalizeCode(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

// TestNormalizeCodeRejectsMalformedOrLegacyInput verifies malformed and
// incompatible pairing codes fail before any value reaches the mailbox.
func TestNormalizeCodeRejectsMalformedOrLegacyInput(t *testing.T) {
	for _, invalid := range []string{
		"abcd-ef",
		"abcd-efgh",
		"mfrg-gzdf-mztwq",
		"23i-kmz",
		"23l-kmz",
		"23o-kmz",
		"230-kmz",
		"231-kmz",
		"23u-kmz",
		"23_-kmz",
		"23jkmz",
		"23jk-mz",
		"23j-Kmz",
	} {
		if _, err := NormalizeCode(invalid); err == nil {
			t.Fatalf("NormalizeCode accepted %q", invalid)
		}
	}
}

// TestNormalizeCodeExplainsVersionCutover gives mixed-version peers actionable
// guidance instead of reporting only a syntax mismatch.
func TestNormalizeCodeExplainsVersionCutover(t *testing.T) {
	_, err := NormalizeCode("mfrg-gzdf-mztwq")
	if err == nil {
		t.Fatal("NormalizeCode accepted the prior 64-bit format")
	}
	for _, expected := range []string{generatedCodeFormat, "update both Wormzy clients"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("NormalizeCode error %q does not contain %q", err, expected)
		}
	}
}

// TestCodeTextPatternMatchesOnlyWholeCodes verifies short-code redaction does
// not tear code-shaped substrings out of ordinary diagnostic words.
func TestCodeTextPatternMatchesOnlyWholeCodes(t *testing.T) {
	for _, code := range []string{"23j-kmz", "23J-KMZ"} {
		if !CodeTextPattern.MatchString("code=" + code) {
			t.Errorf("CodeTextPattern did not match %q", code)
		}
	}
	for _, ordinary := range []string{"direct-race", "send-ns", "recv-ns", "prefix23j-kmzsuffix", "23jkmz", "abcd-ef", "mfrg-gzdf-mztwq"} {
		if CodeTextPattern.MatchString(ordinary) {
			t.Errorf("CodeTextPattern matched ordinary text %q", ordinary)
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
