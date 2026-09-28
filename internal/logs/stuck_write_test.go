package logs

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingBlockedWriter blocks its FIRST Write forever (never released) —
// simulating a real stall that never resolves itself in-process, unlike
// blockingWriter in async_test.go, which always eventually calls unblock().
// Later writes succeed immediately, so this shows whether the pipeline
// recovers on its own once one call genuinely never returns.
type recordingBlockedWriter struct {
	stuckOnFirst chan struct{}

	mu        sync.Mutex
	firstSeen bool
	got       strings.Builder
}

func (r *recordingBlockedWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	isFirst := !r.firstSeen
	r.firstSeen = true
	r.mu.Unlock()
	if isFirst {
		// Block ONLY this call, forever — never sync.Once, whose Do blocks
		// *concurrent* callers until the running call returns, which would
		// wrongly freeze every later write too instead of just the first.
		close(r.stuckOnFirst)
		select {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got.Write(p)
	return len(p), nil
}

func (r *recordingBlockedWriter) Snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got.String()
}

// The incident this guards against: a healthy, running app whose console
// output just stops forever — no shutdown lines, no "dropped N lines"
// recovery message, nothing — even though nothing else in the process is
// wedged. Root cause: loop() had no timeout on its write to the destination,
// so a single call that never returns (a momentary Windows console I/O
// stall, a wedged terminal, anything) permanently parks the one goroutine
// draining the queue. Every log line after that point is silently lost:
// first buffered, then dropped once the queue fills, with the drop-summary
// itself unable to print because only the (permanently stuck) call prints it.
func TestPipelineRecoversFromOneStuckWrite(t *testing.T) {
	w := &recordingBlockedWriter{stuckOnFirst: make(chan struct{})}
	a := newAsyncWriter(w)
	defer a.Stop(200 * time.Millisecond)

	a.Write([]byte("line one\n"))
	<-w.stuckOnFirst // confirmed loop() is now stuck inside this Write

	a.Write([]byte("shutting down\n"))
	a.Write([]byte("web server stopped\n"))

	deadline := time.Now().Add(3 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = w.Snapshot()
		if strings.Contains(got, "web server stopped") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(got, "shutting down") || !strings.Contains(got, "web server stopped") {
		t.Fatalf("pipeline did not recover after one write stalled forever; got %q", got)
	}
}