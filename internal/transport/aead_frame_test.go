package transport

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

// TestAEADReaderRejectsOversizedFrameBeforeAllocation verifies relay-visible
// framing cannot request an attacker-controlled allocation.
func TestAEADReaderRejectsOversizedFrameBeforeAllocation(t *testing.T) {
	aead, err := chacha20poly1305.NewX(make([]byte, chacha20poly1305.KeySize))
	if err != nil {
		t.Fatalf("create AEAD: %v", err)
	}
	var wire bytes.Buffer
	if err := binary.Write(&wire, binary.BigEndian, uint32(maxAEADCiphertextSize+1)); err != nil {
		t.Fatalf("write hostile length: %v", err)
	}

	reader := &aeadReader{r: &wire, aead: aead}
	if _, err := reader.ReadChunk(); err == nil || !strings.Contains(err.Error(), "invalid encrypted frame size") {
		t.Fatalf("ReadChunk error = %v; want bounded-frame rejection", err)
	}
}

// TestAEADWriterRejectsOversizedPlaintext verifies locally generated frames
// cannot overflow or violate the encrypted framing contract.
func TestAEADWriterRejectsOversizedPlaintext(t *testing.T) {
	aead, err := chacha20poly1305.NewX(make([]byte, chacha20poly1305.KeySize))
	if err != nil {
		t.Fatalf("create AEAD: %v", err)
	}
	writer := &aeadWriter{w: &bytes.Buffer{}, aead: aead}
	if err := writer.WriteChunk(make([]byte, maxAEADPlaintextSize+1)); err == nil {
		t.Fatal("WriteChunk accepted oversized plaintext")
	}
}
