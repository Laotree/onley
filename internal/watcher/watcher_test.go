package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const testDebounce = 60 * time.Millisecond

// waitFor polls until cond holds or the deadline passes. fsnotify delivers on
// the kernel's schedule, so a fixed sleep would make the tests flaky on a busy
// machine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// hasEvent reports whether an event for path matching pred was seen.
func hasEvent(events []Event, path string, pred func(Event) bool) bool {
	for _, ev := range events {
		if ev.Path == path && pred(ev) {
			return true
		}
	}
	return false
}

// recorder accumulates every event a watcher emits, so a test can assert on
// events that arrived at different times. Draining the channel per assertion
// would discard the earlier half of a sequence.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func record(w *Watcher) *recorder {
	r := &recorder{}
	go func() {
		for ev := range w.Events() {
			r.mu.Lock()
			r.events = append(r.events, ev)
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *recorder) seen() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func indexed(ev Event) bool { return ev.Create && ev.Write && ev.Err == nil }
func removed(ev Event) bool { return ev.Remove }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestIndexesFileAfterItStopsChanging is the core guarantee: a file that has
// been created but is still open for writing must not be reported yet.
func TestIndexesFileAfterItStopsChanging(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	path := filepath.Join(root, "a.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Written in pieces with pauses longer than the debounce window, so the
	// file is quiet between writes but not finished.
	for i := 0; i < 3; i++ {
		f.WriteString("chunk")
		f.Sync()
		time.Sleep(3 * testDebounce)
	}
	f.Close()

	waitFor(t, "the file to be reported", func() bool {
		return hasEvent(r.seen(), path, indexed)
	})
}

// TestWritesFasterThanDebounceAreNotIndexed covers the guarantee the debounce
// window can actually make: a burst of writes that never leaves the file quiet
// for the whole window produces no event, so no half-written file is indexed.
//
// A pause longer than the window is a different case and cannot be covered
// here: a writer that stops for longer than the debounce is indistinguishable
// from one that finished, and the file is then indexed. Raise -debounce to
// cover writes that stall.
func TestWritesFasterThanDebounceAreNotIndexed(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	path := filepath.Join(root, "burst.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Each write is followed by a gap shorter than the window, so the window
	// never closes while the file is still growing.
	for i := 0; i < 12; i++ {
		f.Write(make([]byte, 4096))
		f.Sync()
		time.Sleep(testDebounce / 4)
	}

	for _, ev := range r.seen() {
		if ev.Path == path && indexed(ev) {
			t.Fatalf("indexed a file whose writes never paused for the debounce window")
		}
	}
	f.Close()

	waitFor(t, "the finished file to be indexed", func() bool {
		return hasEvent(r.seen(), path, indexed)
	})
}

// TestSizeGrowthReopensTheWindow covers the case where a write pauses for longer
// than the debounce window but the size still changes afterwards.
func TestSizeGrowthReopensTheWindow(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	path := filepath.Join(root, "slow.bin")
	write(t, path, "start")
	waitFor(t, "the first write to settle", func() bool {
		return hasEvent(r.seen(), path, indexed)
	})

	// Grow the file after the window has already closed. The quiet window must
	// reopen rather than let the new bytes through unindexed.
	write(t, path, "start-and-then-more")
	waitFor(t, "the grown file to be reported again", func() bool {
		return hasEvent(r.seen(), path, indexed)
	})
}

// TestRemoveIsReported checks that a deleted path is reported as gone.
func TestRemoveIsReported(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	path := filepath.Join(root, "gone.txt")
	write(t, path, "x")
	waitFor(t, "the file to be indexed", func() bool {
		return hasEvent(r.seen(), path, indexed)
	})

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	waitFor(t, "the removal to be reported", func() bool {
		return hasEvent(r.seen(), path, removed)
	})
}

// TestRenameReportsRemoveThenCreate covers an editor's atomic save: the content
// lands on the destination and the temporary file goes away. A caller that
// indexes creates and drops removes must end up with the destination indexed
// and no record of the temporary file.
func TestRenameReportsRemoveThenCreate(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	target := filepath.Join(root, "doc.txt")
	write(t, target, "original")
	waitFor(t, "the original to be indexed", func() bool {
		return hasEvent(r.seen(), target, indexed)
	})

	tmp := filepath.Join(root, ".doc.txt.tmp")
	write(t, tmp, "edited content")
	waitFor(t, "the temporary file to be indexed", func() bool {
		return hasEvent(r.seen(), tmp, indexed)
	})
	if err := os.Rename(tmp, target); err != nil {
		t.Fatalf("rename: %v", err)
	}

	waitFor(t, "the rename to settle", func() bool {
		events := r.seen()
		return hasEvent(events, tmp, removed) && hasEvent(events, target, indexed)
	})
}

// TestNestedDirectoryIsWatched asserts recursion: a file written into a
// subdirectory that existed before the watcher started must be seen.
func TestNestedDirectoryIsWatched(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	path := filepath.Join(nested, "deep.txt")
	write(t, path, "deep")
	waitFor(t, "the nested file to be reported", func() bool {
		return hasEvent(r.seen(), path, indexed)
	})
}

// TestDirectoryCreatedAfterStartIsWatched covers a directory that appears while
// the watcher runs: it needs a new watch, otherwise files inside it are lost.
func TestDirectoryCreatedAfterStartIsWatched(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	fresh := filepath.Join(root, "fresh")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(fresh, "inside.txt")
	write(t, path, "inside")
	waitFor(t, "the file in the new directory to be reported", func() bool {
		return hasEvent(r.seen(), path, indexed)
	})
}

// TestFilesAlreadyInsideNewDirectoryAreReported covers the race between a
// directory being created and its first files appearing: those writes happen
// before a watch on the new directory can exist, so they only arrive by walking
// it.
func TestFilesAlreadyInsideNewDirectoryAreReported(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	// Build the tree somewhere else, then move it in. The move is one rename,
	// so by the time the watcher sees the directory the files are already
	// there, exactly like an unpacked archive landing in one step.
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(filepath.Join(staging, "inner"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, filepath.Join(staging, "inner", "packed.txt"), "packed")
	unpacked := filepath.Join(root, "unpacked")
	if err := os.Rename(staging, unpacked); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// The event carries the path under its new home, not the one it had in
	// the staging directory.
	path := filepath.Join(unpacked, "inner", "packed.txt")

	waitFor(t, "the packed file to be reported", func() bool {
		return hasEvent(r.seen(), path, indexed)
	})

	// The nested directory must now be watched too, or a later write into it
	// would go unseen.
	later := filepath.Join(unpacked, "inner", "later.txt")
	write(t, later, "later")
	waitFor(t, "a file written into the moved tree to be reported", func() bool {
		return hasEvent(r.seen(), later, indexed)
	})
}

// TestExistingFilesAreReported covers the startup sweep, which is what lets
// watch replace a separate scan.
func TestExistingFilesAreReported(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "already.txt")
	write(t, want, "already here")

	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	waitFor(t, "the pre-existing file to be reported", func() bool {
		return hasEvent(r.seen(), want, indexed)
	})
}

// TestSymlinkIsIgnored keeps the watcher aligned with scan, which indexes
// regular files only. Following a link would index the same bytes twice.
func TestSymlinkIsIgnored(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.txt")
	write(t, target, "content")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	go w.Run(context.Background())
	r := record(w)

	waitFor(t, "the real file to be reported", func() bool {
		return hasEvent(r.seen(), target, indexed)
	})
	if hasEvent(r.seen(), link, indexed) {
		t.Fatalf("indexed a symlink, which scan does not do either")
	}
}

// TestContextCancelClosesEvents checks the shutdown path the command relies on.
func TestContextCancelClosesEvents(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	cancel()

	select {
	case _, ok := <-w.Events():
		if ok {
			// Drain anything already queued before the close.
			for range w.Events() {
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Events was not closed after the context was cancelled")
	}
}

// TestNewRejectsMissingRoot keeps the error path from the command honest.
func TestNewRejectsMissingRoot(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "does-not-exist"), testDebounce); err == nil {
		t.Fatalf("New accepted a root that does not exist")
	}
}
