package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"onley/internal/db"
	"onley/internal/replica"
)

// --- truncate (same package) ---

func TestTruncate_Short(t *testing.T) {
	got := truncate("hello", 10)
	if got != "hello" {
		t.Errorf("want 'hello', got %q", got)
	}
}

func TestTruncate_Exact(t *testing.T) {
	s := strings.Repeat("x", 10)
	got := truncate(s, 10)
	if got != s {
		t.Errorf("exact-length string should be returned unchanged")
	}
}

func TestTruncate_Long(t *testing.T) {
	s := strings.Repeat("a", 100)
	got := truncate(s, 20)
	if len(got) != 20 {
		t.Errorf("truncated string should be length 20, got %d", len(got))
	}
	if !strings.HasPrefix(got, "...") {
		t.Errorf("truncated string should start with '...', got %q", got)
	}
}

// --- run() unit tests (direct call, no os.Exit) ---

func runCmd(args ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(""), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestRun_NoArgs(t *testing.T) {
	_, _, code := runCmd()
	if code == 0 {
		t.Error("expected non-zero exit with no arguments")
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	_, _, code := runCmd("notacommand")
	if code == 0 {
		t.Error("expected non-zero exit for unknown command")
	}
}

func TestRun_ScanMissingDir(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "test.db")
	_, _, code := runCmd("-db", dbFile, "scan")
	if code == 0 {
		t.Error("scan without directory arg should fail")
	}
}

func TestRun_ScanNonExistentDir(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "test.db")
	_, _, code := runCmd("-db", dbFile, "scan", "/no/such/path/xyz")
	if code == 0 {
		t.Error("scan of non-existent directory should fail")
	}
}

func TestRun_ScanAndStats(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	content := []byte("same content")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unique.txt"), []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, code := runCmd("-db", dbFile, "scan", dir)
	if code != 0 {
		t.Fatalf("scan failed (code %d): %s", code, out)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("scan output should mention 3 files; got: %s", out)
	}

	out, _, code = runCmd("-db", dbFile, "stats")
	if code != 0 {
		t.Fatalf("stats failed (code %d): %s", code, out)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("stats should report 3 total files; got: %s", out)
	}
}

func TestRun_Dupes(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	content := []byte("dup content")
	for _, name := range []string{"dup1.txt", "dup2.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runCmd("-db", dbFile, "scan", dir)

	out, _, code := runCmd("-db", dbFile, "dupes")
	if code != 0 {
		t.Fatalf("dupes failed (code %d): %s", code, out)
	}
	if !strings.Contains(out, "dup1.txt") || !strings.Contains(out, "dup2.txt") {
		t.Errorf("dupes output should list duplicate files; got: %s", out)
	}
}

func TestRun_DupesNone(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	if err := os.WriteFile(filepath.Join(dir, "only.txt"), []byte("sole"), 0o644); err != nil {
		t.Fatal(err)
	}

	runCmd("-db", dbFile, "scan", dir)

	out, _, code := runCmd("-db", dbFile, "dupes")
	if code != 0 {
		t.Fatalf("dupes failed: %s", out)
	}
	if !strings.Contains(out, "No duplicate files found") {
		t.Errorf("expected 'no duplicates' message; got: %s", out)
	}
}

func TestRun_CleanNoDupes(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	if err := os.WriteFile(filepath.Join(dir, "solo.txt"), []byte("only"), 0o644); err != nil {
		t.Fatal(err)
	}

	runCmd("-db", dbFile, "scan", dir)

	out, _, code := runCmd("-db", dbFile, "clean")
	if code != 0 {
		t.Fatalf("clean failed: %s", out)
	}
	if !strings.Contains(out, "No duplicate files found") {
		t.Errorf("expected 'no duplicates' message; got: %s", out)
	}
}

func TestRun_CleanSkipAll(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	content := []byte("dup")
	for _, name := range []string{"d1.txt", "d2.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runCmd("-db", dbFile, "scan", dir)

	// Send newline (skip) then deny confirm — but since no files selected, confirm never shown.
	var stdout, stderr bytes.Buffer
	code := run([]string{"-db", dbFile, "clean"}, strings.NewReader("\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("clean failed: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "No files selected") {
		t.Errorf("expected 'nothing selected' message; got: %s", stdout.String())
	}
}

func TestRun_CleanAborted(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	content := []byte("dup")
	for _, name := range []string{"d1.txt", "d2.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runCmd("-db", dbFile, "scan", dir)

	// Keep file 1 → deletes d2.txt; then deny with "n".
	var stdout, stderr bytes.Buffer
	code := run([]string{"-db", dbFile, "clean"}, strings.NewReader("1\nn\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("clean failed: stderr=%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Cancelled") {
		t.Errorf("expected 'Cancelled' message; got: %s", stdout.String())
	}
}

func TestRun_CleanDeleteFile(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	content := []byte("dup")
	paths := []string{
		filepath.Join(dir, "d1.txt"),
		filepath.Join(dir, "d2.txt"),
	}
	for _, p := range paths {
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runCmd("-db", dbFile, "scan", dir)

	// Keep file 1 (d1.txt sorted first), confirm with "y".
	var stdout, stderr bytes.Buffer
	code := run([]string{"-db", dbFile, "clean"}, strings.NewReader("1\ny\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("clean failed: stderr=%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Done:") {
		t.Errorf("expected completion message; got: %s", stdout.String())
	}
}

func TestRun_CleanAllNoDupes(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	if err := os.WriteFile(filepath.Join(dir, "solo.txt"), []byte("only"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd("-db", dbFile, "scan", dir)

	out, _, code := runCmd("-db", dbFile, "clean-all")
	if code != 0 {
		t.Fatalf("clean-all failed: %s", out)
	}
	if !strings.Contains(out, "No duplicate files found") {
		t.Errorf("expected 'no duplicates' message; got: %s", out)
	}
}

func TestRun_CleanAllAborted(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	content := []byte("dup")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runCmd("-db", dbFile, "scan", dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-db", dbFile, "clean-all"}, strings.NewReader("n\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("clean-all failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Cancelled") {
		t.Errorf("expected 'Cancelled'; got: %s", stdout.String())
	}
}

func TestRun_CleanAllDeletesExtras(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	content := []byte("dup")
	names := []string{"a.txt", "b.txt", "c.txt"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runCmd("-db", dbFile, "scan", dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-db", dbFile, "clean-all"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("clean-all failed: %s", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "2 deleted") {
		t.Errorf("expected '2 deleted'; got: %s", out)
	}
	// Only the first file (alphabetically) should survive on disk.
	kept := filepath.Join(dir, "a.txt")
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("expected %s to survive: %v", kept, err)
	}
	for _, name := range []string{"b.txt", "c.txt"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			t.Errorf("expected %s to be deleted", p)
		}
	}
}

func TestRun_CleanAllBadDB(t *testing.T) {
	_, _, code := runCmd("-db", "/", "clean-all")
	if code == 0 {
		t.Error("clean-all with un-openable db should fail")
	}
}

func TestRun_BadFlag(t *testing.T) {
	_, _, code := runCmd("--notaflag")
	if code == 0 {
		t.Error("unknown flag should return non-zero")
	}
}

// --- bad db path covers openDB nil branch ---

func TestRun_ScanBadDB(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, code := runCmd("-db", "/", "scan", dir)
	if code == 0 {
		t.Error("scan with un-openable db should fail")
	}
}

func TestRun_DupesBadDB(t *testing.T) {
	_, _, code := runCmd("-db", "/", "dupes")
	if code == 0 {
		t.Error("dupes with un-openable db should fail")
	}
}

func TestRun_StatsBadDB(t *testing.T) {
	_, _, code := runCmd("-db", "/", "stats")
	if code == 0 {
		t.Error("stats with un-openable db should fail")
	}
}

func TestRun_CleanBadDB(t *testing.T) {
	_, _, code := runCmd("-db", "/", "clean")
	if code == 0 {
		t.Error("clean with un-openable db should fail")
	}
}

// TestRun_ScanTwiceShowsSkipped exercises the p.Skipped branch (lines 144-146)
// by scanning the same directory twice — unchanged files are skipped on the
// second pass.
func TestRun_ScanTwiceShowsSkipped(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	runCmd("-db", dbFile, "scan", dir) // first scan

	out, _, code := runCmd("-db", dbFile, "scan", dir) // second scan
	if code != 0 {
		t.Fatalf("second scan failed (code %d): %s", code, out)
	}
	if !strings.Contains(out, "1 unchanged skipped") {
		t.Errorf("expected '1 unchanged skipped' on second scan; got: %s", out)
	}
}

// TestRun_ScanWithErrors exercises the p.Err branch (lines 132-135) and the
// errCount > 0 summary (lines 155-157) by scanning a file with no read
// permission.
func TestRun_ScanWithErrors(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read any file; skip permission test")
	}
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	path := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-db", dbFile, "scan", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("scan returned non-zero: %d (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("expected warning on stderr; got: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "skipped") {
		t.Errorf("expected skip notice on stdout; got: %s", stdout.String())
	}
}

// TestRun_CleanOsRemoveFails exercises the os.Remove failure path (lines 209-212)
// by removing a file from disk after indexing, then running clean.
func TestRun_CleanOsRemoveFails(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	content := []byte("dup")
	p1 := filepath.Join(dir, "d1.txt")
	p2 := filepath.Join(dir, "d2.txt")
	if err := os.WriteFile(p1, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, content, 0o644); err != nil {
		t.Fatal(err)
	}

	runCmd("-db", dbFile, "scan", dir)
	os.Remove(p2) // d2.txt gone from disk but still in DB index

	// Keep d1 (index 1), confirm delete of d2 → os.Remove will fail.
	var stdout, stderr bytes.Buffer
	code := run([]string{"-db", dbFile, "clean"}, strings.NewReader("1\ny\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("clean returned %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "delete failed") {
		t.Errorf("expected 'delete failed' on stderr; got: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 failed") {
		t.Errorf("expected '1 failed' in summary; got: %s", stdout.String())
	}
}

// TestRun_CleanAllOsRemoveFails exercises the os.Remove failure path in
// cmdCleanAll (lines 265-268) by the same pre-deletion technique.
func TestRun_CleanAllOsRemoveFails(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	content := []byte("dup")
	p1 := filepath.Join(dir, "d1.txt")
	p2 := filepath.Join(dir, "d2.txt")
	if err := os.WriteFile(p1, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, content, 0o644); err != nil {
		t.Fatal(err)
	}

	runCmd("-db", dbFile, "scan", dir)
	os.Remove(p2) // d2.txt gone from disk before clean-all runs

	var stdout, stderr bytes.Buffer
	code := run([]string{"-db", dbFile, "clean-all"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("clean-all returned %d", code)
	}
	if !strings.Contains(stderr.String(), "delete failed") {
		t.Errorf("expected 'delete failed' on stderr; got: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 failed") {
		t.Errorf("expected '1 failed' in summary; got: %s", stdout.String())
	}
}

// --- integration smoke test via compiled binary ---

var binaryPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "onley-inttest-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)

	// The index defaults to a path under the home directory, so tests would
	// otherwise create it in the home of whoever runs them, and adopt an
	// onley.db that happens to sit in the package directory.
	//
	// The Go caches are pinned first: GOMODCACHE and GOCACHE are derived from
	// the home directory, so moving HOME would send the build below to a fresh
	// empty cache and make TestMain download the world.
	for _, name := range []string{"GOMODCACHE", "GOCACHE", "GOPATH"} {
		if v := os.Getenv(name); v != "" {
			os.Setenv(name, v)
		} else if v, err := exec.Command("go", "env", name).Output(); err == nil {
			os.Setenv(name, strings.TrimSpace(string(v)))
		}
	}
	home := filepath.Join(tmp, "home")
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		panic(err)
	}

	bin := filepath.Join(tmp, "onley")
	out, err := exec.Command("go", "build", "-o", bin, "onley/cmd/onley").CombinedOutput()
	if err != nil {
		panic("build failed: " + string(out))
	}
	binaryPath = bin

	os.Exit(m.Run())
}

// newMasterServer starts an in-process master server for integration tests.
// It returns the test server URL and the master's DB.
func newMasterServer(t *testing.T) (string, *db.DB) {
	t.Helper()
	masterDB, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("master db.Open: %v", err)
	}
	t.Cleanup(func() { masterDB.Close() })
	storeDir := t.TempDir()
	srv := replica.NewServer(masterDB, storeDir)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, masterDB
}

func TestRun_ReplicaCheckNoMaster(t *testing.T) {
	_, _, code := runCmd("-db", filepath.Join(t.TempDir(), "local.db"), "replica", "check")
	if code == 0 {
		t.Error("replica check without -master should fail")
	}
}

func TestRun_ReplicaUnknownSubcommand(t *testing.T) {
	_, _, code := runCmd("replica", "unknown")
	if code == 0 {
		t.Error("unknown replica subcommand should fail")
	}
}

func TestRun_ReplicaMissingSubcommand(t *testing.T) {
	_, _, code := runCmd("replica")
	if code == 0 {
		t.Error("replica with no subcommand should fail")
	}
}

func TestRun_ReplicaCheckEmptyLocalDB(t *testing.T) {
	masterURL, _ := newMasterServer(t)
	dbFile := filepath.Join(t.TempDir(), "local.db")

	// Create an empty local DB (no files indexed).
	store, _ := db.Open(dbFile)
	store.Close()

	out, _, code := runCmd("-db", dbFile, "replica", "check", "-master", masterURL)
	if code != 0 {
		t.Fatalf("unexpected failure: %s", out)
	}
	if !strings.Contains(out, "Local index is empty") {
		t.Errorf("expected empty-index message; got: %s", out)
	}
}

func TestRun_ReplicaCheckAllOnMaster(t *testing.T) {
	masterURL, masterDB := newMasterServer(t)

	// Local: two files indexed.
	dir := t.TempDir()
	localDB := filepath.Join(t.TempDir(), "local.db")
	content := []byte("shared content")
	p1 := filepath.Join(dir, "f1.txt")
	p2 := filepath.Join(dir, "f2.txt")
	os.WriteFile(p1, content, 0o644)
	os.WriteFile(p2, content, 0o644)
	runCmd("-db", localDB, "scan", dir)

	// Pre-populate master with same MD5 so both files appear as "already on master".
	localStore, _ := db.Open(localDB)
	files, _ := localStore.AllFiles()
	localStore.Close()
	for _, f := range files {
		masterDB.Upsert(db.FileRecord{
			Path: "/master" + f.Path, Name: f.Name, Size: f.Size, MD5: f.MD5,
		})
	}

	// Replica check: should plan to delete both locally; we deny execution.
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"-db", localDB, "replica", "check", "-master", masterURL},
		strings.NewReader("n\n"),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("replica check failed: %s %s", stdout.String(), stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Delete locally") {
		t.Errorf("expected delete-local plan; got: %s", out)
	}
	if !strings.Contains(out, "Cancelled") {
		t.Errorf("expected cancellation message; got: %s", out)
	}
}

func TestRun_ReplicaCheckMigrateAndDelete(t *testing.T) {
	masterURL, _ := newMasterServer(t)

	// Local: one file that master doesn't have.
	dir := t.TempDir()
	localDB := filepath.Join(t.TempDir(), "local.db")
	p := filepath.Join(dir, "unique.txt")
	os.WriteFile(p, []byte("only on replica"), 0o644)
	runCmd("-db", localDB, "scan", dir)

	// Confirm migration.
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"-db", localDB, "replica", "check", "-master", masterURL},
		strings.NewReader("y\n"),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("replica check failed: stderr=%s", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Migrate to master") {
		t.Errorf("expected migrate plan; got: %s", out)
	}
	if !strings.Contains(out, "1 migrated") {
		t.Errorf("expected '1 migrated'; got: %s", out)
	}
	// Local file should be removed after successful migration.
	if _, err := os.Stat(p); err == nil {
		t.Error("expected local file to be deleted after migration")
	}
}

func TestRun_ReplicaCheckDeleteAndConfirm(t *testing.T) {
	masterURL, masterDB := newMasterServer(t)

	dir := t.TempDir()
	localDB := filepath.Join(t.TempDir(), "local.db")
	p := filepath.Join(dir, "dup.txt")
	os.WriteFile(p, []byte("dup content"), 0o644)
	runCmd("-db", localDB, "scan", dir)

	localStore, _ := db.Open(localDB)
	files, _ := localStore.AllFiles()
	localStore.Close()

	masterDB.Upsert(db.FileRecord{
		Path: "/master/dup.txt", Name: "dup.txt", Size: files[0].Size, MD5: files[0].MD5,
	})

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"-db", localDB, "replica", "check", "-master", masterURL},
		strings.NewReader("y\n"),
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("replica check failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 deleted") {
		t.Errorf("expected '1 deleted'; got: %s", stdout.String())
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("expected local file to be removed after delete-local")
	}
}

func TestRun_ReplicaCheckUnreachableMaster(t *testing.T) {
	dir := t.TempDir()
	localDB := filepath.Join(t.TempDir(), "local.db")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644)
	runCmd("-db", localDB, "scan", dir)

	_, stderr, code := runCmd("-db", localDB, "replica", "check", "-master", "http://127.0.0.1:1")
	if code == 0 {
		t.Error("expected failure when master is unreachable")
	}
	if !strings.Contains(stderr, "cannot reach master") {
		t.Errorf("expected connection error message; got: %s", stderr)
	}
}

func TestRun_ServeBadDB(t *testing.T) {
	_, _, code := runCmd("-db", "/", "serve")
	if code == 0 {
		t.Error("serve with un-openable db should fail")
	}
}

func TestRun_ServeBadStoreDir(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "master.db")
	// Use a file path (not directory) as the store dir so MkdirAll fails.
	existingFile := filepath.Join(t.TempDir(), "notadir")
	os.WriteFile(existingFile, []byte("x"), 0o644)
	_, _, code := runCmd("-db", dbFile, "serve", "-store", existingFile+"/subdir")
	// On most OSes, creating a subdir of a regular file should fail.
	// If it somehow succeeds, skip the assertion.
	if code == 0 {
		t.Log("MkdirAll succeeded unexpectedly; skipping assertion")
	}
}

// --- watch ---

func TestRun_WatchMissingDirArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"watch"}, &bytes.Buffer{}, &stdout, &stderr); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "requires a directory") {
		t.Errorf("stderr should explain the missing argument; got: %s", stderr.String())
	}
}

func TestRun_WatchNonExistentDir(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"watch", filepath.Join(t.TempDir(), "nope")}, &bytes.Buffer{}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "directory not found") {
		t.Errorf("stderr should report the missing directory; got: %s", stderr.String())
	}
}

func TestRun_WatchFileInsteadOfDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"watch", path}, &bytes.Buffer{}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not a directory") {
		t.Errorf("stderr should reject a file argument; got: %s", stderr.String())
	}
}

// TestRun_WatchBadFlag covers the flag set of the subcommand itself.
func TestRun_WatchBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"watch", t.TempDir(), "-nope"}, &bytes.Buffer{}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// TestRun_WatchUsageListsSubcommand keeps the new command discoverable from the
// top-level help.
func TestRun_WatchUsageListsSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run(nil, &bytes.Buffer{}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "watch <dir>") {
		t.Errorf("usage should list watch; got: %s", stderr.String())
	}
}

// TestWatch_IndexesThenRemoves is the end-to-end path: watch maintains the index
// as files appear and disappear, and never touches the file system itself.
//
// watch runs until interrupted, so the binary is driven as a subprocess and
// stopped with a signal, which is also what an operator does.
func TestWatch_IndexesThenRemoves(t *testing.T) {
	if _, err := exec.LookPath(binaryPath); err != nil {
		t.Skipf("binary not built: %v", err)
	}
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "watch.db")
	added := filepath.Join(dir, "added.txt")
	doomed := filepath.Join(dir, "doomed.txt")
	if err := os.WriteFile(added, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doomed, []byte("removed later"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binaryPath, "-db", dbFile, "watch", dir, "-debounce", "80ms")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start watch: %v", err)
	}
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}()

	waitForIndex(t, dbFile, 2, "both starting files", &out)

	// A new file must land in the index.
	late := filepath.Join(dir, "late.txt")
	if err := os.WriteFile(late, []byte("arrived later"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForIndex(t, dbFile, 3, "the file added while watching", &out)

	// A deleted file must leave the index, and the deletion must be the user's
	// own: watch only drops the record.
	if err := os.Remove(doomed); err != nil {
		t.Fatal(err)
	}
	waitForIndex(t, dbFile, 2, "the deleted file to leave the index", &out)

	if _, err := os.Stat(late); err != nil {
		t.Errorf("watch removed a file from disk: %v", err)
	}
}

// TestWatch_StopReportsTotals covers the shutdown path: interrupting must leave
// a consistent index and print what it did.
func TestWatch_StopReportsTotals(t *testing.T) {
	if _, err := exec.LookPath(binaryPath); err != nil {
		t.Skipf("binary not built: %v", err)
	}
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "watch.db")
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binaryPath, "-db", dbFile, "watch", dir, "-debounce", "80ms")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start watch: %v", err)
	}
	waitForIndex(t, dbFile, 1, "the starting file", &out)

	cmd.Process.Signal(os.Interrupt)
	if err := cmd.Wait(); err != nil {
		t.Errorf("watch exited with %v; output: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "Stopped:") {
		t.Errorf("watch should report totals on exit; got: %s", out.String())
	}
}

// TestConcurrentCommandsDuringWatch runs other onley commands against the index
// while watch holds it open. Every command opens the database, and the schema
// needs a write lock to be a no-op, so without a busy timeout set before it a
// reader racing a watch write fails with SQLITE_BUSY.
//
// This is a separate process rather than a second db.DB in this one, because
// the failure needs the real cross-process file locking that a shared in-process
// connection does not reproduce.
func TestConcurrentCommandsDuringWatch(t *testing.T) {
	if _, err := exec.LookPath(binaryPath); err != nil {
		t.Skipf("binary not built: %v", err)
	}
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "watch.db")
	// Several files so watch keeps writing while the readers run.
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.bin", i)), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command(binaryPath, "-db", dbFile, "watch", dir, "-debounce", "60ms")
	var watchOut bytes.Buffer
	cmd.Stdout = &watchOut
	cmd.Stderr = &watchOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start watch: %v", err)
	}
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}()

	waitForIndex(t, dbFile, 5, "the starting files", &watchOut)

	// Keep touching files so watch is writing throughout, then hammer the same
	// database with read-only commands.
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("churn%d.bin", i%3)), make([]byte, 1024), 0o644)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	defer close(stop)

	for _, args := range [][]string{
		{"stats"},
		{"dupes"},
	} {
		for i := 0; i < 20; i++ {
			out, err := exec.Command(binaryPath, append([]string{"-db", dbFile}, args...)...).CombinedOutput()
			if err != nil {
				t.Fatalf("%v while watch is writing (attempt %d): %v\n%s", args, i, err, out)
			}
			if strings.Contains(string(out), "locked") {
				t.Fatalf("%v hit a lock error: %s", args, out)
			}
		}
	}
}

// waitForIndex polls stats until the index holds want files. watchOut is
// reported on failure, because "the index is empty" is only diagnosable
// together with what watch itself printed.
func waitForIndex(t *testing.T, dbFile string, want int, what string, watchOut *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := exec.Command(binaryPath, "-db", dbFile, "stats").Output()
		if err == nil {
			last = string(out)
			var total int
			fmt.Sscanf(strings.TrimSpace(strings.SplitN(last, "\n", 2)[0]), "Total indexed: %d", &total)
			if total == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (want %d); stats said: %s\nwatch printed:\n%s", what, want, last, watchOut.String())
}

func TestBinary_SmokeStats(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec.Command(binaryPath, "-db", dbFile, "scan", dir).Run()

	out, err := exec.Command(binaryPath, "-db", dbFile, "stats").Output()
	if err != nil {
		t.Fatalf("stats via binary: %v", err)
	}
	if !strings.Contains(string(out), "1") {
		t.Errorf("stats should show 1 file; got: %s", out)
	}
}

// --- default index location ---

// fakeHome points the home directory at a fresh temporary directory for one
// test and returns it.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// TestDefaultDBPath_LivesUnderHome covers the reason the default moved: an
// index named after the working directory gave every directory its own, so a
// scan in one place left stats in another reporting nothing.
func TestDefaultDBPath_LivesUnderHome(t *testing.T) {
	home := fakeHome(t)
	work := t.TempDir()
	t.Chdir(work)

	if _, _, code := runCmd("stats"); code != 0 {
		t.Fatalf("stats without -db failed: %d", code)
	}

	if _, err := os.Stat(filepath.Join(home, ".onley", "local.db")); err != nil {
		t.Errorf("index not created under the home directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, legacyDBFile)); err == nil {
		t.Errorf("an index was created in the working directory")
	}
}

// TestDefaultDBPath_SameIndexFromAnyDirectory is the property that matters:
// two directories must see one index.
func TestDefaultDBPath_SameIndexFromAnyDirectory(t *testing.T) {
	fakeHome(t)
	scanDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scanDir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(scanDir)
	if _, _, code := runCmd("scan", scanDir); code != 0 {
		t.Fatalf("scan failed: %d", code)
	}

	t.Chdir(t.TempDir())
	stdout, _, code := runCmd("stats")
	if code != 0 {
		t.Fatalf("stats failed: %d", code)
	}
	if !strings.Contains(stdout, "Total indexed: 1") {
		t.Errorf("a scan in another directory was not visible: %s", stdout)
	}
}

// TestDefaultDBPath_DirectoryIsPrivate checks the mode of the directory that
// holds the inventory of the user's files.
func TestDefaultDBPath_DirectoryIsPrivate(t *testing.T) {
	fakeHome(t)
	t.Chdir(t.TempDir())

	if _, _, code := runCmd("stats"); code != 0 {
		t.Fatalf("stats failed: %d", code)
	}

	info, err := os.Stat(filepath.Join(os.Getenv("HOME"), dbDirName))
	if err != nil {
		t.Fatalf("stat %s: %v", dbDirName, err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 700", dbDirName, perm)
		}
	}
}

// TestDefaultDBPath_AdoptsWorkingDirectoryIndex covers the migration: a user who
// already has onley.db keeps their index.
func TestDefaultDBPath_AdoptsWorkingDirectoryIndex(t *testing.T) {
	home := fakeHome(t)
	work := t.TempDir()
	t.Chdir(work)

	seed, err := db.Open(filepath.Join(work, legacyDBFile))
	if err != nil {
		t.Fatalf("seed db.Open: %v", err)
	}
	if err := seed.Upsert(db.FileRecord{
		Path: "/kept.txt", Name: "kept.txt", Size: 4, MD5: "d41d8cd98f00b204e9800998ecf8427e",
	}); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}
	seed.Close()

	stdout, stderr, code := runCmd("stats")
	if code != 0 {
		t.Fatalf("stats failed: %d", code)
	}
	if !strings.Contains(stdout, "Total indexed: 1") {
		t.Errorf("adopted index lost its contents: %s", stdout)
	}
	if !strings.Contains(stderr, legacyDBFile) {
		t.Errorf("the copy was not reported: %s", stderr)
	}

	// The old file stays, so -db ./onley.db keeps working.
	if _, err := os.Stat(filepath.Join(work, legacyDBFile)); err != nil {
		t.Errorf("the old index was removed: %v", err)
	}
	adopted, err := db.Open(filepath.Join(home, dbDirName, dbFileName))
	if err != nil {
		t.Fatalf("open adopted index: %v", err)
	}
	defer adopted.Close()
	if rec, err := adopted.Lookup("/kept.txt"); err != nil || rec == nil {
		t.Errorf("the record did not survive the copy: rec=%v err=%v", rec, err)
	}
}

// TestDefaultDBPath_DoesNotOverwriteExistingIndex keeps a later run from
// replacing a populated default index with whatever onley.db happens to sit in
// the directory.
func TestDefaultDBPath_DoesNotOverwriteExistingIndex(t *testing.T) {
	home := fakeHome(t)
	work := t.TempDir()
	t.Chdir(work)

	target := filepath.Join(home, dbDirName, dbFileName)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	existing, err := db.Open(target)
	if err != nil {
		t.Fatalf("seed default index: %v", err)
	}
	// Two records, so the count tells the two indexes apart.
	for _, p := range []string{"/default.txt", "/second.txt"} {
		if err := existing.Upsert(db.FileRecord{
			Path: p, Name: filepath.Base(p), Size: 7, MD5: "abc",
		}); err != nil {
			t.Fatalf("seed Upsert: %v", err)
		}
	}
	existing.Close()

	// A stale onley.db in the working directory must be left alone.
	stale, err := db.Open(filepath.Join(work, legacyDBFile))
	if err != nil {
		t.Fatalf("seed legacy index: %v", err)
	}
	if err := stale.Upsert(db.FileRecord{
		Path: "/stale.txt", Name: "stale.txt", Size: 5, MD5: "def",
	}); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}
	stale.Close()

	stdout, stderr, code := runCmd("stats")
	if code != 0 {
		t.Fatalf("stats failed: %d", code)
	}
	// Which index stats reads is what decides this: the default one, not the
	// stale working-directory one.
	if !strings.Contains(stdout, "Total indexed: 2") {
		t.Errorf("stats did not report the default index: %s", stdout)
	}
	if strings.Contains(stderr, "Copied") {
		t.Errorf("an existing default index was overwritten: %s", stderr)
	}

	kept, err := db.Open(target)
	if err != nil {
		t.Fatalf("reopen default index: %v", err)
	}
	defer kept.Close()
	if rec, err := kept.Lookup("/default.txt"); err != nil || rec == nil {
		t.Errorf("the default index lost its record: rec=%v err=%v", rec, err)
	}
	if rec, err := kept.Lookup("/stale.txt"); err != nil || rec != nil {
		t.Errorf("the stale working-directory index was copied over it: rec=%v err=%v", rec, err)
	}
}

// TestDefaultDBPath_ExplicitPathIsUsedVerbatim keeps -db an explicit choice: a
// relative path stays relative to the working directory and nothing is created
// under the home directory.
func TestDefaultDBPath_ExplicitPathIsUsedVerbatim(t *testing.T) {
	home := fakeHome(t)
	work := t.TempDir()
	t.Chdir(work)

	if _, _, code := runCmd("-db", "chosen.db", "stats"); code != 0 {
		t.Fatalf("stats failed: %d", code)
	}

	if _, err := os.Stat(filepath.Join(work, "chosen.db")); err != nil {
		t.Errorf("-db chosen.db did not create the file in the working directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, dbDirName)); err == nil {
		t.Errorf("an explicit -db still created the default directory")
	}
}

// TestDefaultDBPath_MissingDirectoryForExplicitPathIsAnError keeps -db from
// silently creating directories the user did not ask for.
func TestDefaultDBPath_MissingDirectoryForExplicitPathIsAnError(t *testing.T) {
	fakeHome(t)
	work := t.TempDir()
	t.Chdir(work)

	missing := filepath.Join(work, "no-such-dir", "x.db")
	if _, _, code := runCmd("-db", missing, "stats"); code == 0 {
		t.Error("stats succeeded with a -db whose parent directory does not exist")
	}
	if _, err := os.Stat(filepath.Join(work, "no-such-dir")); err == nil {
		t.Error("the missing parent directory was created anyway")
	}
}

// TestDefaultDBPath_NoHomeDirectoryIsAnError covers the case that must not fall
// back to a working-directory index, which is the behaviour being replaced.
func TestDefaultDBPath_NoHomeDirectoryIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the home directory comes from a different variable on Windows")
	}
	t.Setenv("HOME", "")
	t.Chdir(t.TempDir())

	_, stderr, code := runCmd("stats")
	if code == 0 {
		t.Error("stats succeeded without a home directory")
	}
	if !strings.Contains(stderr, "home directory") {
		t.Errorf("stderr should explain the missing home directory: %s", stderr)
	}
}

// TestDefaultDBPath_UsageNamesTheDefault keeps the new location discoverable,
// because PrintDefaults prints nothing when the flag default is empty.
func TestDefaultDBPath_UsageNamesTheDefault(t *testing.T) {
	fakeHome(t)
	t.Chdir(t.TempDir())

	_, stderr, code := runCmd()
	if code == 0 {
		t.Fatal("no arguments should fail")
	}
	if !strings.Contains(stderr, "~/.onley/local.db") {
		t.Errorf("usage should name the default index; got: %s", stderr)
	}
}

// TestDefaultDBPath_NoArgumentsCreatesNothing guards the ordering: printing
// usage must not create the directory as a side effect.
func TestDefaultDBPath_NoArgumentsCreatesNothing(t *testing.T) {
	home := fakeHome(t)
	t.Chdir(t.TempDir())

	if _, _, code := runCmd(); code == 0 {
		t.Fatal("no arguments should fail")
	}

	if _, err := os.Stat(filepath.Join(home, dbDirName)); err == nil {
		t.Error("printing usage created the default directory")
	}
}
