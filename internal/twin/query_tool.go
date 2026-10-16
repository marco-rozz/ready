//nolint:goconst
package twin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/pkg/hatwin"
)

// queryToolName is the ha_state_query tool's wire/display name (never
// prefixed — it belongs to the Twin, not to any MCP server).
const queryToolName = "ha_state_query"

// result-limit bounds (twin-native-actuation D9, adapter-only): default 50,
// maximum 200; the truncation count is exact.
const (
	queryDefaultLimit = 50
	queryMaxLimit     = 200
)

// QueryTool wraps hatwin.Twin.Filter as an Eino InvokableTool.
// When the twin is nil (HA disabled in config), it returns a "disabled" message.
type QueryTool struct {
	twin               *hatwin.Twin
	staleThreshold     time.Duration
	hardStaleThreshold time.Duration
}

var _ tool.InvokableTool = (*QueryTool)(nil)

// NewQueryTool creates the ha_state_query tool. t may be nil when
// home_assistant.enabled is false; the tool still registers and returns a
// descriptive message rather than failing at startup. staleThreshold and
// hardStaleThreshold drive the freshness fields in the response payload (see
// home_assistant.stale_threshold_states / hard_stale_threshold in config).
func NewQueryTool(t *hatwin.Twin, staleThreshold, hardStaleThreshold time.Duration) *QueryTool {
	return &QueryTool{twin: t, staleThreshold: staleThreshold, hardStaleThreshold: hardStaleThreshold}
}

// queryArgs is the LLM-facing argument set. state/attributes are `any`
// because they accept several shapes (exact value, array any-of, comparison
// object); sort accepts a string or {by, order}.
type queryArgs struct {
	Area        string          `json:"area,omitempty"`
	Domain      string          `json:"domain,omitempty"`
	DeviceClass string          `json:"device_class,omitempty"`
	EntityID    string          `json:"entity_id,omitempty"`
	OnlyProblem bool            `json:"only_problem,omitempty"`
	Verbose     bool            `json:"verbose,omitempty"`
	State       any             `json:"state,omitempty"`
	Attributes  map[string]any  `json:"attributes,omitempty"`
	Name        string          `json:"name,omitempty"`
	Sort        json.RawMessage `json:"sort,omitempty"`
	Limit       int             `json:"limit,omitempty"`
	GroupBy     string          `json:"group_by,omitempty"`
}

// queryResponse is the JSON payload returned to the LLM. Freshness fields are
// always populated (even on an empty result set) so the agent can reason
// about connection health independently of the query outcome.
type queryResponse struct {
	Results                  []hatwin.EntitySnapshot `json:"results"`
	TwinConnected            bool                    `json:"twin_connected"`
	LastWSActivityAgoSeconds int64                   `json:"last_ws_activity_ago_seconds"`
	LastStateEventAgoSeconds int64                   `json:"last_state_event_ago_seconds"`
	Stale                    bool                    `json:"stale"`
	// More is the exact count of omitted matches when the limit truncated.
	More int `json:"more,omitempty"`
	// Groups carries the group_by:area bucketing (area name → entities),
	// with area-less entities under "unassigned"; present only when
	// group_by=area. The limit applies to the total before grouping.
	Groups map[string][]hatwin.EntitySnapshot `json:"groups,omitempty"`
}

// staleErrorResponse is returned in place of queryResponse when the twin's
// data exceeds hardStaleThreshold.
type staleErrorResponse struct {
	Error              string `json:"error"`
	SecondsSinceLastWS int64  `json:"seconds_since_last_ws"`
}

func (t *QueryTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: queryToolName,
		Desc: "Query the Home Assistant digital twin for current entity states. " +
			"Use this for ALL state reads — do NOT call the MCP get_state tool. " +
			"Filter by area, domain, device_class, entity_id (glob or substring), " +
			"only_problem (unavailable/unknown), state value (exact, list, semantic " +
			"on/off — e.g. state=off on covers means closed — or numeric comparison " +
			`{\"state\": {\">\": 25}}), attribute value (equality or numeric comparison, ` +
			"e.g. attributes={\"hvac_action\": \"heating\"}), or friendly-name substring " +
			"(name, case- and accent-insensitive). Results are ordered (default " +
			"entity_id; sort by name, state or last_changed, ascending/descending), " +
			"capped at limit (default 50, max 200 — the response carries the exact " +
			"count of omitted matches) and can be grouped by area (group_by=area, " +
			"area-less entities under 'unassigned'). Verbose adds full attributes. " +
			"The response always carries twin_connected, last_ws_activity_ago_seconds, " +
			"last_state_event_ago_seconds, and stale — check these before trusting the data.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"area": {
				Type: schema.String,
				Desc: "Filter by area_id or area name (substring, case-insensitive). Empty = all areas.",
			},
			"domain": {
				Type: schema.String,
				Desc: "Filter by HA domain prefix, e.g. 'sensor', 'light', 'climate'. Empty = all.",
			},
			"device_class": {
				Type: schema.String,
				Desc: "Filter by device_class attribute, e.g. 'temperature', 'humidity'. Empty = all.",
			},
			"entity_id": {
				Type: schema.String,
				Desc: "Glob (supports * and ?) or substring pattern against entity_id, e.g. 'sensor.*temp*'.",
			},
			"only_problem": {
				Type: schema.Boolean,
				Desc: "When true, return only entities with state 'unavailable' or 'unknown'.",
			},
			"verbose": {
				Type: schema.Boolean,
				Desc: "When true, include full attributes for each entity. Default false (reduced projection).",
			},
			"state": {
				Type: schema.String,
				Desc: "State filter: exact value, JSON array (any of), 'on'/'off' (semantic per domain — e.g. state=off on covers matches closed), or a numeric comparison object like {\">\": 25} (operators >, <, >=, <=; non-numeric states never match).",
			},
			"attributes": {
				Type: schema.Object,
				Desc: "Attribute filter (evaluated on raw attributes, no verbose needed): name → exact value or numeric comparison object like {\">\": 25}. Multiple attributes AND-composed.",
			},
			"name": {
				Type: schema.String,
				Desc: "Friendly-name substring, case- and accent-insensitive (e.g. 'caffe' matches 'Macchina Caffè').",
			},
			"sort": {
				Type: schema.String,
				Desc: "Order: entity_id (default, ascending), name, state (numeric-aware — numbers before strings), or last_changed. Pass an object {\"by\": ..., \"order\": \"asc|desc\"} for descending.",
			},
			"limit": {
				Type: schema.Number,
				Desc: "Max results (default 50, max 200). When truncated, the response carries the exact count of omitted matches in 'more'.",
			},
			"group_by": {
				Type: schema.String,
				Desc: "Pass 'area' to bucket results per area name (area-less entities under 'unassigned'). The limit applies before grouping.",
			},
		}),
	}, nil
}

func (t *QueryTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	// criticalities-remediation (LOW lifecycle, ctx propagation): honour
	// cancellation before reading twin state.
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("ha_state_query: %w", err)
	}

	if t.twin == nil {
		return "Home Assistant digital twin is disabled (home_assistant.enabled: false in config). " +
			"Use the MCP get_state tool for state reads instead.", nil
	}

	var args queryArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("ha_state_query: invalid arguments: %w", err)
	}

	lastWS := t.twin.LastWSActivity()
	lastState := t.twin.LastStateEvent()

	var wsAgo int64
	if !lastWS.IsZero() {
		wsAgo = int64(time.Since(lastWS).Seconds())
	}

	if t.hardStaleThreshold > 0 && !lastWS.IsZero() && time.Since(lastWS) > t.hardStaleThreshold {
		out, err := json.Marshal(staleErrorResponse{Error: "twin_stale", SecondsSinceLastWS: wsAgo})
		if err != nil {
			return "", fmt.Errorf("ha_state_query: marshal stale response: %w", err)
		}

		return string(out), fmt.Errorf("%w: last WS activity %ds ago", ErrTwinStale, wsAgo)
	}

	sortBy, sortDesc, err := parseSort(args.Sort)
	if err != nil {
		return "", fmt.Errorf("ha_state_query: %w", err)
	}

	results, err := t.twin.Filter(hatwin.FilterQuery{
		Area:        args.Area,
		Domain:      args.Domain,
		DeviceClass: args.DeviceClass,
		EntityID:    args.EntityID,
		OnlyProblem: args.OnlyProblem,
		Verbose:     args.Verbose,
		State:       args.State,
		Attributes:  args.Attributes,
		Name:        args.Name,
		Sort:        sortBy,
		SortDesc:    sortDesc,
	})
	if errors.Is(err, hatwin.ErrNotReady) {
		return "Home Assistant digital twin not ready yet — initial sync still in progress. Retry shortly.", nil
	}
	if err != nil {
		return "", fmt.Errorf("ha_state_query: %w", err)
	}

	// Bounded limit with an exact truncation count (D9, adapter-only).
	limit := args.Limit
	if limit <= 0 {
		limit = queryDefaultLimit
	}
	if limit > queryMaxLimit {
		limit = queryMaxLimit
	}
	more := len(results) - limit
	if more < 0 {
		more = 0
	}
	if more > 0 {
		results = results[:limit]
	}

	var stateAgo int64
	if !lastState.IsZero() {
		stateAgo = int64(time.Since(lastState).Seconds())
	}

	resp := queryResponse{
		Results:                  results,
		TwinConnected:            !lastWS.IsZero(),
		LastWSActivityAgoSeconds: wsAgo,
		LastStateEventAgoSeconds: stateAgo,
		Stale:                    t.staleThreshold > 0 && !lastWS.IsZero() && time.Since(lastWS) > t.staleThreshold,
		More:                     more,
	}

	if args.GroupBy != "" {
		if args.GroupBy != "area" {
			return "", fmt.Errorf("ha_state_query: group_by must be 'area' (got %q)", args.GroupBy)
		}
		resp.Groups = groupByArea(results)
		// Grouped responses carry the buckets, not the flat list.
		resp.Results = nil
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return "", fmt.Errorf("ha_state_query: marshal results: %w", err)
	}

	return string(out), nil
}

// parseSort accepts "by" or {"by": ..., "order": "asc|desc"}.
func parseSort(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, false, nil
	}

	var obj struct {
		By    string `json:"by"`
		Order string `json:"order"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", false, fmt.Errorf("invalid sort: pass \"entity_id\", \"name\", \"state\", \"last_changed\" or {\"by\": ..., \"order\": \"asc|desc\"}")
	}

	return obj.By, obj.Order == "desc", nil
}

// groupByArea buckets entities by area name; area-less entities go under
// "unassigned". Input must already be ordered (deterministic buckets).
func groupByArea(results []hatwin.EntitySnapshot) map[string][]hatwin.EntitySnapshot {
	groups := make(map[string][]hatwin.EntitySnapshot)
	for _, snap := range results {
		area := snap.AreaName
		if area == "" {
			if snap.AreaID != "" {
				area = snap.AreaID
			} else {
				area = "unassigned"
			}
		}
		groups[area] = append(groups[area], snap)
	}

	return groups
}
