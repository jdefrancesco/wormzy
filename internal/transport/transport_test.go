package transport

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"strings"
)

// When no collision exists, keep the original filename.
func TestPickDownloadPath_NoConflict(t *testing.T) {
	dir := t.TempDir()
	got, renamed, err := pickDownloadPath(dir, "example.txt")
	if err != nil {
		t.Fatalf("pickDownloadPath returned error: %v", err)
	}
	want := filepath.Join(dir, "example.txt")
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
	if renamed {
		t.Fatalf("expected renamed=false")
	}
}

// When a collision exists, choose the next numbered variant.
func TestPickDownloadPath_WithConflicts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("failed to seed file: %v", err)
	}
	got, renamed, err := pickDownloadPath(dir, "example.txt")
	if err != nil {
		t.Fatalf("pickDownloadPath returned error: %v", err)
	}
	if !renamed {
		t.Fatalf("expected renamed=true when collision occurs")
	}
	want := filepath.Join(dir, "example (wormzy-1).txt")
	if got != want {
		t.Fatalf("unexpected path. want %s got %s", want, got)
	}

	// Seed the next candidate to ensure we advance counters.
	if err := os.WriteFile(want, []byte("y"), 0o600); err != nil {
		t.Fatalf("failed to seed candidate file: %v", err)
	}
	got, renamed, err = pickDownloadPath(dir, "example.txt")
	if err != nil {
		t.Fatalf("pickDownloadPath returned error: %v", err)
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

// When deterministic suffixes are exhausted, fall back to a random tag.
func TestPickDownloadPath_RandomFallback(t *testing.T) {
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

	got, renamed, err := pickDownloadPath(dir, "example.txt")
	if err != nil {
		t.Fatalf("pickDownloadPath returned error: %v", err)
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

	// ensure the candidate does not already exist
	if _, err := os.Stat(got); err == nil {
		t.Fatalf("random fallback path unexpectedly exists: %s", got)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat on fallback path failed: %v", err)
	}
	t.Logf("random fallback chose %s", got)
}

// Exercise the resolver against a real on-disk directory we can inspect.
func TestPickDownloadPath_TestdataTmpDir(t *testing.T) {
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

	got, renamed, err := pickDownloadPath(dir, "example.txt")
	if err != nil {
		t.Fatalf("pickDownloadPath returned error: %v", err)
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
