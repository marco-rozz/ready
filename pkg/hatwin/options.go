package hatwin

import (
	"time"

	"github.com/rs/zerolog"
)

// Options configures the HA WebSocket digital twin.
// Exactly one of Token/TokenFile must be set.
type Options struct {
	URL       string
	Token     string
	TokenFile string

	PingInterval     time.Duration
	ReadLimit        int64
	InitialBackoff   time.Duration
	MaxBackoff       time.Duration
	BackoffFactor    float64
	MaxRetries       int
	HandshakeTimeout time.Duration
	TopologyDebounce time.Duration

	// OutstandingPingsThreshold is the number of unacknowledged pings that
	// triggers a force-close of the WS to break out of a zombie connection
	// (TCP half-open). Default: 2.
	OutstandingPingsThreshold int
	// StaleThresholdStates is how long since the last WS activity before
	// callers should treat twin data as soft-stale. Default: 60s. The Twin
	// itself does not act on this — it only stores the value for
	// LastWSActivity-based freshness computation by callers.
	StaleThresholdStates time.Duration
	// HardStaleThreshold is how long since the last WS activity before
	// callers should refuse to serve twin data. Default: 300s.
	HardStaleThreshold time.Duration

	// ActiveCommandDomains selects the domains the intent vocabulary serves
	// (home_assistant.commands.domains, D5). nil (absent) ⇒ every table
	// domain except siren; a non-nil slice is taken literally (an empty
	// slice activates none — the tool then refuses everything).
	ActiveCommandDomains []string

	Logger zerolog.Logger
}

func (o *Options) applyDefaults() {
	if o.PingInterval <= 0 {
		o.PingInterval = 30 * time.Second
	}
	if o.ReadLimit <= 0 {
		o.ReadLimit = 32 << 20
	}
	if o.InitialBackoff <= 0 {
		o.InitialBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 60 * time.Second
	}
	if o.BackoffFactor <= 1 {
		o.BackoffFactor = 2.0
	}
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = 10 * time.Second
	}
	if o.TopologyDebounce <= 0 {
		o.TopologyDebounce = 500 * time.Millisecond
	}
	if o.OutstandingPingsThreshold <= 0 {
		o.OutstandingPingsThreshold = 2
	}
	if o.StaleThresholdStates <= 0 {
		o.StaleThresholdStates = 60 * time.Second
	}
	if o.HardStaleThreshold <= 0 {
		o.HardStaleThreshold = 300 * time.Second
	}
}
