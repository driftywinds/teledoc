package telegram

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Reactions double Telegram's flood control: a mass import (re-uploading a
// folder of files to backfill the archive) ingests documents about 1/second
// — safely inside the message-ingestion quotas — but each one also sets a
// reaction, and reaction calls have a much smaller per-chat quota. The
// incident: a ~80-file import exhausted the quota, every SetMessageReaction
// returned 429 with a retry_after of 38s counting down, and every status
// reaction (👍 / 👀 / 🤔) was lost — including 🤔, the "tagging failed,
// needs attention" signal.
//
// The fix is a paced background queue: ingestion only enqueues (never
// blocks, never stalls archiving), one worker sends reactions ~1 per second
// — matching the observed ~1/s quota refill — and on 429 it waits the
// demanded retry_after and retries that reaction up to maxReactionAttempts
// times before giving up with a single clear log line. The worker stops with
// the bot's context; enqueues after that are dropped silently.
type reactionQueue struct {
	api  *bot.Bot
	log  *log.Logger
	pace time.Duration // minimum interval between reaction calls
	ch   chan reactionJob
	done sync.WaitGroup

	// floodBuffer is added on top of a 429's retry_after before retrying, to
	// absorb client/server clock skew. Production sets retryAfterBuffer;
	// tests shrink it to keep them fast.
	floodBuffer time.Duration

	// sendFn is the actual send hook; production leaves it nil so send uses
	// the api client. Tests inject a fake here.
	sendFn func(ctx context.Context, params *bot.SetMessageReactionParams) error
}

type reactionJob struct {
	chatID    int64
	messageID int
	emoji     string
}

const (
	reactionQueueSize   = 512 // bursts up to this size are fully buffered
	maxReactionAttempts = 3   // per reaction, honoring each 429's retry_after
	retryAfterBuffer    = 1500 * time.Millisecond
	reactionPace        = 1100 * time.Millisecond
)

func newReactionQueue(api *bot.Bot, logger *log.Logger) *reactionQueue {
	return &reactionQueue{
		api:         api,
		log:         logger,
		pace:        reactionPace,
		floodBuffer: retryAfterBuffer,
		ch:          make(chan reactionJob, reactionQueueSize),
	}
}

// Start launches the worker; call once after the api client is connected.
// The worker exits when ctx is canceled (bot shutdown drains nothing: at
// that point the process is dying anyway and reactions are status marks,
// not data).
func (q *reactionQueue) Start(ctx context.Context) {
	q.done.Add(1)
	go func() {
		defer q.done.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-q.ch:
				q.process(ctx, job)
			}
		}
	}()
}

// Wait blocks until the worker has exited (after ctx cancellation). Used by
// tests to avoid racing on fake API state.
func (q *reactionQueue) Wait() { q.done.Wait() }

// enqueue adds a reaction to the background queue. It never blocks: a full
// queue drops the reaction with one log line — at 512 buffered reactions
// that means Telegram has been unreachable/flooded for minutes, and a
// dropped status mark beats stalling the ingestion pipeline.
func (q *reactionQueue) enqueue(chatID int64, messageID int, emoji string) {
	select {
	case q.ch <- reactionJob{chatID: chatID, messageID: messageID, emoji: emoji}:
	default:
		q.log.Printf("telegram: reaction queue full; dropping %q on message %d in chat %d", emoji, messageID, chatID)
	}
}

// process sends one reaction with pacing and 429-aware retries. Pacing
// happens BEFORE the call (not after) so a queue that arrived empty doesn't
// delay its first reaction.
func (q *reactionQueue) process(ctx context.Context, job reactionJob) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(q.pace):
	}
	for attempt := 1; ; attempt++ {
		err := q.send(ctx, job)
		if err == nil {
			return
		}
		var flood *bot.TooManyRequestsError
		if !errors.As(err, &flood) || attempt >= maxReactionAttempts {
			q.log.Printf("telegram: reaction %q on message %d in chat %d failed: %v",
				job.emoji, job.messageID, job.chatID, err)
			return
		}
		// Flooded: wait exactly what Telegram demanded (plus a small buffer
		// for client/server clock skew), then try again.
		wait := time.Duration(flood.RetryAfter)*time.Second + q.floodBuffer
		q.log.Printf("telegram: reaction flood control on message %d in chat %d; retrying in %s (attempt %d/%d)",
			job.messageID, job.chatID, wait.Truncate(time.Second), attempt, maxReactionAttempts)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// send performs the actual SetMessageReaction call.
func (q *reactionQueue) send(ctx context.Context, job reactionJob) error {
	params := &bot.SetMessageReactionParams{
		ChatID:    job.chatID,
		MessageID: job.messageID,
		Reaction: []models.ReactionType{{
			Type:              models.ReactionTypeTypeEmoji,
			ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: job.emoji},
		}},
	}
	if q.sendFn != nil {
		return q.sendFn(ctx, params)
	}
	_, err := q.api.SetMessageReaction(ctx, params)
	return err
}
