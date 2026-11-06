package tools

import "errors"

// config-manager tool sentinels. cmd/serve.go registers them with the
// errdigest classifier: only positively not-committed, retryable I/O/lock
// failures are Transient; everything here is a Permanent refusal — retrying
// the same request cannot repair it.
var (
	// ErrConfigStagingBusy is refused when a foreign Session invokes any
	// action while another Session owns staging. The refusal carries no
	// owner details.
	ErrConfigStagingBusy = errors.New("config staging busy")
	// ErrConfigStaleState is returned for stage/revision mismatches and
	// unknown staging identity.
	ErrConfigStaleState = errors.New("config staging state mismatch")
	// ErrConfigNoSession is the fail-closed refusal when the invocation
	// context carries no trusted Session identity.
	ErrConfigNoSession = errors.New("config manager requires a trusted session identity")
	// ErrConfigNoStaging is refused when update/save/discard arrive before
	// any successful read.
	ErrConfigNoStaging = errors.New("no config staging is open")
	// ErrConfigInvalidEdit marks an unusable edit batch (empty or
	// ambiguous old_text, marker placement violations).
	ErrConfigInvalidEdit = errors.New("invalid config edit batch")
	// ErrConfigUnsupportedYAML marks anchors, aliases, merge keys,
	// unsupported scalar spans or a non-current config_version. This
	// restriction belongs to the tool, not to ordinary config loading.
	ErrConfigUnsupportedYAML = errors.New("unsupported YAML document for config-manager")
	// ErrConfigInvalidCandidate marks a final-state validation failure.
	ErrConfigInvalidCandidate = errors.New("invalid configuration candidate")
	// ErrConfigTransientIO wraps positively not-committed, retryable
	// I/O/lock failures (only these are classified TRANSIENT).
	ErrConfigTransientIO = errors.New("transient config manager I/O failure")
)
