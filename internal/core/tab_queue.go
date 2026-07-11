package core

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/cc-hchi/browser-control/internal/protocol"
)

// tabQueue is a cancellable FIFO executor. Requests for different tabs can run
// concurrently, while every request for one claimed tab observes a single,
// deterministic order. cancelAll interrupts the active request and removes all
// requests that were queued before the cancellation generation.
type tabQueue struct {
	mu           sync.Mutex
	running      bool
	generation   uint64
	activeCancel context.CancelFunc
	waiters      []*tabWaiter
}

type tabWaiter struct {
	ready      chan struct{}
	generation uint64
	granted    bool
	err        *protocol.RPCError
}

func (q *tabQueue) run(ctx context.Context, fn func(context.Context) (jsonResult, *protocol.RPCError)) (jsonResult, *protocol.RPCError) {
	waiter := &tabWaiter{ready: make(chan struct{})}
	q.mu.Lock()
	waiter.generation = q.generation
	immediate := !q.running
	if immediate {
		q.running = true
		waiter.granted = true
	} else {
		q.waiters = append(q.waiters, waiter)
	}
	q.mu.Unlock()

	if !immediate {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			if !waiter.granted {
				q.removeWaiterLocked(waiter)
				q.mu.Unlock()
				return nil, cancelledError(ctx)
			}
			q.mu.Unlock()
			// The slot was granted at the same time the caller cancelled.
			// Release it without dispatching browser work.
			q.release()
			return nil, cancelledError(ctx)
		case <-waiter.ready:
			if waiter.err != nil {
				return nil, waiter.err
			}
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	q.mu.Lock()
	if waiter.generation != q.generation {
		q.mu.Unlock()
		cancel()
		q.release()
		return nil, protocol.NewError(protocol.CodeCancelled, "CANCELLED", "tab control was revoked before the request ran", true, nil)
	}
	q.activeCancel = cancel
	q.mu.Unlock()

	result, rpcErr := fn(runCtx)
	cancel()
	q.release()
	return result, rpcErr
}

func (q *tabQueue) cancelAll(reason string) {
	q.mu.Lock()
	q.generation++
	if q.activeCancel != nil {
		q.activeCancel()
	}
	for _, waiter := range q.waiters {
		waiter.err = protocol.NewError(protocol.CodeCancelled, "CANCELLED", "tab control was revoked", true, map[string]any{"reason": reason})
		close(waiter.ready)
	}
	q.waiters = nil
	q.mu.Unlock()
}

func (q *tabQueue) release() {
	q.mu.Lock()
	q.activeCancel = nil
	for len(q.waiters) > 0 {
		waiter := q.waiters[0]
		q.waiters = q.waiters[1:]
		if waiter.err != nil {
			continue
		}
		waiter.granted = true
		close(waiter.ready)
		q.mu.Unlock()
		return
	}
	q.running = false
	q.mu.Unlock()
}

func (q *tabQueue) removeWaiterLocked(target *tabWaiter) {
	for index, waiter := range q.waiters {
		if waiter == target {
			copy(q.waiters[index:], q.waiters[index+1:])
			q.waiters[len(q.waiters)-1] = nil
			q.waiters = q.waiters[:len(q.waiters)-1]
			return
		}
	}
}

func cancelledError(ctx context.Context) *protocol.RPCError {
	message := "request cancelled"
	if ctx.Err() != nil {
		message = ctx.Err().Error()
	}
	return protocol.NewError(protocol.CodeCancelled, "CANCELLED", message, true, nil)
}

// jsonResult is kept as an alias so the queue is independent from browser
// response shapes while avoiding a second generic queue implementation.
type jsonResult = json.RawMessage
