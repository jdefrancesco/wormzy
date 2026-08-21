package transport

import (
	"bytes"
	"errors"
	"net"
	"testing"
)

// TestConfirmPeerPSKRoundTrip verifies both QUIC roles prove possession of the
// PAKE key before the Noise and file streams can proceed.
func TestConfirmPeerPSKRoundTrip(t *testing.T) {
	initiator, responder := net.Pipe()
	defer initiator.Close()
	defer responder.Close()
	psk := bytes.Repeat([]byte{0x42}, 32)
	results := make(chan error, 2)

	go func() {
		results <- confirmPeerPSK(initiator, true, psk, bytes.NewReader(bytes.Repeat([]byte{0x11}, peerConfirmationNonceSize)))
	}()
	go func() {
		results <- confirmPeerPSK(responder, false, psk, bytes.NewReader(bytes.Repeat([]byte{0x22}, peerConfirmationNonceSize)))
	}()

	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("confirmation side %d: %v", i, err)
		}
	}
}

// TestConfirmPeerPSKRejectsMismatchedKeys verifies a network racer without the
// PAKE key cannot reach Noise or file-stream setup.
func TestConfirmPeerPSKRejectsMismatchedKeys(t *testing.T) {
	initiator, responder := net.Pipe()
	results := make(chan error, 2)

	go func() {
		err := confirmPeerPSK(initiator, true, bytes.Repeat([]byte{0x41}, 32), bytes.NewReader(bytes.Repeat([]byte{0x11}, peerConfirmationNonceSize)))
		_ = initiator.Close()
		results <- err
	}()
	go func() {
		err := confirmPeerPSK(responder, false, bytes.Repeat([]byte{0x42}, 32), bytes.NewReader(bytes.Repeat([]byte{0x22}, peerConfirmationNonceSize)))
		_ = responder.Close()
		results <- err
	}()

	matchedFailure := false
	for i := 0; i < 2; i++ {
		err := <-results
		if errors.Is(err, errPeerKeyConfirmation) {
			matchedFailure = true
		}
		if err == nil {
			t.Fatalf("confirmation side %d unexpectedly accepted mismatched keys", i)
		}
	}
	if !matchedFailure {
		t.Fatal("neither side reported a key-confirmation failure")
	}
}
