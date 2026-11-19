package agent

import (
	adk "github.com/cloudwego/eino/adk"
)

// iteratorItem is one element of a pumped ADK AsyncIterator plus its
// end-of-iteration flag.
type iteratorItem[T any] struct {
	event T
	ok    bool
}

// pumpIterator consumes an ADK AsyncIterator on a dedicated goroutine and
// forwards its elements to an unbuffered channel.
//
// Why the pump exists: AsyncIterator.Next() parks in an unbounded-channel
// receive with no context path (adk.AsyncIterator →
// internal.UnboundedChan.Receive returns only on an element or a channel
// close), so a turn executor that stops producing events blocks the caller
// forever — past the attempt budget, the outer deadline, every cancellation.
// The pump decouples the caller from that wait: the caller selects between
// the pump's items and its own ctx.Done.
//
// After an abandon the caller MUST drain the returned channel in the
// background (drainIterator): the pump's pending send needs a receiver, and
// draining is side-effect free because the caller's event handler no longer
// runs — nothing is recorded, tapped, or forwarded after the turn returned.
func pumpIterator[T any](iter *adk.AsyncIterator[T]) <-chan iteratorItem[T] {
	items := make(chan iteratorItem[T])
	go func() {
		defer close(items)
		for {
			event, ok := iter.Next()
			items <- iteratorItem[T]{event: event, ok: ok}
			if !ok {
				return
			}
		}
	}()

	return items
}

// drainIterator discards the remaining items of an abandoned pump in the
// background, so the executor can finish (or stay wedged) without ever
// blocking on its pending sends.
func drainIterator[T any](items <-chan iteratorItem[T]) {
	go func() {
		for range items {
		}
	}()
}
