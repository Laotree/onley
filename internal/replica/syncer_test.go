package replica

import (
	"crypto/md5"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"onley/internal/db"
)

// countingMaster wraps the real handler and counts what it was asked.
type countingMaster struct {
	inner   http.Handler
	mu      sync.Mutex
	checks  map[string]int
	ingests int
}

func newCountingMaster(t *testing.T) (*Client, *countingMaster, *db.DB) {
	t.Helper()
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("master db.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	inner := NewServer(store, t.TempDir()).Handler()
	c := &countingMaster{inner: inner, checks: map[string]int{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/check":
			c.mu.Lock()
			c.checks[r.URL.Query().Get("md5")]++
			c.mu.Unlock()
		case "/v1/ingest":
			c.mu.Lock()
			c.ingests++
			c.mu.Unlock()
		}
		c.inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return NewClient(ts.URL), c, store
}

func (c *countingMaster) checkCount(sum string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checks[sum]
}

func (c *countingMaster) totalChecks() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.checks)
}

func (c *countingMaster) ingestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ingests
}

// tempFile writes content to a new file and returns its path and digest.
func tempFile(t *testing.T, content string) (path, sum string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, fmt.Sprintf("%x", md5.Sum(data))
}

// TestSyncer_UploadsWithoutTouchingTheLocalFile is the property this syncer
// exists to have. Every new file takes the migrate path in `replica check`,
// which uploads and then removes the local copy, so a syncing watcher would
// empty the directory it was told to watch.
func TestSyncer_UploadsWithoutTouchingTheLocalFile(t *testing.T) {
	client, rec, masterDB := newCountingMaster(t)
	path, sum := tempFile(t, "content to push")

	s := NewSyncer(client, time.Hour)
	s.Enqueue(sum, path)
	res := s.Sync()

	if res.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1", res.Uploaded)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the syncer removed the local file: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "content to push" {
		t.Errorf("the local file was altered: %q err=%v", got, err)
	}
	if files, err := masterDB.FindByMD5(sum); err != nil || len(files) != 1 {
		t.Fatalf("master holds %d record(s) for the digest (err=%v)", len(files), err)
	}
	if rec.ingestCount() != 1 {
		t.Errorf("master received %d upload(s), want 1", rec.ingestCount())
	}
}

// TestSyncer_SkipsContentTheMasterAlreadyHas: a second copy is not sent again,
// and is not deleted either.
func TestSyncer_SkipsContentTheMasterAlreadyHas(t *testing.T) {
	client, rec, _ := newCountingMaster(t)
	path, sum := tempFile(t, "already backed up")
	if err := client.Ingest(path, sum); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	before := rec.ingestCount()

	s := NewSyncer(client, time.Hour)
	s.Enqueue(sum, path)
	res := s.Sync()

	if res.AlreadyPresent != 1 {
		t.Errorf("AlreadyPresent = %d, want 1", res.AlreadyPresent)
	}
	if res.Uploaded != 0 {
		t.Errorf("Uploaded = %d, want 0 for content the master already has", res.Uploaded)
	}
	if got := rec.ingestCount(); got != before {
		t.Errorf("the content was uploaded again: %d request(s) where %d was expected", got, before)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the local copy was removed even though nothing was wrong: %v", err)
	}
}

// TestSyncer_AsksOncePerDigest: files sharing content are one question and one
// upload, and every one of them is left alone.
func TestSyncer_AsksOncePerDigest(t *testing.T) {
	client, rec, _ := newCountingMaster(t)
	a, sumA := tempFile(t, "shared content")
	b, sumB := tempFile(t, "shared content")
	c, sumC := tempFile(t, "different")
	if sumA != sumB {
		t.Fatalf("the fixture does not share a digest: %s vs %s", sumA, sumB)
	}

	s := NewSyncer(client, time.Hour)
	s.Enqueue(sumA, a)
	s.Enqueue(sumB, b)
	s.Enqueue(sumC, c)
	res := s.Sync()

	// Uploaded counts files, which is what the caller sees: three files were
	// pushed. The master received two requests, because two of those files are
	// the same bytes.
	if res.Uploaded != 3 {
		t.Errorf("Uploaded = %d, want 3 files", res.Uploaded)
	}
	if got := rec.totalChecks(); got != 2 {
		t.Errorf("the master was asked about %d digest(s), want 2", got)
	}
	if n := rec.checkCount(sumA); n != 1 {
		t.Errorf("the shared digest was asked about %d time(s), want 1", n)
	}
	if rec.ingestCount() != 2 {
		t.Errorf("master received %d upload request(s), want 2 for 3 files", rec.ingestCount())
	}
	for _, p := range []string{a, b, c} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed", p)
		}
	}
}

// TestSyncer_RetriesFailedPaths: an unreachable master must not lose the queue,
// and must not cost the local file either.
func TestSyncer_RetriesFailedPaths(t *testing.T) {
	client := NewClient("http://127.0.0.1:1")
	path, sum := tempFile(t, "will not get through")

	s := NewSyncer(client, time.Hour)
	s.Enqueue(sum, path)
	res := s.Sync()

	if res.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", res.Failed)
	}
	if s.Pending() != 1 {
		t.Errorf("Pending = %d, want the path queued again", s.Pending())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a failed upload removed the local file: %v", err)
	}
}

// TestSyncer_AFileRemovedBeforeItsRoundIsAFailureNotASilentDrop: a path that is
// gone when the round runs is neither uploaded nor left queued forever.
func TestSyncer_RemovedFileDoesNotRetryForever(t *testing.T) {
	client, _, _ := newCountingMaster(t)
	s := NewSyncer(client, time.Hour)
	sum := fmt.Sprintf("%x", md5.Sum([]byte("gone")))
	s.Enqueue(sum, filepath.Join(t.TempDir(), "not-here"))

	res := s.Sync()
	if res.Failed != 1 {
		t.Errorf("Failed = %d, want 1", res.Failed)
	}
	// It stays queued: a file can come back (an editor rewriting it), and a
	// permanently missing path just costs one failed round each time.
	if s.Pending() != 1 {
		t.Errorf("Pending = %d, want the path retried", s.Pending())
	}
}

// TestSyncer_QueueIsTakenBeforeWorkStarts: a path queued while a round is in
// flight is not swallowed by it.
func TestSyncer_QueueIsTakenBeforeWorkStarts(t *testing.T) {
	client, _, _ := newCountingMaster(t)
	path, sum := tempFile(t, "first")
	s := NewSyncer(client, time.Hour)
	s.Enqueue(sum, path)

	s.Sync()
	if s.Pending() != 0 {
		t.Fatalf("the queue was not drained: %d", s.Pending())
	}
	s.Enqueue(sum, path)
	if s.Pending() != 1 {
		t.Errorf("a path queued after the batch was taken was lost")
	}
}

// TestSyncer_NextIntervalBacksOffAndRecovers: a master that is down must not be
// probed every interval for hours, and the wait must come back afterwards.
func TestSyncer_NextIntervalBacksOffAndRecovers(t *testing.T) {
	s := NewSyncer(NewClient("http://127.0.0.1:1"), time.Second)

	if got := s.NextInterval(true); got != time.Second {
		t.Fatalf("first wait = %v, want the interval", got)
	}
	for _, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if got := s.NextInterval(false); got != want {
			t.Errorf("wait after a failure = %v, want %v", got, want)
		}
	}
	for i := 0; i < 20; i++ {
		if got := s.NextInterval(false); got > 16*time.Second {
			t.Fatalf("wait = %v, want it capped at 16x the interval", got)
		}
	}
	if got := s.NextInterval(true); got != time.Second {
		t.Errorf("wait after a success = %v, want the interval back", got)
	}
}

// TestSyncer_EmptyQueueIsANoOp: the loop calls this on every tick whether or not
// anything changed, so an empty round must not report work or slow the interval
// down.
func TestSyncer_EmptyQueueIsANoOp(t *testing.T) {
	client, rec, _ := newCountingMaster(t)
	s := NewSyncer(client, time.Hour)
	res := s.Sync()
	if res.Uploaded != 0 || res.AlreadyPresent != 0 || res.Failed != 0 {
		t.Errorf("an empty round reported %+v", res)
	}
	if !res.OK() {
		t.Error("an empty round should count as OK so the interval does not back off")
	}
	if rec.totalChecks() != 0 {
		t.Error("an empty round asked the master something")
	}
}

func TestSyncer_DefaultInterval(t *testing.T) {
	s := NewSyncer(NewClient("http://127.0.0.1:1"), 0)
	if got := s.NextInterval(true); got != DefaultSyncInterval {
		t.Errorf("interval = %v, want the default %v", got, DefaultSyncInterval)
	}
}

// TestSyncer_RetriesWhenTheUploadFails reaches the branch the previous test
// cannot: a master that answers "I do not have it" and then refuses the upload.
// The path has to stay queued, or a transient upload failure silently drops the
// file from the push queue forever.
func TestSyncer_RetriesWhenTheUploadFails(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("master db.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	inner := NewServer(store, t.TempDir()).Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ingest" {
			http.Error(w, "disk full", http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()

	path, sum := tempFile(t, "will not fit")
	s := NewSyncer(NewClient(ts.URL), time.Hour)
	s.Enqueue(sum, path)
	res := s.Sync()

	if res.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", res.Failed)
	}
	if s.Pending() != 1 {
		t.Errorf("Pending = %d, want the path queued for another attempt", s.Pending())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a refused upload removed the local file: %v", err)
	}
}
