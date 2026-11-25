package zmqcli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/marco-rozz/ready/internal/protocol"
)

// Channel.Message metadata keys carrying the Turn correlation of a final
// reply (turn-steering S14). The values of the id lists are JSON arrays so a
// client-minted id may contain any character.
const (
	// MetaTurnID is the daemon-minted Turn ID.
	MetaTurnID = "turn_id"
	// MetaTurnRequestIDs lists every `message` request id the Turn consumed,
	// in consumption order (opener first).
	MetaTurnRequestIDs = "turn_request_ids"
	// MetaTurnDroppedIDs lists the candidates a /stop dropped.
	MetaTurnDroppedIDs = "turn_dropped_request_ids"
)

// ackWait bounds how long a Turn start waits for the acknowledgement of its
// own opener to be written first (the worker can start a Turn before the
// intake goroutine reports the admission).
const ackWait = 2 * time.Second

// unansweredReq is one request of the Connection still waiting for its final
// reply. identity routes the reply; exec marks a one-shot scripted request.
type unansweredReq struct {
	requestID string
	identity  []byte
	exec      bool
}

// admitRequest registers an unanswered request, or reports the structured
// rejection: an `exec` may not overlap any unanswered request and nothing may
// overlap an unanswered `exec`; a duplicate request id is rejected too. It
// also decides whether the request becomes the current slot (the Turn
// opener): only when nothing else is unanswered.
func (c *Channel) admitRequest(connectionID string, identity []byte, requestID string, exec bool) (admitted bool) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	unans := c.unanswered[connectionID]

	for _, u := range unans {
		if u.requestID == requestID || exec || u.exec {
			return false
		}
	}

	c.unanswered[connectionID] = append(unans, unansweredReq{requestID: requestID, identity: identity, exec: exec})

	if len(unans) == 0 {
		c.pending[connectionID] = pendingEntry{identity: identity, requestID: requestID, activityKey: &activityKey{}}
	}

	if !exec {
		c.acks[ackKey(connectionID, requestID)] = make(chan struct{})
	}

	return true
}

func ackKey(connectionID, requestID string) string { return connectionID + "\x00" + requestID }

// hasPending reports whether the Connection has any unanswered request.
func (c *Channel) hasPending(connectionID string) bool {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	return c.hasPendingLocked(connectionID)
}

func (c *Channel) hasPendingLocked(connectionID string) bool {
	_, slot := c.pending[connectionID]

	return slot || len(c.unanswered[connectionID]) > 0
}

// clearPending forgets every unanswered request of a released Connection.
func (c *Channel) clearPending(connectionID string) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	delete(c.pending, connectionID)
	delete(c.unanswered, connectionID)

	for k, ch := range c.acks {
		if len(k) > len(connectionID) && k[:len(connectionID)+1] == connectionID+"\x00" {
			select {
			case <-ch:
			default:
				close(ch)
			}

			delete(c.acks, k)
		}
	}
}

// lookupUnanswered returns the unanswered request with the given id.
func (c *Channel) lookupUnansweredLocked(connectionID, requestID string) (unansweredReq, bool) {
	for _, u := range c.unanswered[connectionID] {
		if u.requestID == requestID {
			return u, true
		}
	}

	return unansweredReq{}, false
}

// consume removes the given ids from the Connection's unanswered set (the
// exactly-one-reply invariant: a request id is answered once).
func (c *Channel) consumeLocked(connectionID string, ids []string) {
	if len(ids) == 0 {
		return
	}

	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}

	kept := c.unanswered[connectionID][:0:0]
	for _, u := range c.unanswered[connectionID] {
		if !drop[u.requestID] {
			kept = append(kept, u)
		}
	}

	if len(kept) == 0 {
		delete(c.unanswered, connectionID)

		return
	}

	c.unanswered[connectionID] = kept
}

// decodeIDs parses a metadata id list; a malformed or empty value is nil.
func decodeIDs(v string) []string {
	if v == "" {
		return nil
	}

	var ids []string
	if err := json.Unmarshal([]byte(v), &ids); err != nil {
		return nil
	}

	return ids
}

// EncodeIDs renders an id list for the Turn metadata keys.
func EncodeIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}

	b, _ := json.Marshal(ids)

	return string(b)
}

// sendEvent writes one unsolicited control event to a Connection's peer. It
// never consumes a pending slot; a dropped peer is not an error worth failing
// a Turn for.
func (c *Channel) sendEvent(connectionID string, resp protocol.Response) error {
	activeID, identity, _, _ := c.sess.snapshot()
	if activeID != connectionID || len(identity) == 0 {
		return nil
	}

	resp.ConnectionID = connectionID
	resp.Status = protocol.ResponseStatusOK
	resp.ResponseType = protocol.ResponseTypeControl

	payload, err := protocol.Marshal(resp)
	if err != nil {
		return fmt.Errorf("zmqcli: marshal %s event: %w", resp.Content, err)
	}

	if sendErr := c.listener.Send(identity, payload); sendErr != nil {
		c.logger.Warn().Err(sendErr).Str(ConnectionID, connectionID).Str("event", resp.Content).Msg("failed to send turn event")

		return fmt.Errorf("zmqcli: send %s event: %w", resp.Content, sendErr)
	}

	return nil
}

// AcknowledgeMessage writes message_accepted for an admitted `message`
// request (disposition from the address inbox) and releases any Turn start
// waiting to follow it.
func (c *Channel) AcknowledgeMessage(connectionID, requestID, disposition, turnID string) {
	_ = c.sendEvent(connectionID, protocol.Response{
		RequestID: requestID,
		Content:   protocol.ControlMessageAccepted,
		Turn:      &protocol.TurnInfo{Disposition: disposition, TurnID: turnID},
	})

	c.pendingMu.Lock()
	ch, ok := c.acks[ackKey(connectionID, requestID)]
	if ok {
		delete(c.acks, ackKey(connectionID, requestID))
	}
	c.pendingMu.Unlock()

	if ok {
		close(ch)
	}
}

// awaitAck waits (bounded) until the acknowledgement of requestID was
// written, so turn_started never precedes message_accepted.
func (c *Channel) awaitAck(connectionID, requestID string) {
	c.pendingMu.Lock()
	ch, ok := c.acks[ackKey(connectionID, requestID)]
	c.pendingMu.Unlock()

	if !ok {
		return
	}

	select {
	case <-ch:
	case <-time.After(ackWait):
	}
}

// NotifyTurnStarted makes openerRequestID the Connection's current Turn slot
// (progress, activity and the final reply correlate to it) and writes
// turn_started. requestIDs are the opener followed by the pre-loaded
// candidates that belong to this Connection.
func (c *Channel) NotifyTurnStarted(connectionID, openerRequestID, turnID string, requestIDs []string, deferred bool) {
	if connectionID == "" {
		return
	}

	c.awaitAck(connectionID, openerRequestID)

	c.pendingMu.Lock()
	if u, ok := c.lookupUnansweredLocked(connectionID, openerRequestID); ok {
		if slot, has := c.pending[connectionID]; !has || slot.requestID != openerRequestID {
			c.pending[connectionID] = pendingEntry{identity: u.identity, requestID: openerRequestID, activityKey: &activityKey{}}
		}
	}
	c.pendingMu.Unlock()

	if openerRequestID == "" {
		return
	}

	_ = c.sendEvent(connectionID, protocol.Response{
		RequestID: openerRequestID,
		Content:   protocol.ControlTurnStarted,
		Turn:      &protocol.TurnInfo{TurnID: turnID, RequestIDs: requestIDs, Deferred: deferred},
	})
}

// NotifySteerInjected writes steer_injected for candidates claimed into a
// Turn: requestIDs are the claimed requests of this Connection in claim order.
func (c *Channel) NotifySteerInjected(connectionID, openerRequestID, turnID string, requestIDs []string, superseded int) {
	if connectionID == "" || len(requestIDs) == 0 {
		return
	}

	_ = c.sendEvent(connectionID, protocol.Response{
		RequestID: openerRequestID,
		Content:   protocol.ControlSteerInjected,
		Turn:      &protocol.TurnInfo{TurnID: turnID, RequestIDs: requestIDs, SupersededToolCalls: &superseded},
	})
}

// replyTurnInfo builds the `turn` object of a final reply from the message
// metadata: nil for a reply that belongs to no Turn (command replies, an
// exec), so those payloads stay as before.
func replyTurnInfo(meta map[string]string, consumed, dropped []string, opener string) *protocol.TurnInfo {
	turnID := meta[MetaTurnID]
	if turnID == "" && len(consumed) == 0 && len(dropped) == 0 {
		return nil
	}

	ids := consumed
	if len(ids) == 0 && opener != "" {
		ids = []string{opener}
	}

	return &protocol.TurnInfo{TurnID: turnID, RequestIDs: ids, DroppedRequestIDs: dropped}
}
