package runner

import (
	"io"
	"sync"
	"time"
)

// activityWriter forwards a lane's output while remembering when it last
// produced any. The watchdog reads that timestamp to decide whether the lane
// has gone silent; wrapping the writer is what makes silence observable
// without asking the child process anything.
type activityWriter struct {
	mu    sync.Mutex
	dst   io.Writer
	last  time.Time
	bytes int64
	now   func() time.Time
}

func newActivityWriter(dst io.Writer, now func() time.Time) *activityWriter {
	if now == nil {
		now = time.Now
	}
	return &activityWriter{dst: dst, last: now(), now: now}
}

// Write records the activity and forwards to the destination.
func (w *activityWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.last = w.now()
	w.bytes += int64(len(p))
	dst := w.dst
	w.mu.Unlock()
	// The write itself happens outside the lock so a slow log file cannot
	// block the watchdog's read of last.
	return dst.Write(p)
}

// LastActivity reports when output was last seen.
func (w *activityWriter) LastActivity() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// IdleFor reports how long the lane has been silent as of now.
func (w *activityWriter) IdleFor(now time.Time) time.Duration {
	return now.Sub(w.LastActivity())
}

// Bytes reports how much output the lane has produced.
func (w *activityWriter) Bytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}
