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
	t.Helper()
	store := openMemDB(t)
	storeDir := t.TempDir()
	srv := NewServer(store, storeDir)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, store
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
	const md5sum = "aabbccdd00112233445566778899aabb"

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
