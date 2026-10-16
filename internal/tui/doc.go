// Package tui implements the `ready chat` interactive terminal UI on top of
// Bubble Tea (charm.land/bubbletea/v2).
//
// Layout is three regions: a scrollable transcript viewport (fills the whole
// remaining height — the frame spans 100% of the terminal, Pi Agent layout,
// change `tui-scrollback-transcript` / the architecture decision records), a multiline input editor
// with a `READY. ❯❯ ` lipgloss prefix (middle, dynamic height), and a one-
// line persistent status line (bottom). The program runs in the ALTERNATE
// SCREEN (banner-in-transcript change): the frame owns 100% of the terminal
// for the whole session and on exit (quit/ctrl+d/SIGINT) the pre-session
// main screen is restored byte-identical, the stdout banner (cmd/chat.go
// prints it before entering the alt screen) included. Mouse capture is
// PERMANENT (Amendment 3, mouse ownership): the client never hands the
// mouse back to the terminal — the wheel always scrolls the internal
// viewport, and selection (click → drag → reverse-video highlight over the
// transcript) and copy (release → rendered text to the system clipboard via
// OSC52 / background pbcopy) are TUI-managed in every turn state.
//
// Input while a turn is in flight is steering-queued: RETURN during a turn
// stores the prompt in a local FIFO (`pending`), rendered as a pending echo
// block, sent automatically when the in-flight turn completes — one reply
// timeout per sent turn, armed at send and re-armed by every progress chunk
// of the in-flight turn, so the effective deadline is time since the last
// progress, not since send (a turn streaming progress is never abandoned; a
// turn producing nothing still exits 124). Each timer is tagged with the
// turn's sequence, so a stale timer from an older turn is inert against a
// newer one still in flight — Bubble Tea commands cannot be cancelled, and
// re-arming only ever adds more timers: each arm also carries an arm
// sequence, so only the latest arm of the current turn may end the turn and
// every superseded timer is inert.
// The daemon never sees concurrent message requests (its
// request_in_flight rejection is handled as inert).
//
// The head block leading the viewport is static styled text — the glyph
// banner (glyph mode only) plus the session-facts info block (daemon
// address, session ID, client version). The model reference is NOT in it:
// the persistent status line is its only home, always visible once the
// handshake carries it (coloured by state, unstyled-faint when the state is
// not yet reported).
//
// The transcript is progressively re-rendered
// from accumulated `progress` chunks on a 50 ms coalesced tick; the final
// `reply` is the authoritative flush that replaces only the current turn's
// live preview. Completed turns live in the raw-markdown session record
// (`completed`) behind a render cache (`completedRenderd`/`completedDirty`)
// invalidated on finalise, resize and background change: any resize re-renders
// the whole visible transcript at the new width (markdown recalculated),
// exactly once, inside the app's managed area.
//
// The ZMQ DEALER socket is fed into the TUI via a blocking tea.Cmd over
// dealer.Recv(); exactly one such Cmd is in flight at a time (preserving ZMQ
// arrival order) and it is re-issued after every handled message. The TUI is
// the exclusive consumer of dealer.Recv() after the synchronous pre-TUI auth
// handshake — the receive-loop goroutine is never started on this path. On
// tea.Quit the dealer is closed, unblocking the in-flight recv.
//
// The TUI never calls os.Exit; it exposes an exitReason that the chat
// dispatcher (cmd/chat.go) maps to the documented exit code after
// Program.Run returns.
package tui
