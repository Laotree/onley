package replica

import (
	"bytes"
	"crypto/md5"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func newTestServer(t *testing.T) (*Server, *httptest.Server, *db.DB) {
	srv, ts, store, _ := newTestServerWithStore(t)
	return srv, ts, store
}

// newTestServerWithStore also returns the store directory, for the tests that
// need to assert on what was left on disk.
func newTestServerWithStore(t *testing.T) (*Server, *httptest.Server, *db.DB, string) {
	t.Helper()
	store := openMemDB(t)
	storeDir := t.TempDir()
	srv := NewServer(store, storeDir)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, store, storeDir
}

// --- Server unit tests ---

func TestServer_Health(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}
}

func TestServer_CheckNotFound(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/check?md5=doesnotexist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body checkResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Found {
		t.Error("expected found=false for unknown md5")
	}
}

func TestServer_CheckFound(t *testing.T) {
	_, ts, store := newTestServer(t)
	if err := store.Upsert(db.FileRecord{
		Path: "/master/file.txt", Name: "file.txt", Size: 10, MD5: "abc123",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/v1/check?md5=abc123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var body checkResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Found {
		t.Error("expected found=true")
	}
	if len(body.Paths) == 0 {
		t.Error("expected paths to be non-empty")
	}
}

func TestServer_CheckMissingMD5(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/check")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestServer_IngestStoresFile(t *testing.T) {
	_, ts, store := newTestServer(t)
	client := NewClient(ts.URL)

	dir := t.TempDir()
	localPath := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(localPath, []byte("hello replica"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The digest of what was actually written, not a plausible-looking constant.
	// The master verifies this now, and it did not before, which is why a
	// fabricated value used to pass unnoticed.
	md5sum := md5OfFile(t, localPath)

	// Not on master yet.
	found, err := client.Check(md5sum)
	if err != nil || found {
		t.Fatalf("pre-ingest: found=%v err=%v", found, err)
	}

	if err := client.Ingest(localPath, md5sum); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Now the master DB must have it.
	found, err = client.Check(md5sum)
	if err != nil {
		t.Fatalf("post-ingest check: %v", err)
	}
	if !found {
		t.Error("expected found=true after ingest")
	}

	// Verify it is actually indexed in the master DB.
	records, err := store.FindByMD5(md5sum)
	if err != nil || len(records) == 0 {
		t.Errorf("expected record in master DB; got %d records, err=%v", len(records), err)
	}
}

// md5OfFile returns the digest of path's contents. Tests that upload something
// have to declare the digest the master will verify against, and computing it is
// the only way to declare the right one.
func md5OfFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fmt.Sprintf("%x", md5.Sum(data))
}

// --- Client unit tests ---

func TestClient_PingOK(t *testing.T) {
	_, ts, _ := newTestServer(t)
	if err := NewClient(ts.URL).Ping(); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestClient_PingUnreachable(t *testing.T) {
	if err := NewClient("http://127.0.0.1:1").Ping(); err == nil {
		t.Error("expected error pinging unreachable server")
	}
}

func TestClient_IngestMissingFile(t *testing.T) {
	_, ts, _ := newTestServer(t)
	err := NewClient(ts.URL).Ingest("/no/such/file.txt", "deadbeef01234567890abcdef1234567")
	if err == nil {
		t.Error("expected error uploading non-existent file")
	}
}

func TestClient_IngestBadMD5(t *testing.T) {
	_, ts, _ := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	os.WriteFile(path, []byte("x"), 0o644)

	// MD5 too short — server returns 400.
	err := NewClient(ts.URL).Ingest(path, "ab")
	if err == nil {
		t.Error("expected error for invalid md5")
	}
}

// --- streaming uploads ---

// TestIngestRequest_BodyIsStreamedNotBuffered is the property the change exists
// for. A buffered body has to know its length up front, so a length of -1 proves
// the bytes are produced as they are sent rather than held in memory first.
func TestIngestRequest_BodyIsStreamedNotBuffered(t *testing.T) {
	_, srv, _ := newTestServer(t)
	c := NewClient(srv.URL)

	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	content := bytes.Repeat([]byte("payload"), 1000)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	req, errc, err := c.ingestRequest(path, "deadbeef")
	if err != nil {
		t.Fatalf("ingestRequest: %v", err)
	}
	defer req.Body.Close()

	// A buffered body would have had its length measured up front: NewRequest
	// computes ContentLength for the reader types it knows, and a bytes.Buffer is
	// one of them. A pipe is not, so nothing is known in advance and the transport
	// falls back to chunked encoding.
	if req.ContentLength > 0 {
		t.Errorf("ContentLength = %d, want an unmeasured length for a streamed body", req.ContentLength)
	}
	if ct := req.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
		t.Errorf("Content-Type = %q", ct)
	}

	// The body still has to be a well-formed multipart form, boundary and all.
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("body goroutine: %v", err)
	}
	if !bytes.Contains(body, content) {
		t.Error("the streamed body does not carry the file content")
	}
	if !bytes.Contains(body, []byte(`name="md5"`)) || !bytes.Contains(body, []byte("deadbeef")) {
		t.Error("the md5 field is missing from the streamed body")
	}
}

// TestIngestRequest_BodyErrorReachesTheCaller covers the failure mode that a
// naive pipe implementation gets wrong: closing the pipe without an error
// presents a short body as a complete one, and the upload lands as a truncated
// file with nothing indicating it.
func TestIngestRequest_BodyErrorReachesTheCaller(t *testing.T) {
	_, srv, _ := newTestServer(t)
	c := NewClient(srv.URL)

	// Large enough that the writer cannot finish before the reader goes away.
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(path, make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	req, errc, err := c.ingestRequest(path, "deadbeef")
	if err != nil {
		t.Fatalf("ingestRequest: %v", err)
	}
	// Abandon the request the way a cancelled transfer would.
	if err := req.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	select {
	case err := <-errc:
		if err == nil {
			t.Error("the body goroutine reported success after the reader went away")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the body goroutine did not finish; a cancelled upload would hang")
	}
}

// TestIngest_LargeFileLandsIntact goes past maxIngestMemory, so the master has to
// spill the part to a temporary file. That path is the one a streaming client
// could plausibly break, since the file is no longer in memory on either side.
func TestIngest_LargeFileLandsIntact(t *testing.T) {
	_, srv, masterDB := newTestServer(t)
	c := NewClient(srv.URL)

	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	content := make([]byte, maxIngestMemory+(5<<20))
	// crypto/rand, not math/rand: this wants bytes that a digest collision
	// cannot fake, and the linter is right about the deprecation.
	if _, err := cryptorand.Read(content); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", md5.Sum(content))

	if err := c.Ingest(path, sum); err != nil {
		t.Fatalf("Ingest of %d bytes: %v", len(content), err)
	}

	files, err := masterDB.FindByMD5(sum)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("master holds %d records for the digest, want 1", len(files))
	}
	if files[0].Size != int64(len(content)) {
		t.Errorf("master recorded size %d, want %d", files[0].Size, len(content))
	}
	got, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("the stored file differs from what was sent (%d vs %d bytes)", len(got), len(content))
	}
}

// TestNewClient_TimeoutBoundsResponseHeadersNotTransfer pins the semantics of the
// timeout. A blanket Client.Timeout also bounds how long a body may take to
// send, which depends on the file size and the link rather than on whether the
// master is healthy.
func TestNewClient_TimeoutBoundsResponseHeadersNotTransfer(t *testing.T) {
	c := NewClient("http://example.invalid:8080")
	if c.hc.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 so the upload is not time-boxed", c.hc.Timeout)
	}
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.hc.Transport)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	// The dial and handshake timeouts have to survive the clone, or a dead host
	// would hang instead of failing.
	if tr.TLSHandshakeTimeout == 0 || tr.IdleConnTimeout == 0 {
		t.Errorf("cloning lost the default timeouts: %+v", tr)
	}
}

// TestIngest_ReportsTheUploadError checks that a failure while producing the body
// reaches the caller as an error, rather than arriving at the master as a short
// file that nobody can tell apart from a complete one.
func TestIngest_ReportsTheUploadError(t *testing.T) {
	srv, closeServer := newFailingIngestServer(t)
	defer closeServer()

	c := NewClient(srv.URL)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The server hangs up without reading the body, which is what a master that
	// dies mid-upload looks like from here.
	err := c.Ingest(path, "deadbeef")
	if err == nil {
		t.Fatal("Ingest reported success although the upload was cut short")
	}
	if !strings.Contains(err.Error(), path) && !strings.Contains(err.Error(), "EOF") &&
		!strings.Contains(err.Error(), "connection") {
		t.Logf("error was: %v", err)
	}
}

// newFailingIngestServer accepts the connection and closes it without reading the
// body, so the client's upload is interrupted partway through.
func newFailingIngestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	return srv, srv.Close
}

// --- digest verification ---

// TestServer_IngestRejectsMismatchedMD5 is the case that was possible before this
// change: content stored under a digest it does not have, indexed as though it
// did. Check then tells every other replica this content is backed up here.
func TestServer_IngestRejectsMismatchedMD5(t *testing.T) {
	_, ts, store, storeDir := newTestServerWithStore(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(path, []byte("THE REAL CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	const claimed = "0000000000000000000000000000dead"

	err := NewClient(ts.URL).Ingest(path, claimed)
	if err == nil {
		t.Fatal("Ingest accepted a file whose digest does not match")
	}
	if !strings.Contains(err.Error(), "md5 mismatch") {
		t.Errorf("the error should say the digest did not match; got: %v", err)
	}

	// Nothing may be left under the claimed address: a file whose content does not
	// match its location is worse than an absent one, because the next replica
	// to ask about this digest would find it.
	assertNoFiles(t, storeDir)
	files, err := store.FindByMD5(claimed)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("the master indexed %d record(s) for the claimed digest", len(files))
	}
}

// TestServer_IngestMismatchLeavesOtherFilesAlone makes sure the cleanup removes
// the rejected upload and not the store.
func TestServer_IngestMismatchLeavesOtherFilesAlone(t *testing.T) {
	_, ts, _ := newTestServer(t)
	client := NewClient(ts.URL)

	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	if err := os.WriteFile(good, []byte("good content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Ingest(good, md5OfFile(t, good)); err != nil {
		t.Fatalf("the honest upload failed: %v", err)
	}

	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(bad, []byte("different content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Ingest(bad, md5OfFile(t, good)); err == nil {
		t.Fatal("the mismatched upload was accepted")
	}

	found, err := client.Check(md5OfFile(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("the earlier honest upload was removed along with the rejected one")
	}
}

// TestServer_IngestAcceptsMatchingMD5 is the ordinary path, stated so the check
// cannot be tightened into rejecting valid uploads.
func TestServer_IngestAcceptsMatchingMD5(t *testing.T) {
	_, ts, store := newTestServer(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "fine.txt")
	content := []byte("content that matches its digest")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := md5OfFile(t, path)

	if err := NewClient(ts.URL).Ingest(path, sum); err != nil {
		t.Fatalf("a matching digest was rejected: %v", err)
	}
	records, err := store.FindByMD5(sum)
	if err != nil || len(records) != 1 {
		t.Fatalf("want 1 record, got %d (err=%v)", len(records), err)
	}
	got, err := os.ReadFile(records[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("the stored bytes are not what was sent")
	}
}

// TestServer_IngestRejectsMalformedMD5 covers the format check that replaced the
// old length test. The store layout is derived from this value, so the format
// matters as well as the length.
func TestServer_IngestRejectsMalformedMD5(t *testing.T) {
	_, ts, _, storeDir := newTestServerWithStore(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{
		"",
		"ab",
		"aabbccdd00112233445566778899aab",   // 31
		"aabbccdd00112233445566778899aabbc", // 33
		"gabbccdd00112233445566778899aabb",  // not hex
		"../../etc/passwd00000000000000000", // not hex, and looks like a path
	} {
		if err := NewClient(ts.URL).Ingest(path, bad); err == nil {
			t.Errorf("the master accepted md5 %q", bad)
		}
	}
	assertStoreUntouched(t, storeDir)
}

// assertNoFiles fails if the store directory holds any file.
//
// Used for an upload rejected for a content mismatch. Its address was
// legitimate, and the directories it created are shared with every other file of
// the same digest, so what must not survive is the file.
func assertNoFiles(t *testing.T, storeDir string) {
	t.Helper()
	var found []string
	err := filepath.Walk(storeDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk store: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("the store still holds %d file(s): %v", len(found), found)
	}
}

// assertStoreUntouched fails if anything at all is left under the store,
// directories included.
//
// Used for a request rejected on its format, where even an empty directory is
// residue: those are what a traversal attempt leaves behind when the address was
// never validated.
func assertStoreUntouched(t *testing.T, storeDir string) {
	t.Helper()
	var found []string
	err := filepath.Walk(storeDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p != storeDir {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk store: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("the store still holds %d entr(ies): %v", len(found), found)
	}
}

// TestServer_IngestRejectsPathTraversalInMD5 covers the reason the digest is
// format-checked rather than merely length-checked. The md5 is split into
// directory levels for the content-addressed layout, so a value carrying "../"
// escaped the store entirely:
//
//	{"ok":true,"path":"/tmp/outside-the-store/payload.txt"}
//
// Any client that can reach /v1/ingest could write wherever the master's process
// can. A length check does not stop it: the payload is longer than four
// characters.
func TestServer_IngestRejectsPathTraversalInMD5(t *testing.T) {
	_, ts, _, storeDir := newTestServerWithStore(t)

	dir := t.TempDir()
	payload := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(payload, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Aim the traversal at a directory this test owns and can write to, so a
	// successful escape is observable rather than hidden behind a permission
	// error on some parent of the temp tree.
	victim := t.TempDir()
	rel, err := filepath.Rel(storeDir, victim)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	traversal := filepath.Join(rel, "escaped")
	want := filepath.Join(victim, "escaped")

	// The handler splits the value after two characters and joins the halves, so
	// this is where the upload would land. Deriving it the same way keeps the
	// assertion pointed at the real destination.
	got := filepath.Join(storeDir, traversal[:2], traversal[2:])
	if got != want {
		t.Fatalf("test arithmetic: escape target is %s, wanted %s", got, want)
	}

	if err := NewClient(ts.URL).Ingest(payload, traversal); err == nil {
		t.Fatal("the master accepted an md5 containing a path traversal")
	}
	if _, err := os.Stat(want); err == nil {
		t.Errorf("the upload escaped the store: %s was created", want)
	}
	// Nothing under the store either: a request rejected on its format must not
	// have touched the filesystem at all.
	assertStoreUntouched(t, storeDir)
}
