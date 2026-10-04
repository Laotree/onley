package replica

import (
	"sync"
	"time"
)

// DefaultSyncInterval is how often a Syncer drains its queue when no interval is
// given.
const DefaultSyncInterval = 30 * time.Second

// Syncer pushes files to a master in batches.
//
// It uploads and nothing else. It never removes a local file, because the
// caller that could plausibly want that — a watcher pointed at a directory a
// person saves things into — is exactly where it would be most surprising. The
// migrate path in `replica check` does delete locally, and a new file always
// takes it, so a syncing watcher would empty the directory it was told to watch.
// Deletion stays in `replica check`, which asks first.
//
// The cost of that choice is worth stating: the master holds a copy and the
// local copy stays, so disk use goes up rather than down. This is a push, not
// the consolidation that gives `replica check` its purpose.
//
// Paths arrive as they are indexed, so the first batch is whatever existed when
// the watcher started — its startup sweep reports the whole tree. That is the
// reconcile which catches whatever a previous run missed, and it needs no
// separate pass.
type Syncer struct {
	client   *Client
	interval time.Duration
	// maxBackoff caps how far the wait grows while the master stays
	// unreachable. Without it, a master that is down for a week is probed every
	// interval and reports itself every interval.
	maxBackoff time.Duration

	mu      sync.Mutex
	pending map[string]string // path -> digest
	current time.Duration
}

// NewSyncer creates a syncer that drains its queue every interval.
func NewSyncer(client *Client, interval time.Duration) *Syncer {
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	return &Syncer{
		client:     client,
		interval:   interval,
		maxBackoff: interval * 16,
		pending:    map[string]string{},
		current:    interval,
	}
}

// Enqueue adds a path and the digest already computed for it.
//
// The digest comes from the caller because it has just read the file to index
// it. Hashing again here would read every file twice, and this is the code
// whose whole job is to be cheap about large files.
func (s *Syncer) Enqueue(md5sum, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[path] = md5sum
}

// Pending reports how many paths are waiting.
func (s *Syncer) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// NextInterval returns how long the caller should wait before draining again.
//
// A round that failed doubles the wait; a successful one puts it back. The
// waiting is the caller's to do, because it is the one with a context.
func (s *Syncer) NextInterval(lastRoundOK bool) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lastRoundOK {
		s.current = s.interval
		return s.current
	}
	s.current *= 2
	if s.current > s.maxBackoff {
		s.current = s.maxBackoff
	}
	return s.current
}

// Result counts what one round did.
type Result struct {
	// Uploaded is the number of files sent to the master.
	Uploaded int
	// AlreadyPresent is the number the master already held, so nothing was sent.
	// Those files remain on disk.
	AlreadyPresent int
	// Failed is the number of paths that could not be dealt with. They are
	// queued again rather than dropped.
	Failed int
}

// OK reports whether the round had no failures, which is what the backoff keys
// off: one unreachable master should slow the retries down, not a file that
// happens to be unreadable.
func (r Result) OK() bool { return r.Failed == 0 }

// Sync sends the queued files.
//
// The queue is taken before any work starts, so a path that changes while the
// round runs is queued afresh by the next Enqueue rather than lost. A path that
// fails goes back for the next round.
//
// One question per digest across the batch rather than one per file: the answer
// depends only on the content, so two paths carrying the same bytes get one
// answer between them and one upload.
func (s *Syncer) Sync() Result {
	s.mu.Lock()
	batch := make(map[string]string, len(s.pending))
	for path, sum := range s.pending {
		batch[path] = sum
		delete(s.pending, path)
	}
	s.mu.Unlock()

	var res Result
	if len(batch) == 0 {
		return res
	}

	// Group by digest so each distinct content is settled once. One path carries
	// the upload; the others are the same bytes under a different name.
	firstOf := map[string]string{}
	for path, sum := range batch {
		if _, seen := firstOf[sum]; !seen {
			firstOf[sum] = path
		}
	}

	for sum, path := range firstOf {
		found, err := s.client.Check(sum)
		if err != nil {
			res.Failed++
			s.Enqueue(sum, path)
			continue
		}
		if found {
			// The master has these bytes. The local copies stay: a second copy of
			// content that is backed up costs disk, while deleting it and being
			// wrong about the backup costs the file.
			res.AlreadyPresent += filesWithDigest(batch, sum)
			continue
		}
		if err := s.client.Ingest(path, sum); err != nil {
			res.Failed++
			s.Enqueue(sum, path)
			continue
		}
		res.Uploaded += filesWithDigest(batch, sum)
	}
	return res
}

// filesWithDigest counts how many queued paths carry the given digest, so the
// result counts files the way the caller sees them rather than questions asked.
func filesWithDigest(batch map[string]string, sum string) int {
	n := 0
	for _, s := range batch {
		if s == sum {
			n++
		}
	}
	return n
}
