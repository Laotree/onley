package replica

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
