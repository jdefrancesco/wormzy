package transport

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

type shortCompletionWriter struct {
	buf bytes.Buffer
	max int
}

type failedCompletionLingerStream struct {
	deadline time.Time
}

// Read simulates a reset after application-level completion is already authenticated.
func (s *failedCompletionLingerStream) Read([]byte) (int, error) {
	return 0, errors.New("peer reset during teardown")
}

// SetReadDeadline records the bounded best-effort linger deadline.
func (s *failedCompletionLingerStream) SetReadDeadline(deadline time.Time) error {
	s.deadline = deadline
	return nil
}

// TestLingerAfterAuthenticatedTransferCompletionIgnoresTeardownError documents the success boundary.
func TestLingerAfterAuthenticatedTransferCompletionIgnoresTeardownError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream := &failedCompletionLingerStream{}
	lingerAfterAuthenticatedTransferCompletion(ctx, stream)
	if stream.deadline.IsZero() {
		t.Fatal("best-effort completion linger did not set a deadline")
	}
	if remaining := time.Until(stream.deadline); remaining <= 0 || remaining > transferCompletionLingerTimeout {
		t.Fatalf("best-effort completion linger deadline = %s; want at most %s", remaining, transferCompletionLingerTimeout)
	}
}

// Write accepts only a small prefix to exercise legal short writes.
func (w *shortCompletionWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.buf.Write(p)
}

// TestTransferCompletionProtocol_AuthenticatesBothRoles verifies the file-key-bound completion exchange.
func TestTransferCompletionProtocol_AuthenticatesBothRoles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	senderStream, receiverStream := net.Pipe()
	defer senderStream.Close()
	defer receiverStream.Close()

	key := []byte("01234567890123456789012345678901")
	digest := []byte("01234567890123456789012345678901")
	type result struct {
		mode string
		err  error
	}
	results := make(chan result, 2)
	go func() {
		results <- result{mode: "send", err: runTransferCompletionProtocol(ctx, senderStream, "send", "code-1", key, digest, 4096)}
	}()
	go func() {
		results <- result{mode: "recv", err: runTransferCompletionProtocol(ctx, receiverStream, "recv", "code-1", key, digest, 4096)}
	}()

	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s completion protocol: %v", result.mode, result.err)
		}
	}
}

// TestFinishTransferSession_RejectsLegacyPeer verifies completion cannot be silently downgraded.
func TestFinishTransferSession_RejectsLegacyPeer(t *testing.T) {
	peer := rendezvous.SelfInfo{Features: []string{featureICEv1}}
	for _, mode := range []string{"send", "recv"} {
		t.Run(mode, func(t *testing.T) {
			err := finishTransferSession(
				context.Background(),
				Config{Mode: mode},
				peer,
				nil,
				"",
				nil,
				nil,
				0,
				nil,
			)
			if err == nil || !strings.Contains(err.Error(), "update Wormzy") {
				t.Fatalf("legacy-peer %s error = %v; want upgrade guidance", mode, err)
			}
		})
	}
}

// TestTransferCompletionTimeout_IsBounded verifies receipt confirmation does not inherit a five-minute idle wait.
func TestTransferCompletionTimeout_IsBounded(t *testing.T) {
	if got := transferCompletionTimeout(defaultTransferIdleTO); got != defaultTransferCompletionTimeout {
		t.Fatalf("default completion timeout = %s; want %s", got, defaultTransferCompletionTimeout)
	}
	if got := transferCompletionTimeout(2 * time.Second); got != 2*time.Second {
		t.Fatalf("short idle completion timeout = %s; want 2s", got)
	}
}

// TestWriteTransferCompletionMessage_HandlesShortWrites verifies framing survives partial writer progress.
func TestWriteTransferCompletionMessage_HandlesShortWrites(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	digest := []byte("01234567890123456789012345678901")
	message, err := newTransferCompletionMessage("code-1", transferCompletionReceiverVerified, key, digest, 4096)
	if err != nil {
		t.Fatalf("newTransferCompletionMessage: %v", err)
	}
	short := &shortCompletionWriter{max: 3}
	if err := writeTransferCompletionMessage(short, message); err != nil {
		t.Fatalf("writeTransferCompletionMessage: %v", err)
	}
	decoded, err := readTransferCompletionMessage(&short.buf)
	if err != nil {
		t.Fatalf("readTransferCompletionMessage: %v", err)
	}
	if decoded != message {
		t.Fatalf("decoded message = %#v; want %#v", decoded, message)
	}
}

// TestTransferCompletionMessage_RejectsTampering verifies digest, size, role, and MAC binding.
func TestTransferCompletionMessage_RejectsTampering(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	digest := []byte("01234567890123456789012345678901")
	message, err := newTransferCompletionMessage("code-1", transferCompletionReceiverVerified, key, digest, 4096)
	if err != nil {
		t.Fatalf("newTransferCompletionMessage: %v", err)
	}
	if err := verifyTransferCompletionMessage(message, "code-1", transferCompletionReceiverVerified, key, digest, 4096); err != nil {
		t.Fatalf("verifyTransferCompletionMessage: %v", err)
	}

	tests := map[string]func() error{
		"role": func() error {
			tampered := message
			tampered.Role = transferCompletionSenderConfirmed
			return verifyTransferCompletionMessage(tampered, "code-1", transferCompletionReceiverVerified, key, digest, 4096)
		},
		"size": func() error {
			return verifyTransferCompletionMessage(message, "code-1", transferCompletionReceiverVerified, key, digest, 4097)
		},
		"digest": func() error {
			wrongDigest := append([]byte(nil), digest...)
			wrongDigest[0] ^= 0xff
			return verifyTransferCompletionMessage(message, "code-1", transferCompletionReceiverVerified, key, wrongDigest, 4096)
		},
		"MAC": func() error {
			tampered := message
			tampered.MAC = strings.Repeat("0", len(tampered.MAC))
			return verifyTransferCompletionMessage(tampered, "code-1", transferCompletionReceiverVerified, key, digest, 4096)
		},
	}
	for name, verify := range tests {
		t.Run(name, func(t *testing.T) {
			if err := verify(); err == nil {
				t.Fatal("accepted tampered completion message")
			}
		})
	}
}

// TestTransferCompletionMessage_HidesFileFingerprint verifies the relay sees no raw digest or size field.
func TestTransferCompletionMessage_HidesFileFingerprint(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	digest := []byte("01234567890123456789012345678901")
	message, err := newTransferCompletionMessage("code-1", transferCompletionReceiverVerified, key, digest, 4096)
	if err != nil {
		t.Fatalf("newTransferCompletionMessage: %v", err)
	}
	var raw bytes.Buffer
	if err := writeTransferCompletionMessage(&raw, message); err != nil {
		t.Fatalf("writeTransferCompletionMessage: %v", err)
	}
	wire := raw.Bytes()
	for _, forbidden := range [][]byte{[]byte(`"digest"`), []byte(`"size"`), []byte(hex.EncodeToString(digest))} {
		if bytes.Contains(wire, forbidden) {
			t.Fatalf("completion wire message exposed %q", forbidden)
		}
	}
}

// TestWaitForTransferCompletionEOFRequiresCleanFIN verifies the completion
// stream synchronization rejects data after the authenticated exchange.
func TestWaitForTransferCompletionEOFRequiresCleanFIN(t *testing.T) {
	if err := waitForTransferCompletionEOF(strings.NewReader("")); err != nil {
		t.Fatalf("clean completion FIN: %v", err)
	}
	if err := waitForTransferCompletionEOF(strings.NewReader("unexpected")); err == nil {
		t.Fatal("accepted trailing completion-stream data")
	}
}
