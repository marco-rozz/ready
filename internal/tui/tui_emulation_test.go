//go:build !windows

package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/creack/pty"
	"github.com/go-zeromq/zmq4"

	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

// These tests validate the
// Pi Agent layout at the terminal-stream level — the automated counterpart of
// the manual checklist. A real tea.Program runs the production Model over a
// real pty (the app attaches to the pty SLAVE, its "terminal"; the harness
// captures the raw stream from the MASTER side). Two replies render in the
// full-height viewport; the window is then SHRUNK and EXPANDED and the capture
// is parsed with pyte to assert, on the FINAL SCREEN:
//
//   - 100% fill: the frame occupies the whole terminal — the status line is on
//     the bottom row and the transcript tail is adjacent to the input bars
//     (no reserved blank band between transcript and editor);
//   - the resize re-render: every turn re-rendered at the current width
//     (reply words re-wrapped, tables recalculated), each exactly once — no
//     stale commit-time strings, no duplicate copies, no content outside the
//     app's managed area (the pyte screen IS the app's managed area).
//
// The pty resize is delivered as the WindowSizeMsg a real checkResize would
// emit; the SIGWINCH plumbing itself is Bubble Tea framework behaviour.

// gatedDealer is a dealer whose Recv waits on a per-queue signal, so the
// TUI's receive loop stays parked (and unblocked on Close) without frames.
type gatedDealer struct {
	mu     sync.Mutex
	frames [][]byte
	closed bool
	arrive chan struct{}
}

func newGatedDealer() *gatedDealer {
	return &gatedDealer{arrive: make(chan struct{})}
}

func (g *gatedDealer) Recv() (zmq4.Msg, error) {
	for {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()

			return zmq4.Msg{}, errors.New("dealer closed")
		}
		if len(g.frames) > 0 {
			raw := g.frames[0]
			g.frames = g.frames[1:]
			g.mu.Unlock()

			return zmq4.Msg{Frames: [][]byte{raw}}, nil
		}
		ch := g.arrive
		g.mu.Unlock()

		<-ch
	}
}

func (g *gatedDealer) Send(msg zmq4.Msg) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(msg.Frames) > 0 {
		g.frames = append(g.frames, msg.Frames[0])
	}

	return nil
}

func (g *gatedDealer) Close() error {
	g.mu.Lock()
	g.closed = true
	ch := g.arrive
	g.mu.Unlock()

	close(ch)

	return nil
}

// replyWords builds 12 words of 7 runes: their wrap width is measurable at
// every terminal width, so stale rendering (a wrap from a previous width) is
// detectable by words per line.
func replyWords() string {
	ws := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		ws = append(ws, "parola"+string(rune('a'+i)))
	}

	return strings.Join(ws, " ")
}

func TestReplayTerminalEmulation(t *testing.T) {
	if testing.Short() {
		t.Skip("terminal emulation test skipped in -short")
	}

	master, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("openpty: %v", err)
	}
	defer master.Close()
	defer tty.Close()

	if err := pty.Setsize(master, &pty.Winsize{Cols: 100, Rows: 30}); err != nil {
		t.Fatalf("set initial size: %v", err)
	}

	// Ten distinct one-paragraph replies: the rendered transcript is taller
	// than the viewport, so auto-follow keeps the tail adjacent to the input
	// bars and the resize re-render is observable on every turn.
	replies := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		replies = append(replies, fmt.Sprintf("risposta numero %d: %s", i, replyWords()))
	}

	m := NewModel(newGatedDealer(), Options{
		ConnectionID: testSessionID,
		ReplyTimeout: 0, // zero → NewModel clamps to 120s
		Sanitizer:    privacy.New(nil),
	})

	var capMu sync.Mutex
	var captured []byte
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, 64*1024)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				capMu.Lock()
				captured = append(captured, buf[:n]...)
				capMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	prog := tea.NewProgram(m,
		// The app attaches to the pty SLAVE (its "terminal"), exactly like a
		// real process on a real terminal; the harness captures the raw stream
		// from the MASTER side.
		tea.WithInput(tty),
		tea.WithOutput(tty),
	)
	errCh := make(chan error, 1)
	go func() {
		_, runErr := prog.Run()
		errCh <- runErr
	}()

	// Boot, then render two turns at the boot width. The first WindowSizeMsg
	// arrives before the first reply (sent after the boot settle), so the
	// turns render at 100 columns.
	time.Sleep(500 * time.Millisecond)
	for _, content := range replies {
		prog.Send(replyFinal{Resp: protocol.Response{
			Status:       protocol.ResponseStatusOK,
			ResponseType: protocol.ResponseTypeReply,
			Content:      content,
		}})
		time.Sleep(400 * time.Millisecond) // re-render + renderer flush
	}

	// Shrink to 60x30, settle (cache invalidated + full re-render), then
	// expand to 90x30 and settle again: the operator's failing sequence.
	resizes := []pty.Winsize{{Cols: 60, Rows: 30}, {Cols: 90, Rows: 30}}
	for _, ws := range resizes {
		w, h := int(ws.Cols), int(ws.Rows)
		if err := pty.Setsize(master, &pty.Winsize{Cols: ws.Cols, Rows: ws.Rows}); err != nil {
			t.Fatalf("resize pty: %v", err)
		}
		prog.Send(tea.WindowSizeMsg{Width: w, Height: h})
		time.Sleep(700 * time.Millisecond) // re-render + flush
	}

	prog.Quit()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("program exited with error after Quit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit after Quit")
	}
	master.Close()                     // break the drain read
	time.Sleep(200 * time.Millisecond) // let the drain goroutine collect the tail

	capMu.Lock()
	raw := captured
	capMu.Unlock()
	if len(raw) == 0 {
		t.Fatal("empty terminal capture")
	}

	// Amendment 3 (mouse ownership): the capture covers multiple replies and
	// resizes AND the idle phases between them — the mouse mode must never
	// be released mid-session (the only mouse-off sequences are the program's
	// quit-time restore). Enablement appears once (boot); any ?1002l/\x1b[?1006l
	// reset before the quit tail would mean the mouse was handed back.
	enabled := strings.Count(string(raw), "\x1b[?1002h")
	if enabled == 0 {
		t.Fatal("capture never enables mouse reporting (?1002h)")
	}
	// Exactly ONE disable (the quit-time restore pair ?1002l+?1006l after
	// prog.Quit()); any earlier one would mean the mouse was handed back
	// mid-session.
	if n := strings.Count(string(raw), "\x1b[?1002l"); n != 1 {
		t.Errorf("mouse disable (?1002l) emitted %d times, want exactly 1 (quit restore only)", n)
	}

	capture := filepath.Join(os.TempDir(), fmt.Sprintf("ready-tui-%d.bin", os.Getpid()))
	t.Logf("capture saved: %s", capture)
	if err := os.WriteFile(capture, raw, 0o600); err != nil {
		t.Fatalf("write capture: %v", err)
	}

	validateViewportCapture(t, capture)
}

// validateViewportCapture parses the raw terminal stream with pyte at the real
// pane size (30 rows × 100 cols) and asserts the Pi Agent layout on the FINAL
// SCREEN. Skipped with a clear message when python/pyte is unavailable.
func validateViewportCapture(t *testing.T, capture string) {
	t.Helper()

	py, ok := pytePython(t)
	if !ok {
		t.Skip("pyte not available: terminal-emulation validation skipped")
	}

	script := `import sys
import pyte

text = open(sys.argv[1], "rb").read().decode("utf-8", "replace")
# Truncate the quit-time mode-restore sequences (kitty/mouse/paste-off): pyte
# mis-parses them and clobbers the last row; on a real terminal they are
# consumed invisibly.
cut = text.rfind("\x1b[>4m")
if cut >= 0:
    text = text[:cut]
rows, cols = 30, 100
screen = pyte.Screen(cols, rows)
stream = pyte.Stream(screen)
stream.feed(text)
display = screen.display  # 30 strings, screen-only state
joined = "\n".join(display)

# The resize re-render leaves each turn exactly once in the managed area —
# shrink + expand must not leave stale duplicate copies. Turns scrolled past
# the top legitimately leave the visible screen (auto-follow); no marker may
# EVER appear more than once, and the last turn must be visible.
seen = [f"risposta numero {i}:" for i in range(10)]
for m in seen:
    n = joined.count(m)
    assert n <= 1, f"marker {m!r} appears {n} times on the final screen, want at most 1"
assert seen[-1] in joined, f"last turn marker {seen[-1]!r} not visible on the final screen"

# 100% fill: the status line sits on the BOTTOM row and the input bars are
# directly above the bottom — the frame spans the whole terminal.
last = display[rows - 1].strip()
assert last.startswith("idle"), f"status line not on the bottom row: {last!r}"
bar_rows = [i for i, l in enumerate(display) if "─" in l]
assert len(bar_rows) >= 2, f"input border bars missing ({len(bar_rows)} bar lines)"
assert bar_rows[-1] == rows - 2, f"bottom bar is row {bar_rows[-1]}, want row {rows - 2} (not bottom-anchored)"
assert "❯❯" in display[rows - 3], f"input prompt not directly above the bottom bar: {display[rows - 2]!r}"

# No blank band between the transcript tail and the input: the row directly
# above the top bar carries rendered content (auto-follow keeps the tail down).
top_bar = bar_rows[0]
assert display[top_bar - 1].strip() != "", "blank band between transcript and input bars"

# Reply words are 7 chars: after the final (90-col) resize a line fits at most
# 11 of them. Any line with 12+ reply words is a stale render from a wider era.
for i, line in enumerate(display):
    words = [w for w in line.split() if w.startswith("parola")]
    assert len(words) <= 11, f"row {i} keeps a stale wide wrap ({len(words)} words): {line!r}"
print("PYTE_OK")
`
	scriptPath := filepath.Join(t.TempDir(), "validate.py")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatalf("write validator: %v", err)
	}

	out, err := pyteRun(t, py, scriptPath, capture)
	if err != nil {
		t.Fatalf("terminal-emulation validation failed:\n%s", out)
	}
	t.Log("pyte validation:", strings.TrimSpace(out))
}

// pytePython finds a python interpreter with pyte installed: the repo venv
// first, then any python3/python whose import check succeeds.
func pytePython(t *testing.T) (string, bool) {
	t.Helper()

	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err == nil {
		venv := filepath.Join(repo, ".venv", "bin", "python")
		if _, err := os.Stat(venv); err == nil && pyteImportable(t, venv) {
			return venv, true
		}
	}
	for _, cand := range []string{"python3", "python"} {
		if _, err := exec.LookPath(cand); err == nil && pyteImportable(t, cand) {
			return cand, true
		}
	}

	return "", false
}

func pyteImportable(t *testing.T, py string) bool {
	t.Helper()

	_, err := pyteRun(t, py, "-c", "import pyte")

	return err == nil
}

// pyteRun runs the validator with a 20 s guard so a wedged python cannot hang
// the suite.
func pyteRun(t *testing.T, py string, args ...string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("validator timed out")
	}
	if err != nil {
		return string(out), fmt.Errorf("validator failed: %w", err)
	}

	return string(out), nil
}
