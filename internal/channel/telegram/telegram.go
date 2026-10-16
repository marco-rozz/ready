// Package telegram implements the channel.Channel interface for the Telegram
// Bot API using long polling. Incoming text messages are surfaced to the
// channel manager with the originating chat_id stored in Metadata; outgoing
// messages are sent in HTML parse mode and split on the 4096-character limit.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
)

// ChannelName is the channel identifier used for routing and session keys.
const ChannelName = "telegram"

// MaxMessageLength is the Telegram payload limit for a single message in HTML mode.
const MaxMessageLength = 4096

// MetadataChatID is the Metadata key carrying the Telegram chat_id (base-10 string).
const MetadataChatID = "chat_id"

// Sentinel errors surfaced by the channel.
var (
	ErrMissingChatID = errors.New("telegram: missing chat_id in message metadata")
	ErrInvalidChatID = errors.New("telegram: invalid chat_id metadata")
	ErrClosed        = errors.New("telegram: channel closed")
	// ErrConflict is returned (and logged, followed by an exponential-backoff
	// retry) when Telegram rejects our long-poll because another bot instance
	// is running. The conflict is a *property* condition, not a permanent
	// failure: once the competing listener exits, the next GetUpdates
	// succeeds again — so the poll loop keeps retrying instead of shutting
	// down; two daemons fighting over the same token is an operator
	// misconfiguration that shows up in the repeated WARN entries.
	ErrConflict = errors.New("telegram: another bot instance is already running (HTTP 409 Conflict)")
)

// botAPI is the narrow surface of tgbotapi.BotAPI consumed by Channel.
// It exists so tests can inject a mock implementation.
type botAPI interface {
	Send(ctx context.Context, c tgbotapi.Chattable) (tgbotapi.Message, error)
	SendChatAction(ctx context.Context, chatID int64, action string) error
	GetUpdates(config tgbotapi.UpdateConfig) ([]tgbotapi.Update, error)
	// StopReceivingUpdates is called on Close to unblock any in-flight GetUpdates.
	StopReceivingUpdates()
}

// commandRegistrar is the optional bot surface for the Session commands
// (session-lifecycle 5.3): the bot's own username, stripped from
// `/cmd@<bot>` forms, and the setMyCommands registration. The live wrapper
// implements it; test mocks that do not are simply skipped.
type commandRegistrar interface {
	BotUserName() string
	SetCommands(ctx context.Context, commands []tgbotapi.BotCommand) error
}

// sessionCommands are the daemon-side Session commands registered in the
// Bot API command menu.
func sessionCommands() []tgbotapi.BotCommand {
	return []tgbotapi.BotCommand{
		{Command: "new", Description: "Start a new session"},
		{Command: "compact", Description: "Compact the conversation context"},
	}
}

// telegramHTTPTimeout bounds every HTTP request to the Telegram Bot API,
// including the 10 s long-poll window of GetUpdates. Without it the client
// request has NO timeout: after a network blip (half-dead keep-alive TCP
// connection) the long poll stays wedged forever, pollLoop never returns,
// and Channel.Close() — which waits on it — blocked the whole daemon
// shutdown (reproduced 2026-09-18; see shutdown_stall_test.go). Must stay
// larger than the long-poll window (updateCfg.Timeout = 10 s).
const telegramHTTPTimeout = 20 * time.Second

// telegramPollWindow is the long-poll window passed to GetUpdates (server
// side); it must stay below telegramHTTPTimeout so the client timeout is
// never what ends a normal poll.
const telegramPollWindow = 10 // seconds (tgbotapi UpdateConfig.Timeout unit)

// newHTTPClient builds the HTTP client every Telegram API request runs on.
// Var (not func) solely so tests can override it; production never swaps it.
var newHTTPClient = func() *http.Client { //nolint:gochecknoglobals // test seam: tests override with a short timeout; production always gets telegramHTTPTimeout
	return &http.Client{Timeout: telegramHTTPTimeout}
}

// botWrapper provides a way to bypass the bug in tgbotapi.BotAPI.Send
// where it fails to unmarshal the boolean response of sendChatAction.
type botWrapper struct {
	api   *tgbotapi.BotAPI
	token string
	// hc bounds sendChatAction's hand-rolled request; the wrapper bypasses
	// bot.Client for that call, so it must not fall back to the unbounded
	// http.DefaultClient.
	hc *http.Client
}

func (w *botWrapper) Send(ctx context.Context, c tgbotapi.Chattable) (tgbotapi.Message, error) {
	// criticalities-remediation (LOW lifecycle, ctx propagation): tgbotapi's
	// Send is not context-aware — an already-cancelled context aborts the
	// call here, guaranteeing no network request is issued after
	// cancellation.
	if err := ctx.Err(); err != nil {
		return tgbotapi.Message{}, fmt.Errorf("telegram: send cancelled: %w", err)
	}

	return w.api.Send(c)
}

func (w *botWrapper) SendChatAction(ctx context.Context, chatID int64, action string) error {
	u, err := url.Parse("https://api.telegram.org")
	if err != nil {
		return fmt.Errorf("telegram: invalid base URL: %w", err)
	}
	u.Path = fmt.Sprintf("/bot%s/sendChatAction", w.token)

	data := url.Values{}
	data.Set("chat_id", strconv.FormatInt(chatID, 10))
	data.Set("action", action)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("telegram: failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := w.hc.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: http request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return fmt.Errorf("telegram: sendChatAction failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// BotUserName returns the bot's username (from getMe at construction).
func (w *botWrapper) BotUserName() string { return w.api.Self.UserName }

// SetCommands registers the bot's command menu (setMyCommands).
func (w *botWrapper) SetCommands(ctx context.Context, commands []tgbotapi.BotCommand) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("telegram: setMyCommands cancelled: %w", err)
	}

	if _, err := w.api.Request(tgbotapi.NewSetMyCommands(commands...)); err != nil {
		return fmt.Errorf("telegram: setMyCommands failed: %w", err)
	}

	return nil
}

func (w *botWrapper) GetUpdates(config tgbotapi.UpdateConfig) ([]tgbotapi.Update, error) {
	return w.api.GetUpdates(config)
}

func (w *botWrapper) StopReceivingUpdates() {
	// No-op for the direct-GetUpdates path; kept so Close() can signal mocks.
}

// Guarantees at compile time that botWrapper implements botAPI and the
// optional commandRegistrar.
var (
	_ botAPI           = (*botWrapper)(nil)
	_ commandRegistrar = (*botWrapper)(nil)
)

// Guarantees at compile time that Channel implements channel.Channel.
var _ channel.Channel = (*Channel)(nil)

// Channel is the Telegram implementation of channel.Channel.
type Channel struct {
	bot       botAPI
	allowFrom map[string]struct{}
	incoming  chan channel.Message
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	logger    zerolog.Logger
}

// New creates a Channel backed by a live tgbotapi.BotAPI client.
func New(cfg config.TelegramChannelConfig, rootLogger zerolog.Logger) (*Channel, error) {
	if cfg.Token == "" {
		return nil, errors.New("telegram: token is required")
	}

	bot, err := tgbotapi.NewBotAPIWithClient(cfg.Token, tgbotapi.APIEndpoint, newHTTPClient())
	if err != nil {
		return nil, fmt.Errorf("telegram: failed to create bot api: %w", err)
	}

	wrapper := &botWrapper{
		api:   bot,
		token: cfg.Token,
		hc:    newHTTPClient(),
	}

	return newChannel(wrapper, cfg.AllowFrom, rootLogger), nil
}

// newChannel is the unexported constructor that accepts any botAPI implementation.
func newChannel(bot botAPI, allowFrom []string, rootLogger zerolog.Logger) *Channel {
	allow := make(map[string]struct{}, len(allowFrom))

	for _, v := range allowFrom {
		if v == "" {
			continue
		}

		allow[v] = struct{}{}
	}

	c := &Channel{
		bot:       bot,
		allowFrom: allow,
		incoming:  make(chan channel.Message, 32),
		done:      make(chan struct{}),
		logger:    rootLogger.With().Str("component", "telegram").Logger(),
	}

	c.wg.Add(1)

	go c.pollLoop()

	return c
}

// pollLoop calls GetUpdates in a loop, filters messages, and forwards them
// onto the buffered incoming queue until Close is called.
//
// conflictBackoffBase is the first wait after an HTTP 409 Conflict; it
// doubles per successive conflict and is capped at conflictBackoffMax. Var
// (not const) solely so tests can shrink it; production never overrides it.
var conflictBackoffBase = 10 * time.Second //nolint:gochecknoglobals // test seam, same pattern as newHTTPClient

// conflictBackoffMax caps the exponential conflict backoff; a successful
// poll resets the ladder to its base.
const conflictBackoffMax = 5 * time.Minute

// On HTTP 409 Conflict (another bot instance polling the same token) the
// loop keeps retrying with exponential backoff instead of shutting down:
// Telegram's 409 is a property condition — it also terminates the other
// instance's in-flight long poll, and once that listener exits our next
// GetUpdates succeeds, restoring control without a daemon restart.
// For all other transient errors it waits 3 s before retrying.
// Shutdown delay on Close is bounded by updateCfg.Timeout (10 s).
func (c *Channel) pollLoop() {
	defer c.wg.Done()

	updateCfg := tgbotapi.NewUpdate(0)
	updateCfg.Timeout = telegramPollWindow // long-poll window; client timeout (telegramHTTPTimeout) stays above it
	conflictAttempt := 0

	c.registerCommands()

	c.logger.Info().Msg("poll loop started, waiting for updates…")

	for {
		select {
		case <-c.done:
			c.logger.Info().Msg("poll loop stopped (done signal)")

			return
		default:
		}

		updates, err := c.bot.GetUpdates(updateCfg)
		if err != nil {
			if tgErr, ok := errors.AsType[*tgbotapi.Error](err); ok && tgErr.Code == http.StatusConflict {
				conflictAttempt++

				delay := conflictBackoffBase
				for i := 1; i < conflictAttempt && delay < conflictBackoffMax; i++ {
					delay *= 2
				}

				if delay > conflictBackoffMax {
					delay = conflictBackoffMax
				}

				c.logger.Warn().Err(ErrConflict).
					Str("retry_in", delay.String()).
					Msg("another bot instance is polling — retrying until it releases the long poll; control is restored automatically")

				select {
				case <-c.done:
					return
				case <-time.After(delay):
				}

				continue
			}

			c.logger.Warn().Err(err).Msg("GetUpdates error")
			select {
			case <-c.done:
				return
			case <-time.After(3 * time.Second):
			}

			continue
		}

		// A successful poll clears the conflict ladder.
		conflictAttempt = 0

		for _, upd := range updates {
			if upd.UpdateID >= updateCfg.Offset {
				updateCfg.Offset = upd.UpdateID + 1
			}

			msg := c.translate(upd)
			if msg == nil {
				continue
			}

			c.logger.Info().
				Str("chat_id", msg.Metadata[MetadataChatID]).
				Str("message", truncate(msg.Content, 80)).
				Msg("message received")

			select {
			case c.incoming <- *msg:
			case <-c.done:
				return
			}
		}
	}
}

// registerCommands registers the Session commands in the Bot API command
// menu. A failure is logged at WARN and never prevents the channel from
// running.
func (c *Channel) registerCommands() {
	reg, ok := c.bot.(commandRegistrar)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), telegramHTTPTimeout)
	defer cancel()

	if err := reg.SetCommands(ctx, sessionCommands()); err != nil {
		c.logger.Warn().Err(err).Msg("failed to register the session commands (setMyCommands)")

		return
	}

	c.logger.Info().Msg("session commands registered (/new, /compact)")
}

// stripBotMention rewrites a leading `/cmd@<bot_username>` to `/cmd` (the
// form Telegram clients send in group chats); any other text is returned
// unchanged. The match on the username is case-insensitive.
func (c *Channel) stripBotMention(text string) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}

	reg, ok := c.bot.(commandRegistrar)
	if !ok || reg.BotUserName() == "" {
		return text
	}

	head, rest, _ := strings.Cut(text, " ")

	cmd, mention, found := strings.Cut(head, "@")
	if !found || !strings.EqualFold(mention, reg.BotUserName()) {
		return text
	}

	if rest == "" {
		return cmd
	}

	return cmd + " " + rest
}

// translate converts a Telegram update into a channel.Message, returning nil
// when the update should be ignored (non-text, missing chat, or unauthorised).
func (c *Channel) translate(upd tgbotapi.Update) *channel.Message {
	if upd.Message == nil || upd.Message.Chat == nil {
		return nil
	}

	text := strings.TrimSpace(upd.Message.Text)
	if text == "" {
		return nil
	}

	chatIDStr := strconv.FormatInt(upd.Message.Chat.ID, 10)
	if !c.isAllowed(chatIDStr, upd.Message) {
		userName := ""
		if upd.Message.From != nil {
			userName = upd.Message.From.UserName
		}

		c.logger.Warn().
			Str("chat_id", chatIDStr).
			Str("user", userName).
			Msg("message rejected by whitelist")

		return nil
	}

	return &channel.Message{
		Content:     c.stripBotMention(text),
		ChannelName: ChannelName,
		Metadata: map[string]string{
			MetadataChatID: chatIDStr,
		},
	}
}

// isAllowed reports whether the sender of msg is permitted by the whitelist.
// An empty whitelist accepts everyone.  A non-empty whitelist matches against
// the chat_id, the user id, the bare username, and @username — whichever
// representation the operator put in the YAML.
func (c *Channel) isAllowed(chatIDStr string, msg *tgbotapi.Message) bool {
	if len(c.allowFrom) == 0 {
		return true
	}

	if _, ok := c.allowFrom[chatIDStr]; ok {
		return true
	}

	if msg.From == nil {
		return false
	}

	userIDStr := strconv.FormatInt(msg.From.ID, 10)
	if _, ok := c.allowFrom[userIDStr]; ok {
		return true
	}

	if msg.From.UserName == "" {
		return false
	}

	if _, ok := c.allowFrom[msg.From.UserName]; ok {
		return true
	}

	_, ok := c.allowFrom["@"+msg.From.UserName]

	return ok
}

// Name returns the channel identifier.
func (c *Channel) Name() string { return ChannelName }

// Receive blocks until a Telegram message arrives, the context is cancelled,
// or the channel is closed.
func (c *Channel) Receive(ctx context.Context) (channel.Message, error) {
	select {
	case <-ctx.Done():
		return channel.Message{}, ctx.Err()
	case <-c.done:
		return channel.Message{}, ErrClosed
	case msg, ok := <-c.incoming:
		if !ok {
			return channel.Message{}, ErrClosed
		}

		return msg, nil
	}
}

// Send delivers msg.Content to the chat identified by msg.Metadata[chat_id],
// in HTML parse mode, splitting on the Telegram MaxMessageLength limit.
//
// The content is sanitised before sending: only Telegram-supported HTML tags
// are preserved, everything else is entity-escaped.  If the Telegram API
// still rejects the HTML payload (e.g. unclosed tags slipping through) the
// method retries each failing chunk in plain-text mode so the user always
// receives a response.
func (c *Channel) Send(ctx context.Context, msg channel.Message) error {
	chatIDStr, ok := msg.Metadata[MetadataChatID]
	if !ok || chatIDStr == "" {
		return ErrMissingChatID
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidChatID, chatIDStr)
	}

	sanitized := sanitizeHTML(msg.Content)

	for _, chunk := range splitMessage(sanitized, MaxMessageLength) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("telegram: send cancelled: %w", ctx.Err())
		default:
		}

		if sendErr := c.sendChunk(ctx, chatID, chunk); sendErr != nil {
			return sendErr
		}
	}

	return nil
}

// sendChunk sends a single text chunk to chatID in HTML mode.  If the
// Telegram API rejects the HTML, it falls back to plain text so the user
// always receives a response.
func (c *Channel) sendChunk(ctx context.Context, chatID int64, chunk string) error {
	outgoing := tgbotapi.NewMessage(chatID, chunk)
	outgoing.ParseMode = tgbotapi.ModeHTML
	outgoing.DisableWebPagePreview = true

	if _, sendErr := c.bot.Send(ctx, outgoing); sendErr == nil {
		return nil
	} else if !c.isParseError(ctx, sendErr) {
		// criticalities-remediation (M6): the plain-text fallback exists to
		// recover from HTML PARSE rejections (HTTP 400) — a blind retry on
		// every bot.Send error risks double-posting on transient network
		// failures and issues a second network call after context
		// cancellation. Other error classes surface immediately.
		return fmt.Errorf("telegram: failed to send message: %w", sendErr)
	}

	// HTML rejected (parse) — fall back to plain text.
	c.logger.Warn().Msg("HTML send rejected (parse), retrying as plain text")

	plain := stripHTMLTags(chunk)
	outgoing = tgbotapi.NewMessage(chatID, plain)
	outgoing.DisableWebPagePreview = true

	if _, err := c.bot.Send(ctx, outgoing); err != nil {
		return fmt.Errorf("telegram: failed to send message (plain-text fallback): %w", err)
	}

	return nil
}

// isParseError reports whether a bot.Send failure is an HTML parse rejection
// (Telegram HTTP 400) — the only error class the plain-text fallback covers.
func (c *Channel) isParseError(_ context.Context, err error) bool {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusBadRequest
	}

	// Some wrappers surface the status as a plain error string: Telegram's
	// HTTP 400 body is always prefixed "Bad Request".
	lower := strings.ToLower(err.Error())

	return strings.Contains(lower, "bad request") && strings.Contains(lower, "parse")
}

// Close stops the polling loop. It is safe to call multiple times.
// Bounded by telegramHTTPTimeout (20 s): the long-poll window itself is
// 10 s, so an in-flight GetUpdates returns at the latest when the client
// HTTP timeout fires — even if the TCP connection underneath is wedged
// (half-dead keep-alive after a network reset).
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.bot.StopReceivingUpdates() // signals mock implementations to unblock
	})

	c.wg.Wait()

	return nil
}

// splitMessage cuts text into chunks of at most maxLen runes, preferring to
// break at the last newline within the window when one is reasonably close to
// the end (>= maxLen/2). When text fits in a single chunk it is returned as-is.
func splitMessage(text string, maxLen int) []string {
	if maxLen <= 0 || text == "" {
		return []string{text}
	}

	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}

	var chunks []string

	for len(runes) > maxLen {
		cut := maxLen

		if idx := lastIndexRune(runes[:maxLen], '\n'); idx >= maxLen/2 {
			cut = idx + 1
		}

		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}

	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}

	return chunks
}

// lastIndexRune returns the index of the last occurrence of target in runes,
// or -1 when not found.
func lastIndexRune(runes []rune, target rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == target {
			return i
		}
	}

	return -1
}

// SendTyping sends a "typing…" chat action to the specified chat so the user
// sees that the bot is processing their message.  Errors are logged but never
// propagated — typing indicators are best-effort.
func (c *Channel) SendTyping(ctx context.Context, chatID int64) {
	if err := c.bot.SendChatAction(ctx, chatID, tgbotapi.ChatTyping); err != nil {
		c.logger.Warn().Err(err).Int64("chat_id", chatID).Msg("failed to send typing action")
	}
}

// SendStreamMessage sends the first chunk of a streamed response as a new
// Telegram message and returns the message_id for subsequent edits. The
// content is sanitized as HTML; on failure, a plain-text fallback is attempted.
func (c *Channel) SendStreamMessage(ctx context.Context, chatID int64, content string) (int, error) {
	sanitized := sanitizeHTML(content)

	outgoing := tgbotapi.NewMessage(chatID, sanitized)
	outgoing.ParseMode = tgbotapi.ModeHTML
	outgoing.DisableWebPagePreview = true

	sent, err := c.bot.Send(ctx, outgoing)
	if err != nil {
		// HTML rejected — retry as plain text.
		c.logger.Warn().Err(err).Msg("stream HTML send failed, retrying as plain text")

		outgoing = tgbotapi.NewMessage(chatID, stripHTMLTags(content))
		outgoing.DisableWebPagePreview = true

		sent, err = c.bot.Send(ctx, outgoing)
		if err != nil {
			return 0, fmt.Errorf("telegram: failed to send stream message: %w", err)
		}
	}

	return sent.MessageID, nil
}

// SendStreamEdit edits an existing Telegram message (identified by messageID)
// in chatID with new content. Used for incremental streaming delivery: the
// first chunk creates the message via SendStreamMessage, subsequent edits
// update it. The content is sanitized as HTML; on failure, a plain-text
// fallback is attempted. Content is truncated to MaxMessageLength.
func (c *Channel) SendStreamEdit(ctx context.Context, chatID int64, messageID int, content string) error {
	if content == "" {
		return nil
	}

	// Truncate to Telegram's 4096-char limit.
	sanitized := sanitizeHTML(content)
	if len([]rune(sanitized)) > MaxMessageLength {
		sanitized = string([]rune(sanitized)[:MaxMessageLength])
	}

	edit := tgbotapi.EditMessageTextConfig{
		BaseEdit: tgbotapi.BaseEdit{
			ChatID:    chatID,
			MessageID: messageID,
		},
		Text:                  sanitized,
		ParseMode:             tgbotapi.ModeHTML,
		DisableWebPagePreview: true,
	}

	if _, err := c.bot.Send(ctx, edit); err == nil {
		return nil
	}

	// HTML rejected — retry as plain text.
	c.logger.Warn().Msg("stream HTML edit failed, retrying as plain text")

	plain := stripHTMLTags(content)
	if len([]rune(plain)) > MaxMessageLength {
		plain = string([]rune(plain)[:MaxMessageLength])
	}

	edit = tgbotapi.EditMessageTextConfig{
		BaseEdit: tgbotapi.BaseEdit{
			ChatID:    chatID,
			MessageID: messageID,
		},
		Text:                  plain,
		DisableWebPagePreview: true,
	}

	if _, err := c.bot.Send(ctx, edit); err != nil {
		return fmt.Errorf("telegram: failed to edit stream message: %w", err)
	}

	return nil
}

// truncate shortens s to at most maxLen runes, appending "…" when truncated.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}

	return string(runes[:maxLen]) + "…"
}
