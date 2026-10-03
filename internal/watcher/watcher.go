package watcher

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultDebounce is how long a file must stop changing before it is indexed.
const DefaultDebounce = 500 * time.Millisecond

// Event is one settled change under the watched root.
type Event struct {
	// Path is the absolute path the event concerns.
	Path string
	// Create and Write are reported together: both mean the file is present
	// and has settled, so the caller indexes it.
	Create bool
	Write  bool
	// Remove means the path is gone. The caller drops it from the index and
	// must not touch the file system: a rename shows up as a remove of the old
	// path followed by a create of the new one.
	Remove bool
	// Err is set when a path could not be read or watched. The path may still
	// be valid, so the caller reports it and keeps watching.
	Err error
}

// Watcher reports settled file changes under a directory tree.
//
// fsnotify delivers one Write per write call, so a single large copy produces
// hundreds of events spread over seconds. Handing each one to the caller would
// index the file while it is still growing. The watcher therefore holds a
// changed path until no event for it arrives for Debounce, then reports it. It
// compares size and mtime across the quiet window as well: a path whose size
// changed while waiting is still being written, so it waits again.
//
// Unlike fsnotify, the watcher is recursive and covers the files that already
// exist when it starts, which is what makes it behave like a scan that keeps
// running.
type Watcher struct {
	// Debounce is the quiet window a path must stay unchanged for before it
	// is reported.
	Debounce time.Duration

	fsw *fsnotify.Watcher
	out chan Event

	mu      sync.Mutex
	pending map[string]*pending
	closed  bool
}

type pending struct {
	timer *time.Timer
	size  int64
	mtime int64
}

// New creates a watcher for root. The whole existing tree is registered, so a
// change made between this call and the first Run is not missed.
func New(root string, debounce time.Duration) (*Watcher, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create watcher: %w", err)
	}
	w := &Watcher{
		Debounce: debounce,
		fsw:      fsw,
		out:      make(chan Event, 256),
		pending:  map[string]*pending{},
	}
	if err := w.addTree(absRoot); err != nil {
		fsw.Close()
		return nil, err
	}
	if err := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		w.onUpsert(path)
		return nil
	}); err != nil {
		fsw.Close()
		return nil, fmt.Errorf("walk root: %w", err)
	}
	return w, nil
}

// Events returns the channel settled changes arrive on. It is closed when the
// context passed to Run is cancelled or Close is called.
func (w *Watcher) Events() <-chan Event {
	return w.out
}

// Close stops the watcher, cancels every pending wait and releases the watches.
// It is safe to call more than once.
func (w *Watcher) Close() error {
	w.stopTimers()
	return w.fsw.Close()
}

// Run delivers events until ctx is cancelled or the watcher is closed.
func (w *Watcher) Run(ctx context.Context) {
	defer w.shutdown()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.emit(Event{Err: err})
		}
	}
}

// handle routes one raw fsnotify event. Chmod is ignored: it carries no
// content change and macOS emits it for every file that is merely touched.
func (w *Watcher) handle(ev fsnotify.Event) {
	switch {
	case ev.Has(fsnotify.Remove), ev.Has(fsnotify.Rename):
		w.gone(ev.Name)
	case ev.Has(fsnotify.Create):
		// A new file arrives before its first write, and an editor's atomic
		// save shows up as a create of the destination. Both settle through
		// the same quiet window.
		w.onUpsert(ev.Name)
	case ev.Has(fsnotify.Write):
		// A Write on a directory means its contents changed on kqueue and
		// Windows, so it may name a directory rather than a file.
		w.onUpsert(ev.Name)
	}
}

// gone reports a path as removed and cancels any wait on it.
func (w *Watcher) gone(path string) {
	w.cancel(path)
	w.emit(Event{Path: path, Remove: true})
}

// onUpsert reacts to a create or a write on path.
func (w *Watcher) onUpsert(path string) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The path went away between the event and this stat. Reporting it
			// as gone keeps a create-then-delete burst from leaving an index
			// entry behind for a file that no longer exists.
			w.gone(path)
		} else {
			w.emit(Event{Path: path, Err: err})
		}
		return
	}
	if info.IsDir() {
		// A directory created after startup needs its own watch, otherwise
		// files written into it are never seen. Files already inside it were
		// created before that watch existed, so they are queued here.
		w.absorb(path)
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	w.hold(path, info.Size(), info.ModTime().Unix())
}

// absorb registers a newly created directory together with its subdirectories,
// then queues the files already inside it.
func (w *Watcher) absorb(dir string) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir {
				if err := w.fsw.Add(path); err != nil {
					w.emit(Event{Path: path, Err: err})
				}
			}
			return nil
		}
		if d.Type().IsRegular() {
			w.onUpsert(path)
		}
		return nil
	})
}

// hold opens or extends the quiet window for path, using size and mtime as the
// baseline the window is later checked against.
func (w *Watcher) hold(path string, size, mtime int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}

	if p, ok := w.pending[path]; ok {
		p.size, p.mtime = size, mtime
		p.timer.Reset(w.Debounce)
		return
	}
	p := &pending{size: size, mtime: mtime}
	p.timer = time.AfterFunc(w.Debounce, func() { w.checkStable(path) })
	w.pending[path] = p
}

// cancel drops the pending wait for path.
func (w *Watcher) cancel(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p, ok := w.pending[path]; ok {
		p.timer.Stop()
		delete(w.pending, path)
	}
}

// checkStable runs when a path has been quiet for Debounce. It re-stats the path and
// compares against the baseline taken when the window opened: an unchanged size
// and mtime mean the write finished, anything else means the file grew while we
// waited and must not be hashed yet.
func (w *Watcher) checkStable(path string) {
	w.mu.Lock()
	p, ok := w.pending[path]
	if ok {
		delete(w.pending, path)
	}
	w.mu.Unlock()
	if !ok {
		// Cancelled or already reported while the timer was running.
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The file was unlinked without a Remove event reaching us, which
			// happens when the last descriptor closes after the unlink.
			w.emit(Event{Path: path, Remove: true})
			return
		}
		w.emit(Event{Path: path, Err: err})
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	if size, mtime := info.Size(), info.ModTime().Unix(); size != p.size || mtime != p.mtime {
		// Still changing, so reopen the window with the new baseline. This is
		// what keeps a half-written file out of the index.
		w.hold(path, size, mtime)
		return
	}
	w.emit(Event{Path: path, Create: true, Write: true})
}

// addTree registers a watch on dir and every directory below it.
func (w *Watcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if err := w.fsw.Add(path); err != nil {
			return fmt.Errorf("watch %s: %w", path, err)
		}
		return nil
	})
}

// stopTimers cancels every pending wait.
func (w *Watcher) stopTimers() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.pending {
		p.timer.Stop()
	}
	w.pending = map[string]*pending{}
}

// shutdown closes the event channel. It runs once, after the last emit, so no
// timer can send on a closed channel.
func (w *Watcher) shutdown() {
	w.stopTimers()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.out)
}

// emit delivers an event, dropping it when the consumer has fallen behind. A
// send that blocks here would stop draining the fsnotify queue and could
// overflow the kernel buffer, losing every later event instead of this one.
func (w *Watcher) emit(ev Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	select {
	case w.out <- ev:
	default:
	}
}
