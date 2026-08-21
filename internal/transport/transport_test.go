package transport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode"
)

// TestCreateDownloadFileReservesUniquePaths verifies concurrent receives cannot share an inode.
func TestCreateDownloadFileReservesUniquePaths(t *testing.T) {
	dir := t.TempDir()
	const workers = 4
	paths := make(chan string, workers)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			file, path, _, err := createDownloadFile(dir, "payload.bin")
			if err != nil {
				errs <- err
				return
			}
			if err := file.Close(); err != nil {
				errs <- err
				return
			}
			paths <- path
		}()
	}
	group.Wait()
	close(paths)
	close(errs)
	for err := range errs {
		t.Fatalf("createDownloadFile failed: %v", err)
	}
	seen := make(map[string]struct{}, workers)
	for path := range paths {
		if _, exists := seen[path]; exists {
			t.Fatalf("destination was reserved twice: %s", path)
		}
		seen[path] = struct{}{}
	}
	if len(seen) != workers {
		t.Fatalf("reserved %d paths; want %d", len(seen), workers)
	}
}

// TestCreateDownloadFileDoesNotFollowExistingSymlink verifies collision handling protects its target.
func TestCreateDownloadFileDoesNotFollowExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "target.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "payload.txt")); err != nil {
		t.Fatal(err)
	}

	file, path, renamed, err := createDownloadFile(dir, "payload.txt")
	if err != nil {
		t.Fatalf("createDownloadFile failed: %v", err)
	}
	if _, err := file.Write([]byte("received")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if !renamed || filepath.Base(path) != "payload (wormzy-1).txt" {
		t.Fatalf("unexpected collision path %q (renamed=%t)", path, renamed)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "keep" {
		t.Fatalf("symlink target was modified: %q", contents)
	}
}

// TestSanitizeFilenameRemovesTerminalControls verifies peer names cannot inject terminal sequences.
func TestSanitizeFilenameRemovesTerminalControls(t *testing.T) {
	got := sanitizeFilename("report\x1b[31m\n\u202e.txt")
	for _, char := range got {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf) {
			t.Fatalf("sanitized filename still contains terminal control %U", char)
		}
	}
	if strings.ContainsAny(got, "/\\") {
		t.Fatalf("sanitized filename still contains a path separator: %q", got)
	}
}

// TestCreateDownloadFile_NoConflict verifies the original filename is reserved when available.
func TestCreateDownloadFile_NoConflict(t *testing.T) {
	dir := t.TempDir()
	file, got, renamed, err := createDownloadFile(dir, "example.txt")
	if err != nil {
		t.Fatalf("createDownloadFile returned error: %v", err)
	}
	defer file.Close()
	want := filepath.Join(dir, "example.txt")
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
	if renamed {
		t.Fatalf("expected renamed=false")
	}
}

// TestCreateDownloadFile_WithConflicts verifies each collision advances the numbered suffix.
func TestCreateDownloadFile_WithConflicts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}
	file, got, renamed, err := createDownloadFile(dir, "example.txt")
	if err != nil {
		t.Fatalf("createDownloadFile returned error: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if !renamed {
		t.Fatalf("expected renamed=true when collision occurs")
	}
	want := filepath.Join(dir, "example (wormzy-1).txt")
	if got != want {
		t.Fatalf("unexpected path. want %s got %s", want, got)
	}

	file, got, renamed, err = createDownloadFile(dir, "example.txt")
	if err != nil {
		t.Fatalf("createDownloadFile returned error: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(dir, "example (wormzy-2).txt")
	if got != want {
		t.Fatalf("unexpected path. want %s got %s", want, got)
	}
	if !renamed {
		t.Fatalf("expected renamed=true for subsequent collisions")
	}
	t.Logf("conflict resolved to %s", got)
}

// TestCreateDownloadFile_RandomFallback verifies exhaustion uses an atomically reserved random tag.
func TestCreateDownloadFile_RandomFallback(t *testing.T) {
	dir := t.TempDir()
	// Seed the base file plus 99 numbered variants to exhaust the deterministic loop.
	seeds := []string{"example.txt"}
	for i := 1; i <= 99; i++ {
		seeds = append(seeds, fmt.Sprintf("example (wormzy-%d).txt", i))
	}
	for _, name := range seeds {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("failed to seed %s: %v", name, err)
		}
	}

	file, got, renamed, err := createDownloadFile(dir, "example.txt")
	if err != nil {
		t.Fatalf("createDownloadFile returned error: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if !renamed {
		t.Fatalf("expected renamed=true after exhausting deterministic suffixes")
	}
	if strings.Contains(got, "(wormzy-") {
		t.Fatalf("expected random fallback, got deterministic suffix: %s", got)
	}
	if !strings.HasSuffix(got, ".txt") {
		t.Fatalf("expected .txt suffix, got %s", got)
	}

	if _, err := os.Stat(got); err != nil {
		t.Fatalf("random fallback path was not reserved: %v", err)
	}
	t.Logf("random fallback chose %s", got)
}

// TestCreateDownloadFile_TestdataTmpDir exercises reservation in a repository test directory.
func TestCreateDownloadFile_TestdataTmpDir(t *testing.T) {
	dir := filepath.Join("testdata", "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	orig := filepath.Join(dir, "example.txt")
	collision := filepath.Join(dir, "example (wormzy-1).txt")
	t.Cleanup(func() {
		_ = os.Remove(orig)
		_ = os.Remove(collision)
	})

	if err := os.WriteFile(orig, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("seed original: %v", err)
	}

	file, got, renamed, err := createDownloadFile(dir, "example.txt")
	if err != nil {
		t.Fatalf("createDownloadFile returned error: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if !renamed {
		t.Fatalf("expected renamed=true because testdata/tmp already has example.txt")
	}
	want := collision
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
	if _, err := os.Stat(orig); err != nil {
		t.Fatalf("original file should remain: %v", err)
	}
	t.Logf("testdata conflict resolved to %s", got)
}
