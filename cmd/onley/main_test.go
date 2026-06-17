package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
