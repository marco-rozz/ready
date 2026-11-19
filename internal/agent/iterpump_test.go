package agent

import (
	"testing"
	"time"

	adk "github.com/cloudwego/eino/adk"
)

// The pump forwards the producer's items in order and ends with ok=false
// after the iterator closes.
func TestPumpIteratorForwardsAndEnds(t *testing.T) {
	iter, gen := adk.NewAsyncIteratorPair[int]()
	events := pumpIterator(iter)

	go func() {
		gen.Send(1)
		gen.Send(2)
		gen.Close()
	}()

	for want := 1; want <= 2; want++ {
		if item := <-events; !item.ok || item.event != want {
			t.Fatalf("item = %+v, want event %d", item, want)
		}
	}

	if item := <-events; item.ok {
		t.Fatalf("item after close = %+v, want end of iteration", item)
	}
}

// After an abandon the pump must keep accepting the producer's sends (the
// background drain) so the executor's goroutine never blocks forever, while
// the consumer has already returned.
func TestPumpIteratorDrainAfterAbandon(t *testing.T) {
	iter, gen := adk.NewAsyncIteratorPair[int]()
	events := pumpIterator(iter)

	go func() {
		gen.Send(1)
		gen.Send(2)
	}()

	if item := <-events; !item.ok || item.event != 1 {
		t.Fatalf("first item = %+v, want event 1", item)
	}

	// Abandon: drain in the background, then stop reading.
	drainIterator(events)

	// The producer must not block on its further sends.
	done := make(chan struct{})
	go func() {
		gen.Send(3)
		gen.Send(4)
		gen.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("producer blocked after the consumer abandoned the pump")
	}
}
