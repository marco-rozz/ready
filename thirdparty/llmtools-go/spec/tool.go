package spec

import (
	"context"
	"encoding/json"
	"time"
)

const (
	JSONEncoding = "json"
	TextEncoding = "text"

	// SchemaVersion  - Current  schema version.
	SchemaVersion = "2026-01-01"
	VersionOne    = "v1.0.0"
)

var SchemaStartTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

type (
	JSONRawString string
	JSONSchema    json.RawMessage
	FuncID        string
)

// ToolFunc is the low-level function signature stored in the registry.
// It receives JSON-encoded args and returns one or more tool outputs.
type ToolFunc func(ctx context.Context, in json.RawMessage) ([]ToolOutputUnion, error)

// GoToolImpl - Register-by-name pattern for Go tools.
type GoToolImpl struct {
	// Fully-qualified registration key, e.g.
	//   "github.com/acme/flexigpt/tools.Weather"
	FuncID FuncID `json:"funcID" validate:"required"`
}

type Tool struct {
	SchemaVersion string `json:"schemaVersion"`
	ID            string `json:"id"` // UUID-v7
	Slug          string `json:"slug"`
	Version       string `json:"version"` // opaque
	DisplayName   string `json:"displayName"`
	Description   string `json:"description"`

	// ArgSchema describes the JSON arguments that are passed when the tool is invoked.
	ArgSchema JSONSchema `json:"argSchema"`
	GoImpl    GoToolImpl `json:"goImpl"`

	CreatedAt  time.Time `json:"createdAt"`
	ModifiedAt time.Time `json:"modifiedAt"`

	Tags []string `json:"tags,omitempty"`
}
