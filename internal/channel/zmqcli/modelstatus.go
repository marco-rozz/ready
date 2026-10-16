package zmqcli

import (
	"github.com/marco-rozz/ready/internal/protocol"
)

// ModelStatusEvent is one observation of the daemon-global LLM model status
// (llm-status-visibility). Ref is the active `<provider>::<model>`
// reference; it is empty when no candidate is healthy. State encodes
// priority, not identity — the client renders `default` green, `fallback`
// yellow, and `unavailable` red, regardless of which provider is active.
type ModelStatusEvent struct {
	Ref   string
	State protocol.ModelState
}

// ModelStatusSource is the narrow orchestrator surface the channel consumes:
// the current status (reported in the authenticated handshake) plus a
// subscription to its transitions (broadcast as control events). cmd/serve.go
// adapts *orchestrator.Orchestrator to this interface so the channel package
// stays free of the orchestrator's dependency graph.
type ModelStatusSource interface {
	// ModelStatus returns the status to report in the next handshake.
	ModelStatus() ModelStatusEvent
	// SubscribeModelStatus returns a channel of subsequent transitions and a
	// cancel function that must be called to release the subscription.
	SubscribeModelStatus() (<-chan ModelStatusEvent, func())
}

// WatchModelStatus seeds the current model status from src and subscribes to
// its transitions: each event updates the stored status (reported by later
// handshakes) and is broadcast to the connected session as an unsolicited
// control response. Call once after NewChannel; the subscription goroutine
// runs until the channel's own context is cancelled (Close).
func (c *Channel) WatchModelStatus(src ModelStatusSource) {
	c.setModelStatus(src.ModelStatus())

	events, cancel := src.SubscribeModelStatus()

	go func() {
		defer cancel()

		for {
			select {
			case <-c.ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}

				c.setModelStatus(ev)
				c.broadcastModelStatus(ev)
			}
		}
	}()
}

// setModelStatus stores the latest status for handshake reporting.
func (c *Channel) setModelStatus(ev ModelStatusEvent) {
	c.modelMu.Lock()
	c.modelStatus = ev
	c.modelKnown = true
	c.modelMu.Unlock()
}

// currentModelStatus returns the stored status and whether one was ever set
// (a channel with no wired source reports no model fields, preserving the
// pre-change wire shape).
func (c *Channel) currentModelStatus() (ModelStatusEvent, bool) {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()

	return c.modelStatus, c.modelKnown
}

// broadcastModelStatus pushes one unsolicited control response carrying the
// updated model fields to the active session. Under the channel's
// single-session semantics "every connected session" is exactly the one
// authenticated session; with no session active the event is dropped — the
// next handshake reports the current status, so nothing is lost. The client
// DEALER socket tolerates unsolicited frames (progress notifications already
// ride the same path).
func (c *Channel) broadcastModelStatus(ev ModelStatusEvent) {
	sessionID, identity, _, _ := c.sess.snapshot()
	if sessionID == "" || len(identity) == 0 {
		return
	}

	resp, err := protocol.Marshal(protocol.Response{
		ConnectionID: sessionID,
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		ModelRef:     ev.Ref,
		ModelState:   ev.State,
	})
	if err != nil {
		c.logger.Error().Err(err).Msg("failed to marshal model-status event")

		return
	}

	if c.payloadsEnabled {
		c.logger.Debug().
			Str(ConnectionID, sessionID).
			Str("identity", shortIdentity(identity)).
			Str("model_ref", ev.Ref).
			Str("model_state", string(ev.State)).
			Msg("model_status_event")
	}

	if sendErr := c.listener.Send(identity, resp); sendErr != nil {
		c.logger.Warn().Err(sendErr).Str(ConnectionID, sessionID).Msg("failed to send model-status event")
	}
}
