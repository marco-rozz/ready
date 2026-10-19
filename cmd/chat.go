package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/google/uuid"
	"github.com/marco-rozz/ready/internal/mdrender"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/tui"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// Exit codes for the `chat` subcommand. The numeric values are part of the
// public CLI contract. Values follow common shell conventions:
//
//   - ExitOK       (0)   — success
//   - ExitErr      (1)   — generic error, unspecified
//   - ExitBusy     (2)   — daemon refused: another session already attached
//   - ExitAuthFail (3)   — HMAC authentication failed
//   - ExitTimeout  (124) — `timeout(1)` convention for deadline reached
//   - ExitSIGINT   (130) — 128 + SIGINT(2), shell convention for Ctrl+C
const (
	ExitOK       = 0
	ExitErr      = 1
	ExitBusy     = 2
	ExitAuthFail = 3
	ExitTimeout  = 124
	ExitSIGINT   = 130
)

const chatLabel = "chat"

const challenge = "challenge"

// envAddress is the env-var fallback for --address.
const envAddress = "READY_ADDR"

// envToken is the env-var fallback for --token.
const envToken = "READY_TOKEN"

// defaultAddress is used when neither flag nor env var supplies an address.
const defaultAddress = "tcp://127.0.0.1:7600"

// envLogo is the env-var selector for the interactive TUI's logo
// presentation (chat logo modes): auto (default, also on unset
// or unknown) | png (degrades to glyph) | glyph | off. Env-var only —
// `chat` never reads config.yaml.
const envLogo = "READY_LOGO"

// logoMode is the resolved READY logo presentation for one chat client run.
type logoMode int

const (
	// logoModeGlyph prints the glyph art on the main screen before the
	// alt-screen program starts (canonical for terminals without inline
	// images, and the degradation target for `png` when the PNG emit fails).
	// The TUI also renders the same glyph as the head of the scrollable
	// transcript.
	logoModeGlyph logoMode = iota
	// logoModePNG prints the inline PNG on the main screen before the
	// alt-screen program starts; it reappears on exit with the restored
	// main screen. The in-TUI presentation is the same glyph head as glyph
	// mode (inline images must not enter the frame).
	logoModePNG
	// logoModeOff suppresses the logo everywhere; the TUI leads with the
	// info block alone.
	logoModeOff
)

// parseLogoMode maps a READY_LOGO value to the configured mode.
// Unset, empty and unknown values fall back to auto (lenient, like the
// shift-enter resolver in internal/tui). auto is the caller's default
// resolution and never returned as a final mode by resolveLogoMode.
type rawLogoMode int

const (
	rawLogoAuto rawLogoMode = iota
	rawLogoPNG
	rawLogoGlyph
	rawLogoOff
)

func parseLogoMode(raw string) rawLogoMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "png":
		return rawLogoPNG
	case "glyph":
		return rawLogoGlyph
	case "off":
		return rawLogoOff
	default: // "", "auto", unknown values
		return rawLogoAuto
	}
}

// resolveLogoMode folds terminal capability into the configured mode:
// auto → png when the terminal advertises inline images (kitty/iTerm2),
// glyph otherwise; png degrades to glyph on a non-capable terminal;
// glyph and off pass through. imageProto is the detected image protocol
// ("" = not capable), so the resolver is fully injectable in tests.
func resolveLogoMode(raw, imageProto string) logoMode {
	switch parseLogoMode(raw) {
	case rawLogoGlyph:
		return logoModeGlyph

	case rawLogoOff:
		return logoModeOff
	default: // auto (unset/unknown) and png
		if imageProto != "" {
			return logoModePNG
		}

		return logoModeGlyph
	}
}

// defaultReplyTimeout is the per-reply deadline: how long the client waits for
// the agent to respond to a single message before giving up. It must stay at
// or above the daemon's zmqcli.reply_timeout (config.yaml), or the client
// gives up and sends session_end before a legitimately slow reply (e.g. a
// cloud failover model) arrives — the daemon then drops that reply with "no
// pending entry for reply — dropping" since the session is already gone.
// 900s matches the daemon's self-correction floor in cmd/serve.go
// (agentRunTimeout 300s + formatterTimeout 60s + margin), which is itself the
// value operators are expected to raise reply_timeout to for slow providers.
// It intentionally does NOT cap the total interactive session length — the
// user can keep chatting for as long as they like.
const defaultReplyTimeout = 900 * time.Second

// newChatCommand declares the `chat` subcommand. The Action implements the
// two-round HMAC challenge-response handshake described in the design doc.
//
// chat MUST NOT initialise the logging subsystem and MUST NOT create or open
// any file under the runtime home or the Install home. All errors are a single human-readable line on stderr.
func newChatCommand(stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:  chatLabel,
		Usage: "Connect a CLI client to a running ready daemon",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "address",
				Usage:   "ZMQ endpoint of the daemon (e.g. tcp://127.0.0.1:7600)",
				Sources: cli.EnvVars(envAddress),
				Value:   defaultAddress,
			},
			&cli.StringFlag{
				Name:    "token",
				Usage:   "HMAC shared secret used to authenticate with the daemon",
				Sources: cli.EnvVars(envToken),
			},
			&cli.StringFlag{
				Name:  "token-file",
				Usage: "HMAC shared secret read from a file (lower precedence than --token and READY_TOKEN)",
			},
			&cli.StringFlag{
				Name:  "exec",
				Usage: "send a single message non-interactively and exit",
			},
			&cli.DurationFlag{
				Name:  "reply-timeout",
				Usage: "max wait time for each agent reply; does not cap the total session length",
				Value: defaultReplyTimeout,
			},
			&cli.BoolFlag{
				Name:  "raw",
				Usage: "print each reply without terminal formatting (no glamour, no live preview); redaction is still applied",
				Value: false,
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			return runChat(ctx, c, os.Stdout, stderr, os.Stdin)
		},
	}
}

// errSigInt is returned by waitReply/waitControl/recvDirect when SIGINT fires.
var errSigInt = errors.New("sigint")

// errControlAfter is returned by waitReply when a post-handshake control
// message arrives instead of a reply; the caller inspects the returned
// protocol.Response to map it to an exit code.
var errControlAfter = errors.New("post-handshake control")

// runChat is the testable body of the chat Action. It is separated from
// newChatCommand so tests can inject custom stdout/stderr/stdin without
// setting process globals.
//
// The client uses a DEALER socket for fully asynchronous bidirectional
// communication with the daemon's ROUTER socket. The receive surface is split
// per the design section with exclusive ownership:
//   - TUI path (stdin+stdout both TTYs, no --raw, not --exec): the synchronous
//     auth handshake uses direct dealer.Recv() calls; the TUI then owns
//     dealer.Recv() exclusively via a re-issued tea.Cmd. recvLoop is NEVER
//     started on this path.
//   - non-TUI paths (--exec, non-TTY, --raw): the synchronous recvLoop
//     goroutine reads all server messages; progress is discarded (D16);
//     post-handshake control messages are consumed in waitReply and the
//     interactive idle select.
//
// runChat MUST NOT call os.Exit, log, or touch any runtime or Install-home directory.
func runChat(_ context.Context, c *cli.Command, stdout, stderr io.Writer, stdin io.Reader) error {
	address := c.String("address")
	token := c.String("token")
	tokenFile := c.String("token-file")
	execMsg := c.String("exec")
	replyTimeout := c.Duration("reply-timeout")
	raw := c.Bool("raw")

	stdinTTY := isTTY(stdin)
	stdoutTTY := isTTY(stdout)
	useTUI := !raw && execMsg == "" && stdinTTY && stdoutTTY

	// `--token-file` reads the shared secret from a file. Precedence is
	// flag `--token` > env `READY_TOKEN` (both already merged into
	// `token` by the cli flag sources) > file. Avoids the secret sitting in
	// the process list or the environment.
	if token == "" && tokenFile != "" {
		data, readErr := os.ReadFile(tokenFile)
		if readErr != nil {
			return fmt.Errorf("failed to read --token-file %q: %w", tokenFile, readErr)
		}

		token = strings.TrimSpace(string(data))
		if token == "" {
			return fmt.Errorf("--token-file %q is empty after trimming whitespace", tokenFile)
		}
	}

	// Session context: cancelled only on SIGINT or clean exit — no global
	// deadline. Each individual agent reply is guarded by replyTimeout.
	chatCtx, chatCancel := context.WithCancel(context.Background())
	defer chatCancel()

	connectionID := uuid.New().String()

	// Client-side redaction sanitizer, built from this process's home dir
	// (mirrors the daemon's privacy.FromHome). Applied before every render on
	// every path (D4) — including --raw (redaction is a privacy transform).
	sanitizer := privacy.FromHome(nil)

	// SIGINT handling (non-TUI paths own it for the whole session; the TUI
	// path stops it before handing off to Bubble Tea's signal handler).
	sigIntFired := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	var endOnce sync.Once
	var cleanupWg sync.WaitGroup
	cleanupWg.Add(1)

	doSessionEnd := func() {
		endOnce.Do(func() {
			defer cleanupWg.Done()
			chatSendSessionEnd(address, connectionID)
		})
	}

	// C1 (criticalities-remediation): every exit path — successful --exec,
	// exit-code mapping, handshake failure, SIGINT, shutdown control — must
	// tell the daemon to release the session and must not block on the
	// cleanup wait-group afterwards. endOnce makes this idempotent with the
	// explicit doSessionEnd() calls below and in the SIGINT handler; the
	// deferred call is the safety net that guarantees the last resort.
	defer doSessionEnd()

	dealer := zmq4.NewDealer(chatCtx, zmq4.WithID(zmq4.SocketIdentity(connectionID)))

	sigHandlerDone := make(chan struct{})
	go func() {
		defer close(sigHandlerDone)
		select {
		case <-sigCh:
			close(sigIntFired)
			_ = dealer.Close()
			chatCancel()
			doSessionEnd()
		case <-chatCtx.Done():
		}
	}()

	if err := dealer.Dial(address); err != nil {
		_, _ = fmt.Fprintf(stderr, "connect: %v\n", err)
		chatCancel()
		<-sigHandlerDone

		return cli.Exit("", ExitErr)
	}

	// --- auth handshake (synchronous on both paths) -------------------------
	var authResp protocol.Response
	var authErr error
	if useTUI {
		authResp, authErr = chatHandshakeDirect(chatCtx, dealer, connectionID, token, replyTimeout, stderr, sigIntFired)
	} else {
		recv := newRecvLoop(dealer)
		go recv.run(chatCtx, sigIntFired)
		authResp, authErr = chatHandshakeLoop(chatCtx, dealer, connectionID, token, replyTimeout, stderr, sigIntFired, recv)

		if authErr != nil {
			// On handshake failure, abort before entering the interactive/exec loop.
			if !isSigIntFired(sigIntFired) {
				doSessionEnd()
				cleanupWg.Wait()
			}

			return authErr
		}

		// Handshake OK: dispatch to the right non-TUI path.
		var result error
		switch {
		case execMsg != "":
			result = chatExec(chatCtx, dealer, connectionID, execMsg, stdout, stderr, sigIntFired, doSessionEnd, replyTimeout, recv, sanitizer, raw)
		default:
			result = chatInteractive(chatCtx, dealer, connectionID, stdout, stderr, stdin, sigIntFired, doSessionEnd, replyTimeout, recv, sanitizer, raw, stdoutTTY)
		}

		if !isSigIntFired(sigIntFired) {
			doSessionEnd()
			cleanupWg.Wait()
		}

		return result
	}

	if authErr != nil {
		if !isSigIntFired(sigIntFired) {
			doSessionEnd()
			cleanupWg.Wait()
		}

		return authErr
	}

	// --- TUI path: hand the DEALER to Bubble Tea ----------------------------
	// chat-logo-modes: the banner is human-attached only. On this TUI path a
	// resolved PNG mode prints the inline PNG to the scrollback immediately
	// before tea.NewProgram (inside tui.Run); glyph mode prints nothing here —
	// the glyph art renders inside the viewport instead. --exec, piped and
	// --raw paths (useTUI false) never reach this and stay decoration-free.
	// Stop runChat's SIGINT handler so Bubble Tea's own signal handler is the
	// sole authority from here (dual-SIGINT dedupe via endOnce + the TUI's
	// quit guard — D10). We deliberately do NOT cancel chatCtx here — it is
	// the very context tui.Run below uses for tea.WithContext, and cancelling
	// it early would make Program.Run observe an already-cancelled context and
	// exit immediately with "context canceled". The residual handler goroutine
	// (blocked in its select) is left to exit naturally when chatCtx.Done()
	// fires via the deferred chatCancel() once runChat itself returns.
	signal.Stop(sigCh)

	logo := resolveLogoMode(os.Getenv(envLogo), imageProtocol())
	// banner-in-transcript (this change): the banner is stamped on the MAIN
	// screen before the alt-screen program starts — PNG when the terminal
	// supports inline images (auto/png modes), glyph art otherwise — and the
	// alt screen hides it for the whole session. On exit the main screen is
	// restored and the banner reappears exactly as it was before the session.
	// Inside the alt-screen frame the TUI ALWAYS leads with the glyph banner
	// (tui.LogoGlyph) — the in-transcript logo that keeps the head of the
	// scrollable history anchored — except in off mode (no logo anywhere).
	if logo != logoModeOff && !printReadyPNG() {
		// No image capability or emit failed → glyph art on the main screen.
		printReadyGlyphs()
	}

	tuiLogo := tui.LogoNone
	if logo != logoModeOff {
		tuiLogo = tui.LogoGlyph
	}

	model, err := tui.Run(chatCtx, dealer, tui.RunOptions{
		ConnectionID: connectionID,
		// session-lifecycle: the conversation Session the handshake reported.
		SessionID:    authResp.SessionID,
		ReplyTimeout: replyTimeout,
		Sanitizer:    sanitizer,
		Stdin:        stdin,
		Stdout:       stdout,
		// llm-status-visibility: seed the status line's model region from
		// the authenticated handshake response.
		ModelRef:   authResp.ModelRef,
		ModelState: authResp.ModelState,
		// tui-usage-observability: seed the usage segment from the
		// handshake (nil — absent — when the daemon has served no turns).
		Usage: authResp.Usage,
		// zmq-session-liveness: the daemon's heartbeat window drives the TUI
		// ping interval (timeout / 3). Zero (old daemon, metadata absent)
		// keeps the TUI's built-in default cadence.
		LivenessTimeout: parseLivenessTimeout(authResp.LivenessTimeout, stderr),
		// chat-logo-modes: the viewport leads with the glyph banner (glyph
		// mode) or with the static info block alone (png/off modes).
		LogoMode: tuiLogo,
		// chat-tui-logo-modes: static info-block facts, all known after the
		// handshake — daemon address, conversation Session ID (SessionID
		// above), client version.
		DaemonAddress: address,
		ClientVersion: clientVersion(),
	})

	doSessionEnd()

	// Map the TUI exit reason + program error to the documented exit code.
	if err != nil {
		if errors.Is(err, tui.ErrInterrupted) {
			_, _ = fmt.Fprintln(stderr, "interrupted")

			return cli.Exit("", ExitSIGINT)
		}

		_, _ = fmt.Fprintf(stderr, "%v\n", err)

		return cli.Exit("", ExitErr)
	}

	reason := model.ExitReason()
	if reason.Message != "" {
		_, _ = fmt.Fprintln(stderr, reason.Message)
	}

	return cli.Exit("", reason.Code)
}

// chatHandshakeDirect performs the two-round HMAC handshake using direct
// synchronous dealer.Recv() calls (TUI path — no recvLoop, D9). Each Recv is
// guarded by replyTimeout and the SIGINT/cancel select. On success it
// returns the authenticated handshake response so the caller can harvest
// the model-status fields (llm-status-visibility).
func chatHandshakeDirect(_ context.Context, dealer zmq4.Socket, connectionID, token string, timeout time.Duration, stderr io.Writer, sigIntFired chan struct{}) (protocol.Response, error) {
	// Round 1 — probe.
	probeReqID := uuid.New().String()
	probe, _ := protocol.Marshal(protocol.Request{
		ConnectionID: connectionID,
		Type:         protocol.RequestTypeSessionStart,
		RequestID:    probeReqID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := chatSendDealer(dealer, probe); err != nil {
		return protocol.Response{}, mapSendError(stderr, err, sigIntFired)
	}

	challengeResp, err := recvDirect(dealer, timeout, sigIntFired)
	if err != nil {
		return protocol.Response{}, handleHandshakeRecvError(stderr, err, sigIntFired)
	}

	if challengeResp.ErrorCode != challenge {
		_, _ = fmt.Fprintf(stderr, "expected challenge, got status=%q error_code=%q\n",
			challengeResp.Status, challengeResp.ErrorCode)

		return protocol.Response{}, cli.Exit("", ExitErr)
	}

	// Round 2 — authenticated.
	return chatHandshakeVerify(dealer, connectionID, token, challengeResp.Content, timeout, stderr, sigIntFired, recvDirect)
}

// chatHandshakeLoop performs the handshake over the recvLoop (non-TUI path).
// On success it returns the authenticated handshake response (the non-TUI
// paths ignore its model-status payload — there is nowhere to display it).
func chatHandshakeLoop(chatCtx context.Context, dealer zmq4.Socket, connectionID, token string, timeout time.Duration, stderr io.Writer, sigIntFired chan struct{}, recv *recvLoop) (protocol.Response, error) {
	probeReqID := uuid.New().String()
	probe, _ := protocol.Marshal(protocol.Request{
		ConnectionID: connectionID,
		Type:         protocol.RequestTypeSessionStart,
		RequestID:    probeReqID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := chatSendDealer(dealer, probe); err != nil {
		return protocol.Response{}, mapSendError(stderr, err, sigIntFired)
	}

	challengeResp, err := recv.waitControl(chatCtx, timeout, sigIntFired)
	if err != nil {
		return protocol.Response{}, handleHandshakeRecvError(stderr, err, sigIntFired)
	}

	if challengeResp.ErrorCode != challenge {
		_, _ = fmt.Fprintf(stderr, "expected challenge, got status=%q error_code=%q\n",
			challengeResp.Status, challengeResp.ErrorCode)

		return protocol.Response{}, cli.Exit("", ExitErr)
	}

	return chatHandshakeVerify(dealer, connectionID, token, challengeResp.Content, timeout, stderr, sigIntFired, func(_ zmq4.Socket, t time.Duration, sig chan struct{}) (protocol.Response, error) {
		return recv.waitControl(chatCtx, t, sig)
	})
}

// recvFn is either recvDirect (TUI) or recv.waitControl (non-TUI).
type recvFn func(dealer zmq4.Socket, timeout time.Duration, sigIntFired chan struct{}) (protocol.Response, error)

// chatHandshakeVerify sends the authenticated session_start and maps the auth
// response to an exit code. On success it returns the authenticated response
// (carrying the daemon's model status, llm-status-visibility).
func chatHandshakeVerify(dealer zmq4.Socket, connectionID, token, nonceHex string, timeout time.Duration, stderr io.Writer, sigIntFired chan struct{}, recv recvFn) (protocol.Response, error) {
	authToken, hmacErr := chatHMAC(token, nonceHex)
	if hmacErr != nil {
		_, _ = fmt.Fprintf(stderr, "auth: %v\n", hmacErr)

		return protocol.Response{}, cli.Exit("", ExitErr)
	}

	verifyReqID := uuid.New().String()
	verify, _ := protocol.Marshal(protocol.Request{
		ConnectionID: connectionID,
		Type:         protocol.RequestTypeSessionStart,
		RequestID:    verifyReqID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		AuthToken:    authToken,
	})
	if err := chatSendDealer(dealer, verify); err != nil {
		return protocol.Response{}, mapSendError(stderr, err, sigIntFired)
	}

	authResp, err := recv(dealer, timeout, sigIntFired)
	if err != nil {
		return protocol.Response{}, handleHandshakeRecvError(stderr, err, sigIntFired)
	}

	switch authResp.Status {
	case protocol.ResponseStatusAuthFail:
		_, _ = fmt.Fprintln(stderr, "authentication failed")

		return protocol.Response{}, cli.Exit("", ExitAuthFail)
	case protocol.ResponseStatusBusy:
		_, _ = fmt.Fprintln(stderr, "daemon busy: another session is active")

		return protocol.Response{}, cli.Exit("", ExitBusy)
	case protocol.ResponseStatusOK:
		return authResp, nil
	default:
		_, _ = fmt.Fprintf(stderr, "unexpected auth response: %s\n", authResp.Status)

		return protocol.Response{}, cli.Exit("", ExitErr)
	}
}

// recvDirect reads one frame from the dealer with a timeout/SIGINT select
// (TUI handshake path). On success the goroutine has returned before the
// caller proceeds, so the dealer is free for the next consumer.
func recvDirect(dealer zmq4.Socket, timeout time.Duration, sigIntFired chan struct{}) (protocol.Response, error) {
	type result struct {
		resp protocol.Response
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		msg, err := dealer.Recv()
		if err != nil {
			ch <- result{err: err}

			return
		}

		if len(msg.Frames) == 0 {
			ch <- result{err: errors.New("empty frame")}

			return
		}

		var resp protocol.Response
		if parseErr := json.Unmarshal(msg.Frames[len(msg.Frames)-1], &resp); parseErr != nil {
			ch <- result{err: parseErr}

			return
		}

		ch <- result{resp: resp}
	}()

	select {
	case r := <-ch:
		return r.resp, r.err
	case <-time.After(timeout):
		return protocol.Response{}, context.DeadlineExceeded
	case <-sigIntFired:
		return protocol.Response{}, errSigInt
	}
}

// recvLoop is the receive-loop goroutine state for the non-TUI paths. It reads
// from the DEALER socket and dispatches control/reply messages into typed
// channels; progress frames are discarded (D16).
type recvLoop struct {
	dealer  zmq4.Socket
	control chan protocol.Response // auth/handshake/post-handshake lifecycle
	replies chan protocol.Response // final answers to message/exec requests
	errs    chan error             // receive errors
}

func newRecvLoop(dealer zmq4.Socket) *recvLoop {
	return &recvLoop{
		dealer:  dealer,
		control: make(chan protocol.Response, 8),
		replies: make(chan protocol.Response, 8),
		errs:    make(chan error, 1),
	}
}

// run reads messages from the DEALER until the context is done or the socket
// is closed. Progress messages are discarded (not printed — D16); control and
// reply messages are routed to their channels for the main goroutine.
func (r *recvLoop) run(ctx context.Context, sigIntFired chan struct{}) {
	for {
		msg, err := r.dealer.Recv()
		if err != nil {
			if ctx.Err() != nil || isSigIntFired(sigIntFired) {
				return
			}
			select {
			case r.errs <- err:
			default:
			}

			return
		}

		if len(msg.Frames) == 0 {
			continue
		}

		raw := msg.Frames[len(msg.Frames)-1]

		var resp protocol.Response
		if parseErr := json.Unmarshal(raw, &resp); parseErr != nil {
			continue
		}

		switch resp.ResponseType {
		case protocol.ResponseTypeProgress:
			// Discard: progress is either accumulated (TUI) or dropped (every
			// other path). Never printed inline (D16).
		case protocol.ResponseTypeControl:
			select {
			case r.control <- resp:
			case <-ctx.Done():
				return
			}
		case protocol.ResponseTypeReply:
			select {
			case r.replies <- resp:
			case <-ctx.Done():
				return
			}
		default:
			// Legacy: messages without response_type are treated as replies.
			select {
			case r.replies <- resp:
			case <-ctx.Done():
				return
			}
		}
	}
}

// waitControl waits for a control message with timeout (handshake).
func (r *recvLoop) waitControl(ctx context.Context, timeout time.Duration, sigIntFired chan struct{}) (protocol.Response, error) {
	select {
	case resp := <-r.control:
		return resp, nil
	case err := <-r.errs:
		return protocol.Response{}, err
	case <-time.After(timeout):
		return protocol.Response{}, context.DeadlineExceeded
	case <-sigIntFired:
		return protocol.Response{}, errSigInt
	case <-ctx.Done():
		return protocol.Response{}, ctx.Err()
	}
}

// waitReply waits for a reply message with timeout, also selecting on the
// post-handshake control channel so a daemon shutdown / session-expiry
// arriving while waiting for a reply is honoured instead of sitting unread
// (D10 / D16).
func (r *recvLoop) waitReply(ctx context.Context, timeout time.Duration, sigIntFired chan struct{}) (protocol.Response, error) {
	select {
	case resp := <-r.replies:
		return resp, nil
	case resp := <-r.control:
		return resp, errControlAfter
	case err := <-r.errs:
		return protocol.Response{}, err
	case <-time.After(timeout):
		return protocol.Response{}, context.DeadlineExceeded
	case <-sigIntFired:
		return protocol.Response{}, errSigInt
	case <-ctx.Done():
		return protocol.Response{}, ctx.Err()
	}
}

// isModelStatusControl reports whether a post-handshake control response is
// an unsolicited informational event: a model-status event
// (llm-status-visibility — Status ok carrying model fields) or a Session
// lifecycle event (session-lifecycle — `session_changed` / `compacted`). The
// non-TUI paths have no status line or transcript to update, so these are
// ignored rather than treated as lifecycle control; the handshake's own
// control messages (challenge / auth result) never reach this predicate
// because they are consumed synchronously by the handshake path.
func isModelStatusControl(resp protocol.Response) bool {
	if resp.ResponseType != protocol.ResponseTypeControl || resp.Status != protocol.ResponseStatusOK || resp.ErrorCode != "" {
		return false
	}

	return resp.ModelState != "" ||
		resp.Content == protocol.ControlSessionChanged ||
		resp.Content == protocol.ControlCompacted
}

// waitReplyIgnoringModelStatus wraps recvLoop.waitReply, skipping
// unsolicited model-status control events: they carry no lifecycle meaning
// for the non-TUI paths, so the wait resumes instead of surfacing them as
// errControlAfter (which would exit the client).
func waitReplyIgnoringModelStatus(ctx context.Context, recv *recvLoop, timeout time.Duration, sigIntFired chan struct{}) (protocol.Response, error) {
	for {
		resp, err := recv.waitReply(ctx, timeout, sigIntFired)
		if err == nil || !errors.Is(err, errControlAfter) || !isModelStatusControl(resp) {
			return resp, err
		}
	}
}

// chatExec sends a single exec message, renders the reply, and exits.
// Progress chunks are discarded (D16). Post-handshake control and error/shutdown
// replies are handled defensively (D10).
func chatExec(
	chatCtx context.Context,
	dealer zmq4.Socket,
	connectionID, execMsg string,
	stdout, stderr io.Writer,
	sigIntFired chan struct{},
	doSessionEnd func(),
	replyTimeout time.Duration,
	recv *recvLoop,
	sanitizer *privacy.Sanitizer,
	raw bool,
) error {
	if isSigIntFired(sigIntFired) {
		return cli.Exit("", ExitSIGINT)
	}

	reqID := uuid.New().String()
	msg, _ := protocol.Marshal(protocol.Request{
		ConnectionID: connectionID,
		Type:         protocol.RequestTypeExec,
		RequestID:    reqID,
		Content:      execMsg,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := chatSendDealer(dealer, msg); err != nil {
		return mapSendError(stderr, err, sigIntFired)
	}

	resp, err := waitReplyIgnoringModelStatus(chatCtx, recv, replyTimeout, sigIntFired)
	if err != nil {
		if errors.Is(err, errControlAfter) {
			return handleControlAfter(resp, stderr, doSessionEnd)
		}

		return handleRecvError(chatCtx, err, sigIntFired, stderr, doSessionEnd)
	}

	if err := finalizeReply(resp, stdout, stderr, doSessionEnd, sanitizer, raw); err != nil {
		return err
	}

	return cli.Exit("", ExitOK)
}

// chatInteractive runs the stdin line-reader loop for the non-TUI paths
// (--raw, non-TTY stdout, piped stdin). Each turn's reply is rendered once
// (notty / auto dark-light / raw per the matrix); progress is discarded.
// Post-handshake control messages are consumed in the idle select too.
func chatInteractive(
	chatCtx context.Context,
	dealer zmq4.Socket,
	connectionID string,
	stdout, stderr io.Writer,
	stdin io.Reader,
	sigIntFired chan struct{},
	doSessionEnd func(),
	replyTimeout time.Duration,
	recv *recvLoop,
	sanitizer *privacy.Sanitizer,
	raw bool,
	stdoutTTY bool,
) error {
	type lineResult struct {
		text string
		err  error
	}
	lineCh := make(chan lineResult, 1)

	go func() {
		scanner := newLineScanner(stdin)
		for scanner.Scan() {
			lineCh <- lineResult{text: scanner.Text()}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			lineCh <- lineResult{err: scanErr}
		}
		close(lineCh)
	}()

	for {
		if isSigIntFired(sigIntFired) {
			return cli.Exit("", ExitSIGINT)
		}

		var lr lineResult
		select {
		case lr2, ok := <-lineCh:
			if !ok {
				doSessionEnd()

				return cli.Exit("", ExitOK)
			}
			lr = lr2
		case ctrl := <-recv.control:
			// Model-status events are ignored on the non-TUI paths — there is
			// no status line to update; keep waiting for input.
			if isModelStatusControl(ctrl) {
				continue
			}

			return handleControlAfter(ctrl, stderr, doSessionEnd)
		case <-chatCtx.Done():
			doSessionEnd()

			return mapChatError(chatCtx, sigIntFired, stderr, chatCtx.Err())
		}

		if lr.err != nil {
			_, _ = fmt.Fprintf(stderr, "stdin: %v\n", lr.err)
			doSessionEnd()

			return cli.Exit("", ExitErr)
		}

		reqID := uuid.New().String()
		payload, _ := protocol.Marshal(protocol.Request{
			ConnectionID: connectionID,
			Type:         protocol.RequestTypeMessage,
			RequestID:    reqID,
			Content:      lr.text,
			Timestamp:    time.Now().UTC().Format(time.RFC3339),
		})
		if err := chatSendDealer(dealer, payload); err != nil {
			doSessionEnd()

			return mapSendError(stderr, err, sigIntFired)
		}

		resp, err := waitReplyIgnoringModelStatus(chatCtx, recv, replyTimeout, sigIntFired)
		if err != nil {
			if errors.Is(err, errControlAfter) {
				return handleControlAfter(resp, stderr, doSessionEnd)
			}

			if errors.Is(err, context.DeadlineExceeded) {
				_, _ = fmt.Fprintln(stderr, "timeout")
				doSessionEnd()

				return cli.Exit("", ExitTimeout)
			}

			doSessionEnd()

			return mapChatError(chatCtx, sigIntFired, stderr, err)
		}

		if err := finalizeReply(resp, stdout, stderr, doSessionEnd, sanitizer, raw); err != nil {
			return err
		}
		// Continue the loop for the next line.
		_ = stdoutTTY // (styling is resolved inside finalizeReply via the stdout *os.File)
	}
}

// finalizeReply renders a reply (or handles a defensive error/shutdown status)
// and writes it to stdout. Used by both chatExec and chatInteractive.
// An llm_unavailable reply (llm-status-visibility) is a completed-but-failed
// request: its terminal message goes to stderr and the client exits 1 per
// the chat exit-code contract — on these paths there is no session to keep
// open.
func finalizeReply(resp protocol.Response, stdout, stderr io.Writer, doSessionEnd func(), sanitizer *privacy.Sanitizer, raw bool) error {
	switch resp.Status {
	case protocol.ResponseStatusShutdown:
		_, _ = fmt.Fprintln(stderr, "daemon shut down")
		doSessionEnd()

		return cli.Exit("", ExitOK)
	case protocol.ResponseStatusError:
		if resp.ErrorCode == protocol.ErrorCodeLLMUnavailable {
			_, _ = fmt.Fprintln(stderr, resp.Content)
			doSessionEnd()

			return cli.Exit("", ExitErr)
		}

		if resp.ErrorCode == "no_active_session" || resp.ErrorCode == "session_timeout" {
			_, _ = fmt.Fprintln(stderr, "session expired")
		} else {
			_, _ = fmt.Fprintf(stderr, "error: %s\n", resp.ErrorCode)
		}
		doSessionEnd()

		return cli.Exit("", ExitErr)
	case protocol.ResponseStatusOK:
		rendered := renderOneShot(stdout, sanitizer, raw, resp.Content)
		_, _ = fmt.Fprintln(stdout, rendered)

		return nil
	default:
		_, _ = fmt.Fprintf(stderr, "unexpected status: %s\n", resp.Status)
		doSessionEnd()

		return cli.Exit("", ExitErr)
	}
}

// renderOneShot renders markdown for a one-shot stdout path: auto dark/light on
// TTY, notty off-TTY, raw with --raw (D6 / D7). Redaction is always applied.
func renderOneShot(stdout io.Writer, sanitizer *privacy.Sanitizer, raw bool, content string) string {
	f, _ := stdout.(*os.File)
	if raw {
		return mdrender.New(mdrender.OneShotWidth(f), mdrender.StyleRaw, sanitizer).Render(content)
	}

	style := mdrender.ResolveStyleOneShot(f)

	return mdrender.New(mdrender.OneShotWidth(f), style, sanitizer).Render(content)
}

// handleControlAfter maps a post-handshake control message to an exit code
// (D10): shutdown → 0, no_active_session/session_timeout → "session expired"
// → 1, other error → 1.
func handleControlAfter(resp protocol.Response, stderr io.Writer, doSessionEnd func()) error {
	switch resp.Status {
	case protocol.ResponseStatusShutdown:
		_, _ = fmt.Fprintln(stderr, "daemon shut down")
		doSessionEnd()

		return cli.Exit("", ExitOK)
	case protocol.ResponseStatusError:
		if resp.ErrorCode == "no_active_session" || resp.ErrorCode == "session_timeout" {
			_, _ = fmt.Fprintln(stderr, "session expired")
		} else {
			_, _ = fmt.Fprintf(stderr, "error: %s\n", resp.ErrorCode)
		}
		doSessionEnd()

		return cli.Exit("", ExitErr)
	default:
		doSessionEnd()

		return cli.Exit("", ExitErr)
	}
}

// handleHandshakeRecvError converts a handshake receive error to an exit code.
func handleHandshakeRecvError(stderr io.Writer, err error, sigIntFired chan struct{}) error {
	if isSigIntFired(sigIntFired) {
		return cli.Exit("", ExitSIGINT)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		_, _ = fmt.Fprintln(stderr, "timeout")

		return cli.Exit("", ExitTimeout)
	}

	if errors.Is(err, errSigInt) {
		return cli.Exit("", ExitSIGINT)
	}

	_, _ = fmt.Fprintf(stderr, "%v\n", err)

	return cli.Exit("", ExitErr)
}

// handleRecvError converts receive errors to proper exit handling.
func handleRecvError(chatCtx context.Context, err error, sigIntFired chan struct{}, stderr io.Writer, doSessionEnd func()) error {
	if errors.Is(err, context.DeadlineExceeded) {
		_, _ = fmt.Fprintln(stderr, "timeout")
		doSessionEnd()

		return cli.Exit("", ExitTimeout)
	}

	return mapChatError(chatCtx, sigIntFired, stderr, err)
}

// mapSendError converts a send error to an exit code (SIGINT-aware).
func mapSendError(stderr io.Writer, err error, sigIntFired chan struct{}) error {
	if isSigIntFired(sigIntFired) {
		return cli.Exit("", ExitSIGINT)
	}

	_, _ = fmt.Fprintf(stderr, "%v\n", err)

	return cli.Exit("", ExitErr)
}

// mapChatError converts a context / send / recv error to the right exit code.
// SIGINT → exit 130. Deadline exceeded → print "timeout" + exit 124. Other → exit 1.
func mapChatError(ctx context.Context, sigIntFired chan struct{}, stderr io.Writer, err error) error {
	if isSigIntFired(sigIntFired) {
		return cli.Exit("", ExitSIGINT)
	}

	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		_, _ = fmt.Fprintln(stderr, "timeout")

		return cli.Exit("", ExitTimeout)
	}

	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
	}

	return cli.Exit("", ExitErr)
}

// isTTY reports whether r is a *os.File attached to a terminal.
func isTTY(r any) bool {
	f, ok := r.(*os.File)

	return ok && f != nil && term.IsTerminal(int(f.Fd()))
}

// clientVersion reports the chat client's version for the TUI info block
// (chat-tui-logo-modes task 4.4): the main module version from the embedded
// build info when available (go install / release builds), "dev" otherwise
// (plain `go build` / `go run`, and test binaries).
func clientVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return "dev"
}

// newLineScanner wraps stdin in a *bufio.Scanner for the interactive
// line-reader fallback.
func newLineScanner(stdin io.Reader) *bufio.Scanner {
	return bufio.NewScanner(stdin)
}

// isSigIntFired returns true if the SIGINT signal has been received.
func isSigIntFired(sigIntFired chan struct{}) bool {
	select {
	case <-sigIntFired:
		return true
	default:
		return false
	}
}

// chatSendDealer sends raw bytes on the DEALER socket.
func chatSendDealer(dealer zmq4.Socket, data []byte) error {
	if err := dealer.Send(zmq4.NewMsg(data)); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	return nil
}

// chatHMAC computes hex(HMAC-SHA256(secret, hex-decoded nonce)).
// This is the client side of the auth handshake described in the design doc.
// Returns an error if the nonce is not valid hexadecimal.
func chatHMAC(secret, nonceHex string) (string, error) {
	nonce, err := hex.DecodeString(nonceHex)
	if err != nil {
		return "", fmt.Errorf("invalid challenge nonce (not hex): %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(nonce)

	return hex.EncodeToString(mac.Sum(nil)), nil
}

// parseLivenessTimeout decodes the daemon's handshake liveness_timeout
// metadata (zmq-session-liveness). An absent or unparseable value yields 0 —
// the TUI then keeps its built-in default cadence — and a bad value is logged
// at most once, never failing the connection.
func parseLivenessTimeout(raw string, stderr io.Writer) time.Duration {
	if raw == "" {
		return 0
	}

	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		_, _ = fmt.Fprintf(stderr, "chat: ignoring invalid liveness_timeout %q from daemon\n", raw)

		return 0
	}

	return d
}

// chatSendSessionEnd sends a best-effort session_end using a fresh DEALER
// connection and waits for the server acknowledgment before closing the socket.
func chatSendSessionEnd(address, connectionID string) {
	reqID := uuid.New().String()
	data, _ := protocol.Marshal(protocol.Request{
		ConnectionID: connectionID,
		Type:         protocol.RequestTypeSessionEnd,
		RequestID:    reqID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	// Use a fresh short-timeout context — 3 s is generous enough for a local
	// daemon while not hanging a Ctrl+C for too long.
	endCtx, endCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer endCancel()

	endDealer := zmq4.NewDealer(endCtx, zmq4.WithID(zmq4.SocketIdentity(connectionID+"-end")))
	defer func() {
		_ = endDealer.Close()
	}()

	if err := endDealer.Dial(address); err != nil {
		return
	}

	if err := endDealer.Send(zmq4.NewMsg(data)); err != nil {
		return
	}

	// Wait for the reply to ensure the message was delivered.
	_, _ = endDealer.Recv()
}
