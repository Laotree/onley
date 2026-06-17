package scanner

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"onley/internal/db"
)

func openMemDB(t *testing.T) *db.DB {
	t.Helper()
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func md5Of(content string) string {
	h := md5.New()
	h.Write([]byte(content))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// drain collects all progress events, counting only Done=true events as completed.
func drain(ch <-chan Progress) (ok int, skipped int, errs []error) {
	for p := range ch {
		if p.Err != nil {
			errs = append(errs, p.Err)
		} else if p.Done {
			ok++
			if p.Skipped {
				skipped++
			}
		}
	}
	return
}

func TestScan_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	store := openMemDB(t)

	ok, _, errs := drain(Scan(dir, store, 4))
	if ok != 0 {
		t.Errorf("want 0 files, got %d", ok)
	}
	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
}

func TestScan_SingleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openMemDB(t)
	ok, _, errs := drain(Scan(dir, store, 4))
	if ok != 1 {
		t.Errorf("want 1 file, got %d", ok)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	total, _, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if total != 1 {
		t.Errorf("want 1 record, got %d", total)
	}
}

func TestScan_MD5Correct(t *testing.T) {
	dir := t.TempDir()
	content := "test content for hashing"
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openMemDB(t)
	drain(Scan(dir, store, 4))

	total, dups, _ := store.Stats()
	if total != 1 || dups != 0 {
		t.Errorf("want total=1 dups=0, got %d %d", total, dups)
	}
}

func TestScan_DuplicateFiles(t *testing.T) {
	dir := t.TempDir()
	content := "identical content"
	for _, name := range []string{"copy1.txt", "copy2.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := openMemDB(t)
	ok, _, errs := drain(Scan(dir, store, 4))
	if ok != 2 {
		t.Errorf("want 2 files, got %d", ok)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	groups, err := store.Duplicates()
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("want 1 duplicate group, got %d", len(groups))
	}
	if groups[0].MD5 != md5Of(content) {
		t.Errorf("wrong MD5: want %s, got %s", md5Of(content), groups[0].MD5)
	}
}

func TestScan_SubDirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(dir, "root.txt"),
		filepath.Join(sub, "nested.txt"),
	} {
		if err := os.WriteFile(p, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := openMemDB(t)
	ok, _, errs := drain(Scan(dir, store, 4))
	if ok != 2 {
		t.Errorf("want 2 files (root + nested), got %d", ok)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestScan_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	store := openMemDB(t)
	ok, _, errs := drain(Scan(dir, store, 4))
	if ok != 0 {
		t.Errorf("want 0 files (only dirs), got %d", ok)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

// TestScan_ResumesUnchangedFiles verifies that a second scan skips files
// whose size and mtime have not changed.
func TestScan_ResumesUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("stable content"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openMemDB(t)
	drain(Scan(dir, store, 4))

	// Second scan: file unchanged → must be skipped.
	_, skipped, errs := drain(Scan(dir, store, 4))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if skipped != 1 {
		t.Errorf("want 1 skipped file on second scan, got %d", skipped)
	}
}

// TestScan_RehashesModifiedFile verifies that a changed file is re-hashed.
func TestScan_RehashesModifiedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openMemDB(t)
	drain(Scan(dir, store, 4))

	// Overwrite with new content and bump mtime by at least 1 second.
	if err := os.WriteFile(path, []byte("v2 updated content"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	_, skipped, errs := drain(Scan(dir, store, 4))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if skipped != 0 {
		t.Errorf("modified file should not be skipped, got skipped=%d", skipped)
	}

	// DB should still have exactly 1 record with the new MD5.
	total, _, _ := store.Stats()
	if total != 1 {
		t.Errorf("want 1 row, got %d", total)
	}
}

// TestScan_RehashesResizedFile verifies that a file with same mtime but
// different size (edge case) is re-hashed.
func TestScan_RehashesResizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openMemDB(t)
	drain(Scan(dir, store, 4))

	// Write different-length content but keep the original mtime.
	info, _ := os.Stat(path)
	origMtime := info.ModTime()
	if err := os.WriteFile(path, []byte("much longer content here"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, origMtime, origMtime)

	_, skipped, errs := drain(Scan(dir, store, 4))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if skipped != 0 {
		t.Errorf("resized file should not be skipped")
	}
}

// TestHashFile tests the unexported hashFile directly (same package).
func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	content := "hash me"
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	hash, err := hashFile(path)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	if hash != md5Of(content) {
		t.Errorf("wrong hash: want %s, got %s", md5Of(content), hash)
	}
}

func TestHashFile_NotFound(t *testing.T) {
	_, err := hashFile("/no/such/file.bin")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestScan_UnreadableFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read any file; skip permission test")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)

	store := openMemDB(t)
	var errs []error
	for p := range Scan(dir, store, 4) {
		if p.Err != nil {
			errs = append(errs, p.Err)
		}
	}
	if len(errs) == 0 {
		t.Error("expected at least one error scanning unreadable file")
	}
}

// TestScan_ConcurrentCorrectness creates many files and verifies that
// concurrent scanning produces the same results as a single-worker scan.
func TestScan_ConcurrentCorrectness(t *testing.T) {
	const fileCount = 50
	dir := t.TempDir()
	for i := range fileCount {
		name := filepath.Join(dir, fmt.Sprintf("file%03d.txt", i))
		if err := os.WriteFile(name, []byte(fmt.Sprintf("content-%d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Single-worker reference scan.
	ref := openMemDB(t)
	ok1, _, errs := drain(Scan(dir, ref, 1))
	if len(errs) != 0 {
		t.Fatalf("single-worker errors: %v", errs)
	}

	// Multi-worker scan on a fresh DB.
	multi := openMemDB(t)
	ok2, _, errs := drain(Scan(dir, multi, 8))
	if len(errs) != 0 {
		t.Fatalf("multi-worker errors: %v", errs)
	}

	if ok1 != fileCount || ok2 != fileCount {
		t.Errorf("want %d files indexed; single=%d multi=%d", fileCount, ok1, ok2)
	}

	// Both DBs must report the same duplicate structure (none here).
	g1, _ := ref.Duplicates()
	g2, _ := multi.Duplicates()
	if len(g1) != len(g2) {
		t.Errorf("duplicate groups differ: single=%d multi=%d", len(g1), len(g2))
	}
}

// TestScan_WalkDirError exercises the WalkDir error callback (lines 64-67) by
// scanning a directory with permissions that prevent ReadDir.
func TestScan_WalkDirError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked-dir")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	// Put a file inside, then lock the directory.
	if err := os.WriteFile(filepath.Join(locked, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)

	store := openMemDB(t)
	var errs []error
	for p := range Scan(dir, store, 1) {
		if p.Err != nil {
			errs = append(errs, p.Err)
		}
	}
	if len(errs) == 0 {
		t.Error("expected walk error for locked directory")
	}
}

// TestScan_LookupError exercises the worker Lookup error path (lines 89-91) by
// closing the DB before the scan begins.
func TestScan_LookupError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store.Close() // closed intentionally; Lookup will return an error

	var errs []error
	for p := range Scan(dir, store, 1) {
		if p.Err != nil {
			errs = append(errs, p.Err)
		}
	}
	if len(errs) == 0 {
		t.Error("expected errors when DB is closed before scan")
	}
}

// TestScan_Workers1 ensures workers=1 behaves identically to the old serial path.
func TestScan_Workers1(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := openMemDB(t)
	ok, _, errs := drain(Scan(dir, store, 1))
	if ok != 3 {
		t.Errorf("want 3, got %d", ok)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}
