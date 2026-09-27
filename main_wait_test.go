package main

import (
	"context"
	"testing"
	"time"
)

// Regression: a timer bug in the shutdown control flow started the grace
// deadline at BOOT instead of at Ctrl+C, force-exiting every healthy server
// ~10s after launch ('bye (Telegram polling still unwinding; exiting)' on a
// server nobody asked to stop). These tests pin the correct behavior.
//
// A healthy, never-canceled server waits for polling indefinitely — no
// deadline, no exit.
func TestWaitForPollingHealthyServerWaitsForever(t *testing.T) {
	runDone := make(chan struct{})
	ctx := context.Background()

	deadline := time.After(300 * time.Millisecond)
	fired := make(chan struct{})
	go func() {
		waitForPolling(ctx, runDone, 10*time.Millisecond, func() { close(fired) })
		close(runDone) // simulate "still running" until the test ends it
	}()

	select {
	case <-fired:
		t.Fatal("onDeadline fired on a healthy server — the boot-exit regression")
	case <-deadline:
		// Correct: still waiting, no exit.
	}
}

// When ctx is canceled and polling unwinds promptly, waitForPolling returns
// without firing the deadline.
func TestWaitForPollingPromptUnwind(t *testing.T) {
	runDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	go func() {
		time.Sleep(5 * time.Millisecond)
		close(runDone)
	}()

	fired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		waitForPolling(ctx, runDone, 50*time.Millisecond, func() { close(fired) })
		close(done)
	}()

	select {
	case <-fired:
		t.Fatal("onDeadline fired even though polling returned inside the grace window")
	case <-done:
		// Correct.
	case <-time.After(time.Second):
		t.Fatal("waitForPolling did not return after prompt unwind")
	}
}

// When ctx is canceled and polling misses the grace deadline, onDeadline
// fires exactly once and after (roughly) the grace duration.
func TestWaitForPollingDeadlineFiresAfterGrace(t *testing.T) {
	runDone := make(chan struct{}) // polling never returns
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fired := make(chan struct{})
	start := time.Now()
	go func() {
		waitForPolling(ctx, runDone, 40*time.Millisecond, func() { close(fired) })
	}()

	select {
	case <-fired:
		if elapsed := time.Since(start); elapsed < 35*time.Millisecond {
			t.Fatalf("deadline fired after %s, earlier than the grace window", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("onDeadline never fired despite the grace window expiring")
	}
}
