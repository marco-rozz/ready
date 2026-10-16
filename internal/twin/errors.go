package twin

import "errors"

// ErrTwinStale is returned by QueryTool when the Twin's last WS activity
// exceeds the configured hard_stale_threshold. Callers (tests, and the tool's
// own InvokableRun) can match it with errors.Is; the LLM-facing message
// carries the same information in structured JSON form.
var ErrTwinStale = errors.New("hatwin: digital twin data is stale beyond the hard stale threshold")
