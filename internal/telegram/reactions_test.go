package telegram

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
)

// fakeReactionAPI stands in for the real client inside the queue's send hook:
// it records SetMessageReaction calls and can flood the first floods calls
// with 429s carrying the given retry_after.
type fakeReactionAPI struct {
	mu         sync.Mutex
	calls      []reactionJob
	floods     int   // first floods calls return 429
	retryAfter int   // retry_after seconds reported by those 429s
	attempts   int   // total SetMessageReaction calls made
	other      error // non-flood error returned on every call
}

func (f *fakeReactionAPI) set(ctx context.Context, params *bot.SetMessageReactionParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.other != nil {
		return f.other
	}
	job := reactionJob{
		chatID:    params.ChatID.(int64),
		messageID: params.MessageID,
		emoji:     params.Reaction[0].ReactionTypeEmoji.Emoji,
	}
	if f.floods > 0 {
		f.floods--
		return &bot.TooManyRequestsError{Message: "too many requests, test", RetryAfter: f.retryAfter}
	}
	f.calls = append(f.calls, job)
	return nil
}

func newTestQueue(api *fakeReactionAPI, pace time.Duration) *reactionQueue {
	q := newReactionQueue(nil, log.New(io.Discard, "", 0))
	q.sendFn = api.set
	q.pace = pace
	q.floodBuffer = time.Millisecond
	return q
}

// A healthy queue sends every enqueued reaction, paced, in order.
func TestReactionQueueSendsAll(t *testing.T) {
	api := &fakeReactionAPI{}
	q := newTestQueue(api, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	for i := 1; i <= 5; i++ {
		q.enqueue(-100, i, "👍")
	}
	waitFor(t, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.calls) == 5
	})
	cancel()
	q.Wait()

	api.mu.Lock()
	defer api.mu.Unlock()
	for i, job := range api.calls {
		if job.messageID != i+1 || job.emoji != "👍" || job.chatID != -100 {
			t.Fatalf("call %d: %+v, want chat -100 message %d 👍", i, job, i+1)
		}
	}
}

// Flooded reactions are retried after the demanded retry_after and land on a
// later attempt.
func TestReactionQueueRetriesAfterFlood(t *testing.T) {
	api := &fakeReactionAPI{floods: 2, retryAfter: 1}
	q := newTestQueue(api, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	q.enqueue(-100, 42, "🤔")
	waitFor(t, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.calls) == 1
	})
	cancel()
	q.Wait()

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.calls) != 1 || api.calls[0].emoji != "🤔" || api.calls[0].messageID != 42 {
		t.Fatalf("flooded reaction did not land: %+v", api.calls)
	}
	if api.attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two floods, then success)", api.attempts)
	}
}

// A reaction that stays flooded past maxReactionAttempts is dropped after
// exactly maxReactionAttempts tries (not retried forever).
func TestReactionQueueGivesUpAfterBudget(t *testing.T) {
	api := &fakeReactionAPI{floods: 100, retryAfter: 1}
	q := newTestQueue(api, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	q.enqueue(-100, 7, "👍")
	waitFor(t, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return api.attempts == maxReactionAttempts
	})
	cancel()
	q.Wait()

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.calls) != 0 {
		t.Fatalf("permanently flooded reaction should never land, got %+v", api.calls)
	}
	if api.attempts != maxReactionAttempts {
		t.Fatalf("attempts = %d, want exactly %d", api.attempts, maxReactionAttempts)
	}
}

// Non-flood errors are not retried: one attempt, one log, move on.
func TestReactionQueueNoRetryOnOtherErrors(t *testing.T) {
	api := &fakeReactionAPI{other: errors.New("chat not found")}
	q := newTestQueue(api, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	q.enqueue(-100, 9, "👀")
	waitFor(t, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return api.attempts == 1
	})
	cancel()
	q.Wait()

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (non-flood errors are final)", api.attempts)
	}
}

// A full queue drops the newest reaction instead of blocking the caller.
func TestReactionQueueDropWhenFull(t *testing.T) {
	api := &fakeReactionAPI{}
	q := newTestQueue(api, time.Hour) // worker effectively parked on the first job

	// Fill the buffer completely, then one more must be dropped, not block.
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	deadline := time.After(2 * time.Second)
	for i := 0; i < reactionQueueSize+1; i++ {
		select {
		case q.ch <- reactionJob{chatID: -100, messageID: i, emoji: "👍"}:
		case <-deadline:
			t.Fatal("enqueue blocked on a full queue")
		}
	}
	cancel()
	q.Wait()
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}
