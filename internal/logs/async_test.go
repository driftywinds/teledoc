package logs

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingWriter blocks every Write until released, simulating a wedged
// console.
type blockingWriter struct {
	mu       sync.Mutex
	release  chan struct{}
	onWedge  func() // runs once, right before the first Write blocks
	wedged   bool
	written  strings.Builder
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	first := !b.wedged
	b.wedged = true
	w := &b.written
	b.mu.Unlock()
	if first && b.onWedge != nil {
		b.onWedge()
	}
	<-b.release
	w.Write(p)
	return len(p), nil
}

func (b *blockingWriter) unblock() { close(b.release) }

// The core guarantee: while the output destination is blocked, Write callers
// (request handlers, the sweeper, main) keep returning immediately instead
// of queueing up forever. Overflow lines are dropped; once output recovers
// the backlog flushes.
func TestAsyncWriterNeverStallsOnBlockedOutput(t *testing.T) {
	bw := &blockingWriter{release: make(chan struct{})}
	wedgeDetected := make(chan struct{})
	bw.onWedge = func() { close(wedgeDetected) }

	a := newAsyncWriter(bw)
	defer a.Stop(time.Second)

	// First line goes to the (blocked) writer; the loop goroutine hangs on it.
	a.Write([]byte("first line\n"))
	select {
	case <-wedgeDetected:
	case <-time.After(2 * time.Second):
		t.Fatal("writer never got the first line")
	}

	// While wedged: 3x capacity writes must all return promptly (dropping
	// overflow), never block the caller.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queueCapacity*3; i++ {
			a.Write([]byte("must not stall\n"))
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Write blocked while output was wedged — the incident regression")
	}

	// Unwedge: the first line (and any lines that fit before overflow)
	// flushes through.
	bw.unblock()
	a.Stop(2 * time.Second)
	if !strings.Contains(bw.written.String(), "first line") {
		t.Fatalf("first line lost after unwedging: %q", bw.written.String())
	}
}

// SetupAsync end-to-end: loggers write through the pipeline, plain text when
// the destination is not a terminal (a bytes.Buffer in tests).
func TestSetupAsyncEndToEnd(t *testing.T) {
	var buf bytes.Buffer
	loggers, def, stop := SetupAsync(&buf)
	defer stop()

	loggers["web"].Print("download triggered for document 42")
	def.Print("web UI listening")
	stop() // drain before reading

	out := buf.String()
	if !strings.Contains(out, "web 20") || !strings.Contains(out, "download triggered for document 42") {
		t.Fatalf("origin logger line missing: %q", out)
	}
	if !strings.Contains(out, "app 20") || !strings.Contains(out, "web UI listening") {
		t.Fatalf("default logger line missing: %q", out)
	}
}

// TryWriteLine returns within its timeout even when the destination blocks
// forever — the shutdown-path guarantee.
func TestTryWriteLineTimeout(t *testing.T) {
	bw := &blockingWriter{release: make(chan struct{})}
	defer bw.unblock()

	start := time.Now()
	TryWriteLine(100*time.Millisecond, "bye")
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("TryWriteLine waited %s; must respect its timeout", elapsed)
	}
}
