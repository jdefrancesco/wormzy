package transport

import (
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
	r.t.Logf("stage %s %v %s", stage, state, detail)
}

// Integration-ish check: loopback transfer of a multi-MB file with idle timeouts enforced.
// Skipped under -short to keep CI quick.
func TestLargeTransferLoopback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large transfer in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()

	// Build a ~8 MiB random file.
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "large.bin")
	srcData := make([]byte, 8*1024*1024)
	if _, err := rand.Read(srcData); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(srcPath, srcData, 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	code := "test-large-01"
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

	dstPath := filepath.Join(recvDir, "large.bin")
	dstData, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if len(dstData) != len(srcData) {
		t.Fatalf("size mismatch: src %d dst %d", len(srcData), len(dstData))
	}
	if string(dstData) != string(srcData) {
		t.Fatalf("content mismatch after transfer")
	}
}
