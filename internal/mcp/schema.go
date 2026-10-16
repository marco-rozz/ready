package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/rs/zerolog"
)

// maxSchemaDepth bounds recursion into nested object/array schemas. Beyond
// this depth the sub-schema is degraded to schema.String with a note in Desc
// instead of being expanded further. This also acts as the sole defence
// against cyclical schemas (see convertSchema's $ref handling for the direct
// self-reference case): there is no separate visited-node tracking, so a
// schema that is deeply nested but legitimate degrades identically to one
// that is cyclical. Five levels comfortably covers real-world MCP servers
// (including Home Assistant) while bounding worst-case recursion.
const maxSchemaDepth = 5

// JSON Schema "type" keyword values, expressed as constants derived from
// Eino's DataType so the same literal isn't repeated across this file, the
// legacy fallback, the mock server, and tests (which live in this package).
const (
	jsonTypeString  = string(schema.String)
	jsonTypeNumber  = string(schema.Number)
	jsonTypeInteger = string(schema.Integer)
	jsonTypeBoolean = string(schema.Boolean)
	jsonTypeArray   = string(schema.Array)
	jsonTypeObject  = string(schema.Object)

	jsonKeyType        = "type"
	jsonKeyFormat      = "format"
	jsonKeyProperties  = "properties"
	jsonKeyDescription = "description"
	jsonKeyEnum        = "enum"
	jsonKeyItems       = "items"
	jsonKeyAnyOf       = "anyOf"
	jsonKeyOneOf       = "oneOf"
)

// convertMCPSchemaToParams converts an MCP tool's JSON Schema into Eino's
// map[string]*schema.ParameterInfo representation, preserving enum values,
// array element types, nested objects, and numeric/format constraints that
// ParameterInfo cannot represent structurally (folded into Desc instead).
//
// If the MCP schema cannot be unmarshalled into a typed *jsonschema.Schema,
// the conversion falls back to convertMCPSchemaToParamsLegacy and emits a
// single DEBUG log line naming the tool: a tool with an imprecise schema is
// preferable to a missing tool.
func convertMCPSchemaToParams(logger zerolog.Logger, toolName string, inputSchema any) map[string]*schema.ParameterInfo {
	if len(schemaProperties(inputSchema)) == 0 {
		return nil
	}

	root, err := unmarshalToolSchema(inputSchema)
	if err != nil {
		logger.Debug().Str("tool", toolName).Str("error", err.Error()).
			Msg("MCP tool schema could not be parsed as JSON Schema; falling back to legacy conversion")

		return convertMCPSchemaToParamsLegacy(inputSchema)
	}

	return convertObjectProperties(root, 1)
}

// unmarshalToolSchema re-encodes an mcp.Tool.InputSchema (declared `any` by
// the official SDK and observed client-side as untyped map[string]any) as
// JSON and decodes it into a *jsonschema.Schema with fully typed fields. This
// round-trip runs once per tool at discovery time, never on the CallTool hot
// path.
func unmarshalToolSchema(inputSchema any) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(inputSchema)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal MCP input schema: %w", err)
	}

	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("failed to unmarshal MCP input schema as JSON Schema: %w", err)
	}

	return &s, nil
}

// schemaProperties extracts the "properties" map from an untyped MCP input
// schema (map[string]any), returning nil when the schema is not a map or
// declares no properties. Centralises the shape check shared by the
// fidelity-preserving and legacy conversion paths.
func schemaProperties(inputSchema any) map[string]any {
	m, ok := inputSchema.(map[string]any)
	if !ok {
		return nil
	}

	props, ok := m[jsonKeyProperties].(map[string]any)
	if !ok {
		return nil
	}

	return props
}

// schemaRequired extracts the "required" array from an untyped MCP input
// schema (map[string]any) as a set, tolerating a missing or malformed field.
func schemaRequired(inputSchema any) map[string]bool {
	m, ok := inputSchema.(map[string]any)
	if !ok {
		return nil
	}

	reqAny, ok := m["required"].([]any)
	if !ok {
		return nil
	}

	requiredSet := make(map[string]bool, len(reqAny))
	for _, r := range reqAny {
		if s, ok := r.(string); ok {
			requiredSet[s] = true
		}
	}

	return requiredSet
}

// convertObjectProperties converts every property of an object schema into a
// ParameterInfo map, deriving each sub-parameter's Required flag from this
// object's own "required" array (never from an enclosing level). Returns nil
// when the object declares no properties (either a leaf scalar owner or a
// free-form/open object), matching the "no properties" contract at every
// nesting level.
func convertObjectProperties(s *jsonschema.Schema, depth int) map[string]*schema.ParameterInfo {
	if len(s.Properties) == 0 {
		return nil
	}

	requiredSet := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		requiredSet[r] = true
	}

	params := make(map[string]*schema.ParameterInfo, len(s.Properties))
	for name, prop := range s.Properties {
		paramInfo := convertSchema(prop, depth)
		paramInfo.Required = requiredSet[name]
		params[name] = paramInfo
	}

	return params
}

// convertSchema recursively converts a single JSON Schema node into a
// ParameterInfo. It never panics and never recurses beyond maxSchemaDepth:
// constructs it cannot represent faithfully (excess depth, unresolved $ref,
// anyOf/oneOf composition) degrade to schema.String with an explanatory note
// appended to Desc rather than causing an error.
func convertSchema(s *jsonschema.Schema, depth int) *schema.ParameterInfo {
	paramInfo := &schema.ParameterInfo{}
	desc := s.Description
	var notes []string

	switch {
	case depth > maxSchemaDepth:
		paramInfo.Type = schema.String
		paramInfo.Desc = composeDesc(desc, []string{"(schema nested beyond the supported depth; treated as an opaque string)"})

		return paramInfo
	case s.Ref != "":
		paramInfo.Type = schema.String
		paramInfo.Desc = composeDesc(desc, []string{"(schema uses an unresolved $ref, possibly self-referential; treated as an opaque string)"})

		return paramInfo
	case nullableAlternative(s) != nil:
		// Pydantic/FastMCP commonly represents T | None as anyOf/oneOf with
		// one concrete branch and one null branch. ParameterInfo cannot model
		// nullability directly, but it can and should preserve T's real shape.
		alternative := *nullableAlternative(s)
		if alternative.Description == "" {
			alternative.Description = desc
		}
		paramInfo = convertSchema(&alternative, depth)
		paramInfo.Desc = composeDesc(paramInfo.Desc, []string{"(null is also permitted)"})

		return paramInfo
	case len(s.AnyOf) > 0 || len(s.OneOf) > 0:
		paramInfo.Type = schema.String
		paramInfo.Desc = composeDesc(desc, []string{"(accepts alternative shapes via anyOf/oneOf, not representable here; treated as an opaque string)"})

		return paramInfo
	}

	typ, nullNote := resolveType(s)
	if nullNote != "" {
		notes = append(notes, nullNote)
	}

	switch schema.DataType(typ) {
	case schema.String, schema.Number, schema.Integer, schema.Boolean, schema.Array, schema.Object, schema.Null:
		paramInfo.Type = schema.DataType(typ)
	default:
		paramInfo.Type = schema.String
		if typ != "" {
			notes = append(notes, fmt.Sprintf("(unrecognized schema type %q treated as string)", typ))
		}
	}

	if len(s.Enum) > 0 {
		if enumStrs, ok := stringEnum(s.Enum); ok {
			paramInfo.Enum = enumStrs
		} else {
			notes = append(notes, formatAllowedValues(s.Enum))
		}
	}

	notes = appendConstraintNotes(notes, s)

	switch paramInfo.Type {
	case schema.Array:
		convertArrayItems(paramInfo, s, depth)
		if len(s.ItemsArray) > 0 || len(s.PrefixItems) > 0 {
			notes = append(notes, "(elements have heterogeneous positional types via tuple-form items; not representable as a single element type)")
		}
	case schema.Object:
		paramInfo.SubParams = convertObjectProperties(s, depth+1)
	}

	paramInfo.Desc = composeDesc(desc, notes)

	return paramInfo
}

// nullableAlternative returns the sole non-null branch from an anyOf/oneOf
// composition that contains exactly one concrete schema and one null schema.
// More complex unions remain unrepresentable and follow the normal
// degradation path in convertSchema.
func nullableAlternative(s *jsonschema.Schema) *jsonschema.Schema {
	alternatives := s.AnyOf
	if len(alternatives) == 0 {
		alternatives = s.OneOf
	}
	if len(alternatives) != 2 {
		return nil
	}

	var concrete *jsonschema.Schema
	var nulls int
	for _, alternative := range alternatives {
		if alternative == nil {
			return nil
		}
		if alternative.Type == string(schema.Null) ||
			(len(alternative.Types) == 1 && alternative.Types[0] == string(schema.Null)) {
			nulls++

			continue
		}
		if concrete != nil {
			return nil
		}
		concrete = alternative
	}

	if nulls != 1 {
		return nil
	}

	return concrete
}

// resolveType picks the effective JSON Schema type for s. When s declares a
// type union (Types), the first non-"null" entry is used and, if "null" was
// among the alternatives, a note reporting that is returned so the caller can
// fold it into Desc.
func resolveType(s *jsonschema.Schema) (typ, nullNote string) {
	if s.Type != "" {
		return s.Type, ""
	}

	if len(s.Types) == 0 {
		return "", ""
	}

	var hasNull bool
	for _, t := range s.Types {
		if t == string(schema.Null) {
			hasNull = true

			continue
		}
		if typ == "" {
			typ = t
		}
	}

	if hasNull {
		nullNote = "(null is also permitted)"
	}

	return typ, nullNote
}

// convertArrayItems populates ElemInfo from a single-schema "items" keyword.
// Tuple-form items (ItemsArray/PrefixItems) and the no-items case both leave
// ElemInfo nil without error; the caller appends a note for the tuple case.
func convertArrayItems(paramInfo *schema.ParameterInfo, s *jsonschema.Schema, depth int) {
	if s.Items == nil {
		return
	}

	paramInfo.ElemInfo = convertSchema(s.Items, depth+1)
}

// appendConstraintNotes folds every JSON Schema constraint that
// schema.ParameterInfo cannot represent structurally into readable notes:
// numeric range (minimum/maximum/exclusiveMinimum/exclusiveMaximum), pattern,
// format, default, and array item-count bounds (minItems/maxItems).
func appendConstraintNotes(notes []string, s *jsonschema.Schema) []string {
	if r := formatRange(s.Minimum, s.Maximum, s.ExclusiveMinimum, s.ExclusiveMaximum); r != "" {
		notes = append(notes, r)
	}

	if s.Pattern != "" {
		notes = append(notes, fmt.Sprintf("(pattern: %s)", s.Pattern))
	}

	if s.Format != "" {
		notes = append(notes, fmt.Sprintf("(format: %s)", s.Format))
	}

	if len(s.Default) > 0 {
		notes = append(notes, fmt.Sprintf("(default: %s)", formatRawJSON(s.Default)))
	}

	if r := formatItemBounds(s.MinItems, s.MaxItems); r != "" {
		notes = append(notes, r)
	}

	return notes
}

// composeDesc is the single point of truth for how the original description
// and any degradation/constraint notes are combined. When there are no notes
// the result is byte-identical to base, with no trailing whitespace or
// parentheses, so a schema with only type+description round-trips exactly.
func composeDesc(base string, notes []string) string {
	result := base
	for _, n := range notes {
		if n == "" {
			continue
		}
		if result == "" {
			result = n
		} else {
			result += " " + n
		}
	}

	return result
}

// stringEnum reports whether every value in vals is a JSON string, returning
// the converted []string when so. ParameterInfo.Enum is typed []string, so a
// non-string (or mixed) enum must be rendered elsewhere (see
// formatAllowedValues) instead of being silently dropped.
func stringEnum(vals []any) ([]string, bool) {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}

	return out, true
}

// formatAllowedValues renders a non-string-only enum as a readable,
// comma-separated note appended to Desc.
func formatAllowedValues(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprintf("%v", v)
	}

	return fmt.Sprintf("(allowed values: %s)", joinComma(parts))
}

// formatRange renders the numeric bounds of a schema as "(range: lo..hi)".
// A missing bound is rendered as -inf/+inf so the note stays legible when
// only one side is constrained. Returns "" when no bound is set.
func formatRange(minimum, maximum, exclMin, exclMax *float64) string {
	lower, upper := "", ""

	switch {
	case minimum != nil:
		lower = fmt.Sprintf("%v", *minimum)
	case exclMin != nil:
		lower = fmt.Sprintf(">%v", *exclMin)
	}

	switch {
	case maximum != nil:
		upper = fmt.Sprintf("%v", *maximum)
	case exclMax != nil:
		upper = fmt.Sprintf("<%v", *exclMax)
	}

	if lower == "" && upper == "" {
		return ""
	}
	if lower == "" {
		lower = "-inf"
	}
	if upper == "" {
		upper = "+inf"
	}

	return fmt.Sprintf("(range: %s..%s)", lower, upper)
}

// formatItemBounds renders minItems/maxItems as "(items: lo..hi)", mirroring
// formatRange's missing-bound convention.
func formatItemBounds(minItems, maxItems *int) string {
	lower, upper := "", ""
	if minItems != nil {
		lower = fmt.Sprintf("%d", *minItems)
	}
	if maxItems != nil {
		upper = fmt.Sprintf("%d", *maxItems)
	}

	if lower == "" && upper == "" {
		return ""
	}
	if lower == "" {
		lower = "-inf"
	}
	if upper == "" {
		upper = "+inf"
	}

	return fmt.Sprintf("(items: %s..%s)", lower, upper)
}

// formatRawJSON renders a json.RawMessage default value without surrounding
// quotes for strings, e.g. `"heat"` becomes `heat` and `21` stays `21`.
func formatRawJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}

	return fmt.Sprintf("%v", v)
}

// joinComma joins parts with ", " without pulling in strings.Join for a
// single call site.
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}

	return out
}

// convertMCPSchemaToParamsLegacy is the pre-fidelity conversion, preserved
// verbatim as the fallback when a tool's schema cannot be unmarshalled into a
// typed *jsonschema.Schema. It reads only the top-level "type" and
// "description" keys from the untyped property map; enum, items, nested
// objects and constraints are not recognised. See D6 in the design: a tool
// with an imprecise schema must still be registered.
func convertMCPSchemaToParamsLegacy(inputSchema any) map[string]*schema.ParameterInfo {
	properties := schemaProperties(inputSchema)
	if len(properties) == 0 {
		return nil
	}

	params := make(map[string]*schema.ParameterInfo, len(properties))

	requiredSet := schemaRequired(inputSchema)

	for name, prop := range properties {
		paramInfo := &schema.ParameterInfo{
			Required: requiredSet[name],
		}

		if propMap, ok := prop.(map[string]any); ok {
			if typeVal, ok := propMap["type"].(string); ok {
				switch typeVal {
				case jsonTypeString:
					paramInfo.Type = schema.String
				case jsonTypeNumber, jsonTypeInteger:
					paramInfo.Type = schema.Number
				case jsonTypeBoolean:
					paramInfo.Type = schema.Boolean
				case jsonTypeArray:
					paramInfo.Type = schema.Array
				default:
					paramInfo.Type = schema.String
				}
			}

			if desc, ok := propMap["description"].(string); ok {
				paramInfo.Desc = desc
			}
		}

		params[name] = paramInfo
	}

	return params
}
