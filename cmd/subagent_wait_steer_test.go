package main

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/steer"
	"github.com/marco-rozz/ready/internal/subagent"
)

// subagent_wait ends early on a steering candidate or a stop; pending results
// stay pending and the Turn context is never cancelled.
func TestSubagentWait_InterruptedBySteeringSignal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		want    string
	}{
		{"candidate", "no, the other one", steer.SubagentWaitInterruptedSteer},
		{"stop", stopCommand, steer.SubagentWaitInterruptedStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newLifecycleEnv(t)
			rt := env.runtime()

			ctx := context.Background()
			cur, _ := rt.sessions.History(ctx, session.Address("cli"))

			sr := subagent.New(subagent.Deps{Enabled: true, Logger: zerolog.Nop(), WaitTimeout: time.Minute}, subagent.NewDefinitions(nil))
			wt := newSubagentWaitTool(sr, rt, zerolog.Nop())

			in := steer.New(nil, steer.WithStopCommand())
			turn := in.OpenTurn(steer.TurnSpec{ID: "t1", Steerable: true, MaxSteers: 8, MaxRestarts: 4})

			toolCtx := context.WithValue(ctx, toolSessionKeyCtxKey{}, cur.ID)
			toolCtx = context.WithValue(toolCtx, subagentRuntimeCtxKey{}, rt)
			toolCtx = steer.WithTurn(toolCtx, turn)

			go func() {
				time.Sleep(50 * time.Millisecond)
				in.Admit(channel.Message{Content: tc.message, Origin: channel.OriginHuman})
			}()

			start := time.Now()

			got, err := wt.InvokableRun(toolCtx, `{"targets":[],"timeout":120}`)
			if err != nil {
				t.Fatalf("InvokableRun: %v", err)
			}

			if got != tc.want {
				t.Fatalf("result = %q, want %q", got, tc.want)
			}

			if time.Since(start) > 3*time.Second {
				t.Fatal("wait was not interrupted promptly")
			}

			if toolCtx.Err() != nil {
				t.Fatal("the Turn context must not be cancelled")
			}
		})
	}
}
