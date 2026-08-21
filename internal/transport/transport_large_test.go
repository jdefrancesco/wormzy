package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
)

type tReporter struct{ t *testing.T }

func (r tReporter) Logf(format string, args ...interface{}) {
	r.t.Logf(format, args...)
}

func (r tReporter) Stage(stage Stage, state StageState, detail string) {
	if stage == StageTransfer && state == StageStateRunning {
		return
	}
	r.t.Logf("stage %s %v %s", stage, state, detail)
}

// TestLargeTransferLoopback verifies a multi-MiB transfer with idle timeouts enforced.
func TestLargeTransferLoopback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large transfer in short mode")
	}
	srcData := make([]byte, 2*1024*1024)
	if _, err := rand.Read(srcData); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	testTransferLoopback(t, "large.bin", srcData)
}

// TestEmptyTransferLoopback verifies metadata follows the header directly for a zero-byte file.
func TestEmptyTransferLoopback(t *testing.T) {
	testTransferLoopback(t, "empty.bin", nil)
}

// testTransferLoopback transfers payload through the complete loopback protocol and compares it on disk.
func testTransferLoopback(t *testing.T, filename string, srcData []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, filename)
	if err := os.WriteFile(srcPath, srcData, 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	code := testPairingCode
	idle := 20 * time.Second

	recvDir := filepath.Join(tmpDir, "recv")
	if err := os.MkdirAll(recvDir, 0o755); err != nil {
		t.Fatalf("mkdir recv: %v", err)
	}

	sendCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Config{
			Mode:        "send",
			FilePath:    srcPath,
			Code:        code,
			RelayAddr:   mini.Addr(),
			Loopback:    true,
			IdleTimeout: idle,
		}, tReporter{t})
		sendCh <- err
	}()

	time.Sleep(200 * time.Millisecond)

	_, err = Run(ctx, Config{
		Mode:        "recv",
		Code:        code,
		RelayAddr:   mini.Addr(),
		Loopback:    true,
		IdleTimeout: idle,
		DownloadDir: recvDir,
	}, tReporter{t})
	if err != nil {
		t.Fatalf("receiver run: %v", err)
	}

	if err := <-sendCh; err != nil {
		t.Fatalf("sender run: %v", err)
	}

	dstPath := filepath.Join(recvDir, filename)
	dstData, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(dstData, srcData) {
		t.Fatalf("content mismatch after transfer")
	}
}
