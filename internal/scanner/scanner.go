package scanner

import (
	"crypto/md5"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"onley/internal/db"
)

// Progress is emitted for every notable event during a scan.
// Done=false means a worker just started hashing Path (not yet finished).
// Done=true  means the file has been fully processed (hashed+upserted or skipped).
type Progress struct {
	Path     string
	WorkerID int  // which worker (0-indexed)
	Current  int  // total completed so far (only meaningful when Done=true)
	Active   int  // number of workers currently hashing
	Done     bool // false=started, true=finished
	Skipped  bool // true when file was unchanged and skipped (Done must be true)
	Err      error
}

// Scan walks root and indexes every regular file using a pool of worker goroutines.
// Each worker does a Lookup + (optional) MD5 hash concurrently; a single writer
// serializes all Upserts to avoid SQLite write contention.
// Files whose size and mtime match the stored record are skipped (resume support).
func Scan(root string, store *db.DB, workers int) <-chan Progress {
	ch := make(chan Progress, workers)

	type job struct {
		path  string
		size  int64
		mtime int64
	}

	type result struct {
		path     string
		workerID int
		active   int
		record   *db.FileRecord // non-nil: needs Upsert
		skipped  bool
		started  bool // true = worker just started hashing; no record yet
		err      error
	}

	go func() {
		defer close(ch)

		jobs := make(chan job, workers*2)
		results := make(chan result, workers*2)

		var active atomic.Int64

		// Walk goroutine: enumerate files and feed the job queue.
		go func() {
			defer close(jobs)
			filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					results <- result{path: path, err: err}
					return nil // keep walking
				}
				if !d.Type().IsRegular() {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					results <- result{path: path, err: err}
					return nil
				}
				jobs <- job{path: path, size: info.Size(), mtime: info.ModTime().Unix()}
				return nil
			})
		}()

		// Worker goroutines: Lookup + hash (the expensive part, runs concurrently).
		var wg sync.WaitGroup
		for id := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					existing, err := store.Lookup(j.path)
					if err != nil {
						results <- result{path: j.path, workerID: id, err: err}
						continue
					}
					if existing != nil && existing.Size == j.size && existing.Mtime == j.mtime {
						results <- result{path: j.path, workerID: id, skipped: true}
						continue
					}

					// Signal that this worker has started hashing.
					results <- result{path: j.path, workerID: id, started: true}

					n := int(active.Add(1))
					hash, err := hashFile(j.path)
					active.Add(-1)

					if err != nil {
						results <- result{path: j.path, workerID: id, err: err}
						continue
					}
					results <- result{
						path:     j.path,
						workerID: id,
						active:   n,
						record: &db.FileRecord{
							Path:  j.path,
							Name:  filepath.Base(j.path),
							Size:  j.size,
							MD5:   hash,
							Mtime: j.mtime,
						},
					}
				}
			}()
		}

		// Close results once all workers have finished.
		go func() {
			wg.Wait()
			close(results)
		}()

		// Single writer: serializes Upserts and emits Progress events.
		var count int
		for r := range results {
			if r.err != nil {
				ch <- Progress{Path: r.path, WorkerID: r.workerID, Done: true, Err: r.err}
				continue
			}
			if r.started {
				// Worker just started hashing — let caller update the display.
				ch <- Progress{Path: r.path, WorkerID: r.workerID, Done: false}
				continue
			}
			if r.skipped {
				count++
				ch <- Progress{Path: r.path, WorkerID: r.workerID, Current: count, Done: true, Skipped: true}
				continue
			}
			if err := store.Upsert(*r.record); err != nil {
				ch <- Progress{Path: r.path, WorkerID: r.workerID, Done: true, Err: err}
				continue
			}
			count++
			ch <- Progress{Path: r.path, WorkerID: r.workerID, Current: count, Active: r.active, Done: true}
		}
	}()

	return ch
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
