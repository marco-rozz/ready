package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/cli"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/okf"
	"github.com/marco-rozz/ready/internal/session"
)

// /verify replies (okf-authoring): fixed English product strings.
const (
	msgVerifyUsage        = "Usage: /verify <bundle>/<concept> [<your-id> [<token>]]"
	msgVerifyAuth         = "Verification requires an authenticated channel."
	msgVerifyBundle       = "Bundle not found."
	msgVerifyConcept      = "Concept not found."
	msgVerifyNotConcept   = "Not a concept."
	msgVerifyBadRef       = "Invalid concept reference."
	msgVerifyBadID        = "Invalid identity: use 1-64 characters from A-Z a-z 0-9 . _ @ + -."
	msgVerifyBadToken     = "Invalid token."
	msgVerifyUnmergeable  = "The concept cannot be updated: fix its frontmatter first."
	msgVerifyUnavailable  = "Knowledge bundles are not available."
	msgVerifyFailed       = "Verification failed; see the daemon log."
	msgVerifyNotCurrent   = "Verification recorded, but it is not current: generated.at is later than now."
	verifyChunkRunes      = 3500
	verifyPlaceholderID   = "<your-id>"
	verifyPlaceholderTkn  = "<token>"
	verifyConfirmLeadText = "To record your verification of exactly this content, send:"
)

// verifyPrincipalChecker is implemented by channels that can tell an
// authenticated principal from a mere allowed sender.
type verifyPrincipalChecker interface {
	VerifyPrincipal(md map[string]string) bool
}

// verifyPrincipal reports whether msg comes from an authenticated principal:
// the stdin CLI, the HMAC-authenticated ZMQ CLI (both are the "cli" channel),
// or a Telegram private chat whose sender's numeric user id is in allow_from.
func (rt *sessionRuntime) verifyPrincipal(msg channel.Message) bool {
	switch msg.ChannelName {
	case cli.ChannelName:
		return true
	case telegram.ChannelName:
		if rt.mgr == nil {
			return false
		}
		chk, ok := rt.mgr.Channel(msg.ChannelName).(verifyPrincipalChecker)

		return ok && chk.VerifyPrincipal(msg.Metadata)
	}

	return false
}

// handleVerify runs the two-phase /verify command: never a Turn, never
// recorded, never reaching the model.
func (rt *sessionRuntime) handleVerify(ctx context.Context, addr session.Address, msg channel.Message, arg string) {
	if !rt.verifyPrincipal(msg) {
		rt.sendFixed(ctx, msg, msgVerifyAuth)

		return
	}
	if rt.okf == nil {
		rt.sendFixed(ctx, msg, msgVerifyUnavailable)

		return
	}
	args := strings.Fields(arg)
	if len(args) == 0 || len(args) > 3 {
		rt.sendFixed(ctx, msg, msgVerifyUsage)

		return
	}
	bundle, id, err := okf.ParseConceptRef(args[0])
	if err != nil {
		rt.sendFixed(ctx, msg, msgVerifyBadRef)

		return
	}
	if len(args) >= 2 && !okf.ValidHumanID(args[1]) {
		rt.sendFixed(ctx, msg, msgVerifyBadID)

		return
	}
	if len(args) == 3 && !okf.ValidToken(args[2]) {
		rt.sendFixed(ctx, msg, msgVerifyBadToken)

		return
	}
	qualified := bundle + "/" + id

	if len(args) == 3 {
		res, confirmErr := rt.okf.VerifyConfirm(ctx, bundle, id, args[1], args[2], msg.ChannelName)
		if confirmErr != nil {
			rt.sendFixed(ctx, msg, rt.verifyErrorReply(confirmErr, qualified))

			return
		}
		rt.logger.Info().Str("address", string(addr)).Str("channel", msg.ChannelName).
			Str("bundle", bundle).Str("concept", id).Str("human_id", res.HumanID).
			Msg("okf verification recorded")
		if res.NotCurrent {
			rt.sendFixed(ctx, msg, msgVerifyNotCurrent)

			return
		}
		rt.sendFixed(ctx, msg, fmt.Sprintf("Verification recorded. Tier: %s.", res.Tier))

		return
	}

	pv, err := rt.okf.VerifyPreview(ctx, bundle, id)
	if err != nil {
		rt.sendFixed(ctx, msg, rt.verifyErrorReply(err, qualified))

		return
	}
	who := verifyPlaceholderID
	if len(args) == 2 {
		who = args[1]
	}
	rt.sendVerifyPreview(ctx, msg, pv, who)
}

// verifyErrorReply maps a writer error to its fixed reply, logging the
// unexpected ones.
func (rt *sessionRuntime) verifyErrorReply(err error, qualified string) string {
	switch {
	case errors.Is(err, okf.ErrUnknownBundle):
		return msgVerifyBundle
	case errors.Is(err, okf.ErrUnknownConcept), errors.Is(err, okf.ErrOutsideBundle), errors.Is(err, okf.ErrSymlinkTarget):
		return msgVerifyConcept
	case errors.Is(err, okf.ErrInvalidReference):
		return msgVerifyNotConcept
	case errors.Is(err, okf.ErrChangedSincePreview):
		return fmt.Sprintf("Concept changed since the preview; review it again with /verify %s.", verifyRef(qualified))
	case errors.Is(err, okf.ErrUnmergeable):
		return msgVerifyUnmergeable
	}
	rt.logger.Warn().Err(err).Str("concept", qualified).Msg("verify command failed")

	return msgVerifyFailed
}

// verifyRef percent-encodes the concept part of a qualified id so the
// reference survives the command's whitespace splitting.
func verifyRef(qualified string) string {
	bundle, id, ok := strings.Cut(qualified, "/")
	if !ok {
		return qualified
	}

	return bundle + "/" + encodeRefPath(id)
}

func encodeRefPath(p string) string {
	var sb strings.Builder
	for _, b := range []byte(p) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '-', b == '.', b == '_', b == '~', b == '/':
			sb.WriteByte(b)
		default:
			fmt.Fprintf(&sb, "%%%02X", b)
		}
	}

	return sb.String()
}

// previewHeader is the trust summary lines of a preview.
func previewHeader(pv *okf.VerifyPreview) []string {
	gen := "absent"
	if pv.GenBy != "" || pv.GenAt != "" {
		gen = fmt.Sprintf("by %s at %s", orAbsent(pv.GenBy), orAbsent(pv.GenAt))
	}
	events := fmt.Sprintf("%d", pv.Events)
	if pv.Latest != "" {
		events += " (latest: " + pv.Latest + ")"
	}

	return []string{
		"Verify " + pv.Qualified,
		"Type: " + pv.Type,
		fmt.Sprintf("Trust: %s; status: %s", pv.Tier, pv.Status),
		"Generated: " + gen,
		"Verifications: " + events,
	}
}

func orAbsent(s string) string {
	if s == "" {
		return "absent"
	}

	return s
}

func contentLabel(pv *okf.VerifyPreview) string {
	if pv.Truncated {
		return fmt.Sprintf("Content (%d bytes; showing the first %d, the rest is cut):", pv.Size, len(pv.Content))
	}

	return fmt.Sprintf("Content (%d bytes):", pv.Size)
}

// fenceFor returns a backtick fence longer than any backtick run in s.
func fenceFor(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}

	return strings.Repeat("`", max(3, longest+1))
}

// sendVerifyPreview renders the preview verbatim: a fenced block on markdown
// channels, HTML-escaped <pre> chunks on Telegram. The raw content is never
// interpreted as markup.
func (rt *sessionRuntime) sendVerifyPreview(ctx context.Context, msg channel.Message, pv *okf.VerifyPreview, who string) {
	confirm := fmt.Sprintf("/verify %s %s %s", verifyRef(pv.Qualified), who, pv.Token)
	header := previewHeader(pv)

	if msg.ChannelName != telegram.ChannelName {
		fence := fenceFor(pv.Content)
		text := strings.Join(header, "\n") + "\n" + contentLabel(pv) + "\n" + fence + "\n" + pv.Content
		if !strings.HasSuffix(pv.Content, "\n") {
			text += "\n"
		}
		text += fence + "\n" + verifyConfirmLeadText + "\n" + confirm
		rt.sendFixed(ctx, msg, text)

		return
	}

	tg := msg
	tg.Metadata = make(map[string]string, len(msg.Metadata)+1)
	for k, v := range msg.Metadata {
		tg.Metadata[k] = v
	}
	tg.Metadata[telegram.MetadataPreformatted] = "1"

	rt.sendFixed(ctx, tg, html.EscapeString(strings.Join(header, "\n")+"\n"+contentLabel(pv)))
	for _, chunk := range splitRunes(pv.Content, verifyChunkRunes) {
		rt.sendFixed(ctx, tg, "<pre>"+html.EscapeString(chunk)+"</pre>")
	}
	rt.sendFixed(ctx, tg, html.EscapeString(verifyConfirmLeadText)+"\n<code>"+html.EscapeString(confirm)+"</code>")
}

// splitRunes cuts s into chunks of at most n runes, preferring a newline in
// the second half of the chunk.
func splitRunes(s string, n int) []string {
	if s == "" {
		return []string{""}
	}
	runes := []rune(s)
	var out []string
	for len(runes) > n {
		cut := n
		for i := n - 1; i >= n/2; i-- {
			if runes[i] == '\n' {
				cut = i + 1

				break
			}
		}
		out = append(out, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}

	return out
}
