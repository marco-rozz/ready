package steer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
)

func human(s string) channel.Message {
	return channel.Message{Content: s, ChannelName: "cli", Origin: channel.OriginHuman}
}

func harness(s string) channel.Message {
	return channel.Message{Content: s, ChannelName: "cli", Origin: channel.OriginHarness}
}

func isCmd(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "/new") || strings.HasPrefix(strings.TrimSpace(s), "/compact")
}

func spec() TurnSpec {
	return TurnSpec{ID: "t1", Steerable: true, MaxSteers: 8, MaxRestarts: 4}
}

func nextNow(t *testing.T, in *Inbox) Work {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	w, err := in.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	return w
}

func TestAdmit_IdleQueues(t *testing.T) {
	in := New(isCmd)
	if a := in.Admit(human("hi")); a.Disposition != Queued {
		t.Fatalf("idle admit = %s, want queued", a.Disposition)
	}

	if w := nextNow(t, in); w.Msg.Content != "hi" || w.Deferred {
		t.Fatalf("work = %+v", w)
	}
}

func TestAdmit_EligibilityMatrix(t *testing.T) {
	cases := []struct {
		name  string
		spec  func(*TurnSpec)
		msg   channel.Message
		setup func(*Inbox)
		want  Disposition
	}{
		{"human steers", nil, human("fix"), nil, Steer},
		{"unspecified origin queues", nil, channel.Message{Content: "x"}, nil, Queued},
		{"scripted queues", nil, channel.Message{Content: "x", Origin: channel.OriginScripted}, nil, Queued},
		{"harness queues", nil, harness("x"), nil, Queued},
		{"command queues", nil, human("/new"), nil, Queued},
		{"non-steerable turn queues", func(s *TurnSpec) { s.Steerable = false }, human("x"), nil, Queued},
		{"cap reached queues", func(s *TurnSpec) { s.MaxSteers = 1 }, human("b"), func(in *Inbox) { in.Admit(human("a")) }, Queued},
		{"human queued behind closes window", nil, human("b"), func(in *Inbox) { in.Admit(human("/new")) }, Queued},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := New(isCmd)
			sp := spec()

			if tc.spec != nil {
				tc.spec(&sp)
			}

			in.OpenTurn(sp)

			if tc.setup != nil {
				tc.setup(in)
			}

			if got := in.Admit(tc.msg).Disposition; got != tc.want {
				t.Fatalf("disposition = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFIFOBarrier_CommandThenMessage(t *testing.T) {
	in := New(isCmd)
	turn := in.OpenTurn(spec())

	if a := in.Admit(human("/new")); a.Disposition != Queued {
		t.Fatalf("/new = %s", a.Disposition)
	}

	if a := in.Admit(human("turn on the kitchen")); a.Disposition != Queued {
		t.Fatalf("message after /new = %s, want queued", a.Disposition)
	}

	if turn.PendingCount() != 0 {
		t.Fatal("nothing may steer the old Turn")
	}

	turn.CloseTurn()

	if w := nextNow(t, in); w.Msg.Content != "/new" {
		t.Fatalf("first = %q", w.Msg.Content)
	}

	if w := nextNow(t, in); w.Msg.Content != "turn on the kitchen" {
		t.Fatalf("second = %q", w.Msg.Content)
	}
}

func TestFIFOBarrier_HarnessItemDoesNotClose(t *testing.T) {
	in := New(isCmd)
	in.OpenTurn(spec())
	in.Admit(harness("alert"))

	if a := in.Admit(human("correction")); a.Disposition != Steer {
		t.Fatalf("correction = %s, want steer", a.Disposition)
	}
}

func TestOpenTurn_HumanAlreadyQueuedClosesWindow(t *testing.T) {
	in := New(isCmd)
	in.Admit(human("first"))
	in.Admit(human("second"))
	nextNow(t, in)
	in.OpenTurn(spec())

	if a := in.Admit(human("third")); a.Disposition != Queued {
		t.Fatalf("third = %s, want queued (second is queued ahead)", a.Disposition)
	}
}

func TestDeferral_OrderAndPreload(t *testing.T) {
	in := New(isCmd)
	turn := in.OpenTurn(spec())
	in.Admit(human("A"))
	in.Admit(human("B"))

	// A harness item queued after the Turn must run after the deferred opener.
	in.Admit(harness("alert"))
	in.Admit(human("/compact")) // closes nothing relevant: already candidates

	deferred := turn.CloseTurn()
	if len(deferred) != 2 {
		t.Fatalf("deferred = %d, want 2", len(deferred))
	}

	w := nextNow(t, in)
	if w.Msg.Content != "A" || !w.Deferred || len(w.Preload) != 1 || w.Preload[0].Msg.Content != "B" {
		t.Fatalf("opener = %+v", w)
	}

	next := in.OpenTurn(TurnSpec{ID: "t2", Steerable: true, MaxSteers: 8, MaxRestarts: 4, Preload: w.Preload})
	if got := next.Claim(8); len(got) != 1 || got[0].Msg.Content != "B" {
		t.Fatalf("preloaded claim = %+v", got)
	}

	next.CloseTurn()

	if w := nextNow(t, in); w.Msg.Content != "alert" {
		t.Fatalf("after deferred: %q, want alert", w.Msg.Content)
	}

	if w := nextNow(t, in); w.Msg.Content != "/compact" {
		t.Fatalf("then: %q, want /compact", w.Msg.Content)
	}
}

func TestClaim_OrderAndLimits(t *testing.T) {
	in := New(isCmd, WithClock(func() time.Time { return time.Unix(100, 0) }))
	sp := spec()
	sp.MaxSteers = 3
	turn := in.OpenTurn(sp)

	for _, s := range []string{"a", "b", "c", "d"} {
		in.Admit(human(s))
	}

	if turn.PendingCount() != 3 {
		t.Fatalf("pending = %d, want 3 (cap)", turn.PendingCount())
	}

	if got := turn.Claim(0); got != nil {
		t.Fatalf("limit 0 claimed %v", got)
	}

	got := turn.Claim(2)
	if len(got) != 2 || got[0].Msg.Content != "a" || got[1].Msg.Content != "b" {
		t.Fatalf("claim = %+v", got)
	}

	if got[0].ReceivedAt.Unix() != 100 {
		t.Errorf("received_at = %v", got[0].ReceivedAt)
	}

	if rest := turn.Claim(8); len(rest) != 1 || rest[0].Msg.Content != "c" {
		t.Fatalf("rest = %+v", rest)
	}

	// "d" was queued (cap reached), preserved FIFO.
	turn.CloseTurn()

	if w := nextNow(t, in); w.Msg.Content != "d" {
		t.Fatalf("queued = %q", w.Msg.Content)
	}
}

func TestQueueBound(t *testing.T) {
	in := New(isCmd)
	for i := 0; i < QueueBound; i++ {
		if a := in.Admit(human(fmt.Sprint(i))); a.Disposition != Queued {
			t.Fatalf("admit %d = %s", i, a.Disposition)
		}
	}

	if a := in.Admit(human("overflow")); a.Disposition != Dropped || in.Dropped() != 1 {
		t.Fatalf("overflow = %s dropped=%d", a.Disposition, in.Dropped())
	}
}

func TestStop_DisabledByDefault(t *testing.T) {
	in := New(isCmd)

	if a := in.Admit(human("/stop")); a.Disposition != Queued {
		t.Fatalf("/stop without the option = %s, want an ordinary queued message", a.Disposition)
	}
}

func TestStop(t *testing.T) {
	in := New(isCmd, WithStopCommand())

	if a := in.Admit(human(" /stop ")); a.Disposition != Stop || a.Stopping {
		t.Fatalf("idle stop = %+v, want Stop without Stopping", a)
	}

	if in.Len() != 0 {
		t.Fatal("/stop must never be queued")
	}

	turn := in.OpenTurn(spec())
	in.Admit(human("a"))
	in.Admit(human("b"))
	sig := turn.Signal()

	a := in.Admit(human("/stop"))
	if !a.Stopping || len(a.Dropped) != 2 || a.TurnID != "t1" {
		t.Fatalf("stop = %+v", a)
	}

	select {
	case <-sig:
	default:
		t.Fatal("signal must fire on stop")
	}

	if turn.Preempt() != ReasonStop || !turn.Stopping() || turn.PendingCount() != 0 {
		t.Fatal("turn must be stopping with no candidates")
	}

	if got := turn.DroppedByStop(); len(got) != 2 {
		t.Fatalf("dropped by stop = %d", len(got))
	}

	if a := in.Admit(human("late")); a.Disposition != Queued {
		t.Fatalf("after stop = %s, want queued", a.Disposition)
	}

	if d := turn.CloseTurn(); d != nil {
		t.Fatalf("deferred after stop = %v", d)
	}
}

func TestStop_NothingToStopWhileDelivering(t *testing.T) {
	in := New(isCmd, WithStopCommand())
	turn := in.OpenTurn(spec())
	turn.MarkDelivering()

	if a := in.Admit(human("/stop")); a.Disposition != Stop || a.Stopping {
		t.Fatalf("stop while delivering = %+v", a)
	}

	// A candidate during delivery still defers, and is never preemptive.
	if a := in.Admit(human("late")); a.Disposition != Steer {
		t.Fatalf("late = %s", a.Disposition)
	}

	if turn.Preempt() != ReasonNone {
		t.Fatal("delivering turn must not be preempted")
	}

	if d := turn.CloseTurn(); len(d) != 1 {
		t.Fatalf("deferred = %d", len(d))
	}
}

func TestStop_NonHumanIsNotStop(t *testing.T) {
	in := New(isCmd, WithStopCommand())
	in.OpenTurn(spec())

	if a := in.Admit(channel.Message{Content: "/stop", Origin: channel.OriginScripted}); a.Disposition == Stop {
		t.Fatal("scripted /stop must not be handled out of band")
	}
}

func TestSignalAndRestartCap(t *testing.T) {
	in := New(isCmd)
	sp := spec()
	sp.MaxRestarts = 1
	turn := in.OpenTurn(sp)

	select {
	case <-turn.Signal():
		t.Fatal("signal must not fire with nothing pending")
	default:
	}

	in.Admit(human("a"))

	select {
	case <-turn.Signal():
	default:
		t.Fatal("signal must fire on a candidate")
	}

	if turn.Preempt() != ReasonSteer {
		t.Fatal("want steer preemption")
	}

	turn.NoteRestart()
	turn.Claim(8)

	// Restart cap reached: later candidates neither preempt nor wake waiters.
	in.Admit(human("b"))

	if turn.Preempt() != ReasonNone {
		t.Fatal("cap reached: no preemption")
	}

	if turn.Signal() != nil {
		t.Fatal("cap reached: signal must be a never-ready channel")
	}

	if d := turn.CloseTurn(); len(d) != 1 {
		t.Fatalf("deferred = %d, want 1", len(d))
	}
}

func TestSignalRearmsAfterClaim(t *testing.T) {
	in := New(isCmd)
	turn := in.OpenTurn(spec())
	in.Admit(human("a"))
	turn.Claim(8)

	select {
	case <-turn.Signal():
		t.Fatal("signal must re-arm after the claim emptied the candidates")
	default:
	}
}

func TestNext_BlocksUntilAdmitAndHonoursContext(t *testing.T) {
	in := New(isCmd)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if _, err := in.Next(ctx); err == nil {
		t.Fatal("expected ctx error")
	}

	done := make(chan Work, 1)

	go func() {
		w, _ := in.Next(context.Background())
		done <- w
	}()

	time.Sleep(20 * time.Millisecond)
	in.Admit(human("go"))

	select {
	case w := <-done:
		if w.Msg.Content != "go" {
			t.Fatalf("got %q", w.Msg.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("Next did not wake")
	}
}

func TestCloseDrainsThenErrClosed(t *testing.T) {
	in := New(isCmd)
	in.Admit(human("a"))
	in.Close()

	if w := nextNow(t, in); w.Msg.Content != "a" {
		t.Fatalf("drain = %q", w.Msg.Content)
	}

	if _, err := in.Next(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func TestDiscardCounts(t *testing.T) {
	in := New(isCmd)
	in.Admit(human("q1"))
	in.Admit(human("q2"))
	in.OpenTurn(spec())

	q, c := in.Discard()
	if q != 2 || c != 0 {
		t.Fatalf("discard = %d,%d", q, c)
	}
}

func TestContextCarriesTurn(t *testing.T) {
	in := New(isCmd)
	turn := in.OpenTurn(spec())

	if FromContext(context.Background()) != nil {
		t.Fatal("empty context must carry no Turn")
	}

	if FromContext(WithTurn(context.Background(), turn)) != turn {
		t.Fatal("Turn must round-trip")
	}
}

// Every candidate ends exactly once: claimed, deferred, or (never here)
// dropped, while Admit races Claim and CloseTurn.
func TestRace_AdmitVersusClaimAndClose(t *testing.T) {
	for round := 0; round < 200; round++ {
		in := New(isCmd)
		sp := spec()
		sp.MaxSteers = 32
		turn := in.OpenTurn(sp)

		const n = 12 // below QueueBound: this test is about exactly-once, not saturation

		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			steered  = map[string]int{}
			queuedOK = map[string]int{}
		)

		for i := 0; i < n; i++ {
			wg.Add(1)

			go func(i int) {
				defer wg.Done()

				id := fmt.Sprint(i)
				a := in.Admit(human(id))
				mu.Lock()
				defer mu.Unlock()

				switch a.Disposition {
				case Steer:
					steered[id]++
				case Queued:
					queuedOK[id]++
				default:
					t.Errorf("unexpected %s", a.Disposition)
				}
			}(i)
		}

		seen := map[string]int{}
		claimDone := make(chan struct{})

		go func() {
			defer close(claimDone)

			for k := 0; k < 5; k++ {
				for _, c := range turn.Claim(4) {
					mu.Lock()
					seen[c.Msg.Content]++
					mu.Unlock()
				}
			}
		}()

		<-claimDone

		turn.CloseTurn() // deferred candidates surface through Next

		wg.Wait()

		// Admissions after CloseTurn are queued; drain them.
		for in.Len() > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			w, err := in.Next(ctx)
			cancel()

			if err != nil {
				t.Fatalf("drain: %v", err)
			}

			if w.Deferred {
				seen[w.Msg.Content]++

				for _, p := range w.Preload {
					seen[p.Msg.Content]++
				}

				continue
			}

			seen[w.Msg.Content]++
		}

		for i := 0; i < n; i++ {
			id := fmt.Sprint(i)
			if seen[id] != 1 {
				t.Fatalf("round %d: candidate %s outcome count = %d, want exactly 1", round, id, seen[id])
			}
		}
	}
}
