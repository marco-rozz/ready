package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/tools"
)

// noticeDeliveryTimeout bounds one Mid-turn Notice delivery (say-mid-turn-
// notice D7). Fixed and non-configurable in production; a var only so tests
// can shorten it.
//
//nolint:gochecknoglobals // test seam for a fixed product constant
var noticeDeliveryTimeout = 15 * time.Second

// noticeLocks serialises the Notice deliveries of one Conversation address:
// one Notice at a time, parts of different Notices never interleave.
type noticeLocks struct {
	m sync.Map // session.Address -> *sync.Mutex
}

func (l *noticeLocks) lock(addr session.Address) func() {
	v, _ := l.m.LoadOrStore(addr, &sync.Mutex{})
	mu, _ := v.(*sync.Mutex)

	mu.Lock()

	return mu.Unlock
}

// newNoticeDeliverer builds the Turn-scoped deliverer behind the say tool
// (say-mid-turn-notice D2/D6/D7). It applies the daemon's privacy redaction,
// serialises per address, and delivers on a context detached from Turn
// cancellation — a /stop, steering preemption or the agent-run deadline never
// cut a Notice in half — bounded by noticeDeliveryTimeout and by daemon
// shutdown (base is the worker context, cancelled only at shutdown).
// Notice text is never logged.
func (rt *sessionRuntime) newNoticeDeliverer(base context.Context, addr session.Address, msg channel.Message, logger zerolog.Logger) tools.NoticeDeliverer {
	return func(_ context.Context, text string) (channel.NoticeResult, error) {
		text = rt.sanitizer.Redact(text)

		unlock := rt.notices.lock(addr)
		defer unlock()

		ch := rt.mgr.Channel(msg.ChannelName)

		sender, ok := ch.(channel.NoticeSender)
		if !ok {
			return channel.NoticeResult{Outcome: channel.NoticeNotShown, Reason: channel.NoticeReasonChannelUnsupported}, nil
		}

		dctx, cancel := context.WithTimeout(base, noticeDeliveryTimeout)
		defer cancel()

		res, err := sender.SendNotice(dctx, msg.Metadata, text)
		if err != nil {
			logger.Warn().Err(err).Str("channel", msg.ChannelName).Str("address", string(addr)).Msg("mid-turn notice delivery failed")

			return channel.NoticeResult{}, fmt.Errorf("notice delivery on %s: %w", msg.ChannelName, err)
		}

		return res, nil
	}
}
