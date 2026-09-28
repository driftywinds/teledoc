package logs

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// queueCapacity bounds the async pipeline. When output is blocked (a wedged
// console, a stopped pipe) the app keeps RUNNING and logs are dropped past
// this point — a lost log line must never stall a request handler, the
// sweeper, or shutdown.
const queueCapacity = 1024

// asyncWriter decouples log production from console output: writers hand
// lines to a buffered channel, one goroutine performs the actual writes, and
// overflow is dropped with a counter. If the destination wedges, the
// goroutine blocks on the current line while everything else keeps flowing;
// when it unwedges, the backlog flushes and a drop summary is printed.
type asyncWriter struct {
	w       io.Writer
	queued  chan []byte
	dropped atomic.Uint64
	done    chan struct{}
	once    sync.Once
}

// newAsyncWriter starts the writer goroutine.
func newAsyncWriter(w io.Writer) *asyncWriter {
	a := &asyncWriter{w: w, queued: make(chan []byte, queueCapacity), done: make(chan struct{})}
	go a.loop()
	return a
}

func (a *asyncWriter) Write(p []byte) (int, error) {
	// Copy: log.Logger reuses its buffer only under some paths; be safe.
	line := make([]byte, len(p))
	copy(line, p)
	select {
	case a.queued <- line:
		return len(p), nil
	default:
		n := a.dropped.Add(1)
		// Never log synchronously here (that is the whole point); the
		// summary is printed by the loop when output recovers.
		_ = n
		return len(p), nil
	}
}

// writeTimeout bounds how long the loop will wait for a single line to reach
// the destination before giving up on that line and moving to the next one.
//
// Without this, a single write that never returns — a momentary Windows
// console I/O stall, a wedged terminal, anything — parks this goroutine
// forever on that one call. Nothing else in the package ever revisits it:
// the queue keeps accepting lines up to its capacity and then silently drops
// everything past that, and the "dropped N lines" recovery message can never
// print because it is only ever printed by the very call that's stuck. The
// pipeline is then dead — silently — for the rest of the process's life,
// which is indistinguishable from "the app just stopped logging."
const writeTimeout = 2 * time.Second

func (a *asyncWriter) loop() {
	defer close(a.done)
	for line := range a.queued {
		if !a.writeWithTimeout(line) {
			// Treat a stalled write like an overflow drop: it's counted and
			// reported by the next line that actually gets through, instead
			// of silently and permanently disappearing.
			a.dropped.Add(1)
			continue
		}
		if d := a.dropped.Swap(0); d > 0 {
			_, _ = fmt.Fprintf(a.w, "app %s logs: dropped %d line(s) while output was blocked\n", time.Now().Format("2006/01/02 15:04:05"), d)
		}
	}
}

// writeWithTimeout attempts one write, giving up after writeTimeout and
// reporting failure so loop() can move on to the next line. Go cannot cancel
// an in-flight syscall, so a timed-out write's goroutine is abandoned and may
// still land later, out of order with whatever loop() writes next — an
// acceptable trade-off against the alternative (the entire pipeline frozen
// forever on that one call). This mirrors the trade-off TryWriteLine already
// makes for the shutdown-time direct write.
func (a *asyncWriter) writeWithTimeout(line []byte) bool {
	done := make(chan struct{})
	go func() {
		_, _ = a.w.Write(line)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(writeTimeout):
		return false
	}
}

// Stop drains the queue, waiting up to max for the loop to finish.
func (a *asyncWriter) Stop(max time.Duration) {
	a.once.Do(func() { close(a.queued) })
	select {
	case <-a.done:
	case <-time.After(max):
	}
}

// SetupAsync is Setup plus a non-blocking output pipeline and a stop
// function. Use it in main; Setup (synchronous) remains for tests.
func SetupAsync(out io.Writer) (loggers map[string]*log.Logger, defaultLogger *log.Logger, stop func()) {
	a := newAsyncWriter(ForOutput(out))
	loggers = map[string]*log.Logger{}
	for _, origin := range Origins {
		loggers[origin] = newOriginLogger(a, origin)
	}
	defaultLogger = newOriginLogger(a, "app")
	return loggers, defaultLogger, func() { a.Stop(2 * time.Second) }
}

// TryWriteLine writes one line to out directly, bypassing the async
// pipeline, with a hard timeout: shutdown messages must appear even when the
// pipeline is wedged. Safe under contention with the pipeline (a rare
// interleaved line is acceptable at shutdown).
func TryWriteLine(timeout time.Duration, format string, args ...any) {
	done := make(chan struct{})
	go func() {
		line := fmt.Sprintf(format, args...)
		_, _ = fmt.Fprintln(os.Stdout, line)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}