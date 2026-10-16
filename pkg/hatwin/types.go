package hatwin

// EntitySnapshot is the read-API view of a single entity returned by Filter
// and GetState. Attributes are included only when FilterQuery.Verbose is true.
type EntitySnapshot struct {
	EntityID     string         `json:"entity_id"`
	Domain       string         `json:"domain"`
	State        string         `json:"state"`
	DeviceClass  string         `json:"device_class,omitempty"`
	AreaID       string         `json:"area_id,omitempty"`
	AreaName     string         `json:"area_name,omitempty"`
	FriendlyName string         `json:"friendly_name,omitempty"`
	LastChanged  string         `json:"last_changed,omitempty"`
	LastUpdated  string         `json:"last_updated,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
}

// FilterQuery selects a subset of entities from the twin.
type FilterQuery struct {
	// Area filters by area_id (exact, case-insensitive) or area name (substring).
	Area string
	// Domain filters by HA domain prefix, e.g. "sensor" or "light".
	Domain string
	// DeviceClass filters by attributes.device_class.
	DeviceClass string
	// EntityID is a glob (supports * and ?) or substring pattern against entity_id.
	EntityID string
	// OnlyProblem returns only entities whose state is "unavailable" or "unknown".
	OnlyProblem bool
	// Verbose includes the full Attributes map in each EntitySnapshot.
	Verbose bool

	// State filters by entity state (D9): a string (exact; "on"/"off" are
	// resolved semantically per domain via the shared table), a []string/
	// []any (any of), or a map[string]any numeric comparison object
	// ({">": 25} with operators >, <, >=, <=). A non-numeric state never
	// matches a numeric comparison.
	State any
	// Attributes filters on raw attributes regardless of Verbose (D9):
	// name → exact value (string/bool/number) or numeric comparison object.
	// Entries are AND-composed; unsupported forms fail with ErrInvalidFilter.
	Attributes map[string]any
	// Name is a case- and diacritic-insensitive substring matched against
	// the friendly name (fallback: registry name).
	Name string
	// Sort is one of entity_id (default) | name | state | last_changed.
	Sort string
	// SortDesc flips the ordering to descending.
	SortDesc bool
}

// HAConfig holds selected HA global configuration. Sensitive fields (latitude,
// longitude, elevation) are intentionally excluded.
type HAConfig struct {
	Name         string `json:"name"`
	LocationName string `json:"location_name,omitempty"`
	TimeZone     string `json:"time_zone,omitempty"`
	Version      string `json:"version,omitempty"`
}

// TopologyChangedSignal is sent (debounced) on the channel returned by
// Twin.TopologyChanges after any registry-update event from HA.
type TopologyChangedSignal struct{}
