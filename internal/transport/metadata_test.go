package transport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zeebo/blake3"
	"golang.org/x/crypto/chacha20poly1305"
)

// TestVerifyMetadataAcceptsOneValidTrailer verifies the receiver accepts one
// complete trailer followed immediately by the encrypted stream EOF.
func TestVerifyMetadataAcceptsOneValidTrailer(t *testing.T) {
	digest := blake3.Sum256([]byte("payload"))
	meta := fileMetadata{
		Hash:      "blake3-256",
		ChunkSize: uint32(chunkSize),
		Size:      7,
		Digest:    digest[:],
	}
	reader := metadataTestReader(t, metadataTestChunk(t, meta))

	if err := verifyMetadata(reader, digest[:], 7, uint32(chunkSize)); err != nil {
		t.Fatalf("verifyMetadata returned error: %v", err)
	}
}

// TestVerifyMetadataRejectsInvalidTrailers exercises every authenticated
// metadata invariant and rejects data after the sole permitted trailer.
func TestVerifyMetadataRejectsInvalidTrailers(t *testing.T) {
	digest := blake3.Sum256([]byte("payload"))
	valid := fileMetadata{
		Hash:      "blake3-256",
		ChunkSize: uint32(chunkSize),
		Size:      7,
		Digest:    digest[:],
	}

	tests := []struct {
		name    string
		chunks  func(*testing.T) [][]byte
		wantErr string
	}{
		{
			name:    "missing trailer",
			chunks:  func(*testing.T) [][]byte { return nil },
			wantErr: "missing file metadata trailer",
		},
		{
			name: "unexpected trailer",
			chunks: func(*testing.T) [][]byte {
				return [][]byte{[]byte("not metadata")}
			},
			wantErr: "unexpected trailer data",
		},
		{
			name: "unsupported hash",
			chunks: func(t *testing.T) [][]byte {
				meta := valid
				meta.Hash = "sha256"
				return [][]byte{metadataTestChunk(t, meta)}
			},
			wantErr: "unsupported file hash",
		},
		{
			name: "empty digest",
			chunks: func(t *testing.T) [][]byte {
				meta := valid
				meta.Digest = nil
				return [][]byte{metadataTestChunk(t, meta)}
			},
			wantErr: "invalid file digest length",
		},
		{
			name: "short digest",
			chunks: func(t *testing.T) [][]byte {
				meta := valid
				meta.Digest = digest[:len(digest)-1]
				return [][]byte{metadataTestChunk(t, meta)}
			},
			wantErr: "invalid file digest length",
		},
		{
			name: "long digest",
			chunks: func(t *testing.T) [][]byte {
				meta := valid
				meta.Digest = append(append([]byte(nil), digest[:]...), 0)
				return [][]byte{metadataTestChunk(t, meta)}
			},
			wantErr: "invalid file digest length",
		},
		{
			name: "digest mismatch",
			chunks: func(t *testing.T) [][]byte {
				meta := valid
				meta.Digest = append([]byte(nil), digest[:]...)
				meta.Digest[0] ^= 0xff
				return [][]byte{metadataTestChunk(t, meta)}
			},
			wantErr: "file hash mismatch",
		},
		{
			name: "size mismatch",
			chunks: func(t *testing.T) [][]byte {
				meta := valid
				meta.Size++
				return [][]byte{metadataTestChunk(t, meta)}
			},
			wantErr: "file size mismatch",
		},
		{
			name: "chunk size mismatch",
			chunks: func(t *testing.T) [][]byte {
				meta := valid
				meta.ChunkSize--
				return [][]byte{metadataTestChunk(t, meta)}
			},
			wantErr: "file chunk size mismatch",
		},
		{
			name: "trailing encrypted chunk",
			chunks: func(t *testing.T) [][]byte {
				return [][]byte{metadataTestChunk(t, valid), []byte("trailing")}
			},
			wantErr: "encrypted chunk after file metadata trailer",
		},
		{
			name: "duplicate trailer",
			chunks: func(t *testing.T) [][]byte {
				trailer := metadataTestChunk(t, valid)
				return [][]byte{trailer, trailer}
			},
			wantErr: "encrypted chunk after file metadata trailer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := metadataTestReader(t, test.chunks(t)...)
			err := verifyMetadata(reader, digest[:], 7, uint32(chunkSize))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("verifyMetadata error = %v; want error containing %q", err, test.wantErr)
			}
		})
	}
}

// metadataTestChunk encodes one file metadata value with its wire prefix.
func metadataTestChunk(t *testing.T, meta fileMetadata) []byte {
	t.Helper()
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return append([]byte(metaPrefix), payload...)
}

// metadataTestReader encrypts the supplied plaintext chunks into an in-memory
// stream and returns a reader positioned at its first frame.
func metadataTestReader(t *testing.T, chunks ...[]byte) *aeadReader {
	t.Helper()
	aead, err := chacha20poly1305.NewX(make([]byte, chacha20poly1305.KeySize))
	if err != nil {
		t.Fatalf("chacha20poly1305.NewX: %v", err)
	}
	var wire bytes.Buffer
	writer := &aeadWriter{w: &wire, aead: aead}
	for _, chunk := range chunks {
		if err := writer.WriteChunk(chunk); err != nil {
			t.Fatalf("WriteChunk: %v", err)
		}
	}
	return &aeadReader{r: bytes.NewReader(wire.Bytes()), aead: aead}
}
