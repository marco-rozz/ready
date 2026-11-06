package codex

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// The ChatGPT Codex backend delivers tool-call arguments differently from
// the public Responses API: on responses carrying a SINGLE function call it
// streams response.function_call_arguments.delta events (which the Eino
// adapter concatenates into the complete arguments), but on responses with
// PARALLEL calls it omits the deltas entirely and delivers the complete
// arguments only in the response.function_call_arguments.done and
// response.output_item.done events.  agenticopenai v0.2.4 maps neither into
// the aggregated message (the done handler drops item.Arguments), so the
// model receives tool calls with EMPTY arguments and every call fails with
// "unexpected end of JSON input" (observed live 2026-10-20, captured SSE in
// the package tests).
//
// sseArgsRepairer repairs this at the wire level: when an arguments-done
// (or item-done) event arrives for an item that never streamed argument
// deltas, it synthesizes the missing delta event carrying the complete
// arguments — which the adapter then concatenates like any other delta.
// Streams that behave like the public API pass through byte-identical.

type sseArgsRepairer struct {
	src  *bufio.Reader
	orig io.ReadCloser
	// sawDelta marks items whose argument deltas were observed or already
	// synthesized, keyed by the wire item_id.
	sawDelta map[string]bool
	pending  []byte
}

// newSSEArgsRepairer wraps an SSE response body so missing tool-call
// argument deltas are synthesized from the done events.  Only apply it to
// event-stream responses.
func newSSEArgsRepairer(body io.ReadCloser) *sseArgsRepairer {
	return &sseArgsRepairer{
		src:      bufio.NewReader(body),
		orig:     body,
		sawDelta: make(map[string]bool),
	}
}

// wrapIfSSE attaches the repairer when the response carries an SSE body.
// The ChatGPT Codex backend sends NO Content-Type header on its SSE streams
// (observed live 2026-10-20), so an absent type is treated as SSE too: a
// non-SSE body (e.g. a JSON error) passes through byte-identical anyway,
// because the repairer only touches lines starting with "data: ".
func wrapIfSSE(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "text/event-stream") {
		return
	}
	resp.Body = newSSEArgsRepairer(resp.Body)
}

type sseEvent struct {
	Type string `json:"type"`
	// function_call_arguments.{delta,done} fields.
	ItemID      string `json:"item_id"`
	OutputIndex *int64 `json:"output_index"`
	Delta       string `json:"delta"`
	Arguments   string `json:"arguments"`
	// output_item.done field.
	Item *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Arguments string `json:"arguments"`
	} `json:"item"`
}

func (s *sseArgsRepairer) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if err := s.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]

	return n, nil
}

// fill reads one line from the source, applies the repair rules, and queues
// the (possibly augmented) output.  A parse miss never fails the stream:
// unparseable lines pass through untouched.
func (s *sseArgsRepairer) fill() error {
	line, err := s.src.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return err // true EOF or transport error, nothing buffered
	}
	out := string(line)

	if data, ok := strings.CutPrefix(strings.TrimRight(out, "\r\n"), "data: "); ok {
		var ev sseEvent
		if jsonErr := json.Unmarshal([]byte(data), &ev); jsonErr == nil {
			switch ev.Type {
			case "response.function_call_arguments.delta":
				if ev.ItemID != "" {
					s.sawDelta[ev.ItemID] = true
				}
			case "response.function_call_arguments.done":
				// The done event always follows the deltas when they exist.
				// Without deltas this is the only delivery of the complete
				// arguments: synthesize the missing delta from it.
				if !s.sawDelta[ev.ItemID] && ev.ItemID != "" && ev.Arguments != "" {
					out = syntheticDeltaLine(ev.ItemID, ev.OutputIndex, ev.Arguments) + out
					s.sawDelta[ev.ItemID] = true
				} else if s.sawDelta[ev.ItemID] {
					s.sawDelta[ev.ItemID] = true
				}
			case "response.output_item.done":
				// Fallback for a backend variant that skips the
				// arguments.done event: the completed item itself carries
				// the full arguments.  Skipped when the item already saw
				// deltas or an injected one.
				if it := ev.Item; it != nil && it.Type == "function_call" &&
					it.ID != "" && it.Arguments != "" && !s.sawDelta[it.ID] {
					out = syntheticDeltaLine(it.ID, ev.OutputIndex, it.Arguments) + out
					s.sawDelta[it.ID] = true
				}
			}
		}
	}

	s.pending = append(s.pending, out...)

	return nil
}

func (s *sseArgsRepairer) Close() error {
	return s.orig.Close()
}

// syntheticDeltaLine renders the missing delta as both the SSE event line
// and the data line, mirroring the backend's framing.
func syntheticDeltaLine(itemID string, outputIndex *int64, args string) string {
	ev := struct {
		Type        string `json:"type"`
		ItemID      string `json:"item_id"`
		OutputIndex *int64 `json:"output_index,omitempty"`
		Delta       string `json:"delta"`
	}{
		Type:        "response.function_call_arguments.delta",
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Delta:       args,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return "" // unreachable for this struct; the caller passes the line through
	}

	return "event: response.function_call_arguments.delta\ndata: " + string(b) + "\n\n"
}
