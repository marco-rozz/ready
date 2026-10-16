package mcp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"
)

// Test-local literals shared across cases below, factored out to avoid
// repeated string literals (goconst) and to give the intent a name.
const (
	descTheEntity  = "The entity"
	propLevel      = "level"
	propBrightness = attrBrightness
)

// prop is a shorthand for building a raw JSON-Schema-shaped map, matching
// what Tool.InputSchema looks like client-side after the official SDK
// unmarshals a real wire response (map[string]any, not a typed struct).
func prop(fields map[string]any) map[string]any {
	return fields
}

// schemaOf builds a top-level "type: object" input schema as the untyped
// map[string]any shape the official SDK hands to convertMCPSchemaToParams.
func schemaOf(properties map[string]any, required ...string) any {
	return objectProp(properties, required...)
}

// objectProp builds a "type: object" property schema, optionally with its
// own "required" array, avoiding repeated "properties"/"object" literals.
func objectProp(properties map[string]any, required ...string) map[string]any {
	m := map[string]any{jsonKeyType: jsonTypeObject, jsonKeyProperties: properties}
	if len(required) > 0 {
		reqAny := make([]any, len(required))
		for i, r := range required {
			reqAny[i] = r
		}
		m["required"] = reqAny
	}

	return m
}

// buildNestedObjectSchema returns a "type: object" schema nested depth
// levels deep, with leafType at the bottom. Used to probe the maxSchemaDepth
// degradation boundary without any interface{} type assertions.
func buildNestedObjectSchema(depth int, leafType string) map[string]any {
	node := map[string]any{jsonKeyType: leafType}
	for range depth {
		node = objectProp(map[string]any{"next": node})
	}

	return node
}

func convert(t *testing.T, input any) map[string]*schema.ParameterInfo {
	t.Helper()

	return convertMCPSchemaToParams(discardLogger(), "test-tool", input)
}

func TestConvertMCPSchemaToParams_Scalars(t *testing.T) {
	params := convert(t, schemaOf(map[string]any{
		attrEntityID:   prop(map[string]any{jsonKeyType: jsonTypeString, jsonKeyDescription: descTheEntity}),
		propBrightness: prop(map[string]any{jsonKeyType: jsonTypeInteger}),
		propLevel:      prop(map[string]any{jsonKeyType: jsonTypeNumber}),
		"on":           prop(map[string]any{jsonKeyType: jsonTypeBoolean}),
	}, attrEntityID))

	cases := map[string]struct {
		typ      schema.DataType
		desc     string
		required bool
	}{
		attrEntityID:   {schema.String, descTheEntity, true},
		propBrightness: {schema.Integer, "", false},
		propLevel:      {schema.Number, "", false},
		"on":           {schema.Boolean, "", false},
	}

	for name, want := range cases {
		got, ok := params[name]
		if !ok {
			t.Fatalf("missing param %q", name)
		}
		if got.Type != want.typ {
			t.Errorf("%s: Type = %v, want %v", name, got.Type, want.typ)
		}
		if got.Desc != want.desc {
			t.Errorf("%s: Desc = %q, want %q", name, got.Desc, want.desc)
		}
		if got.Required != want.required {
			t.Errorf("%s: Required = %v, want %v", name, got.Required, want.required)
		}
	}
}

func TestConvertMCPSchemaToParams_NoProperties(t *testing.T) {
	if got := convert(t, map[string]any{jsonKeyType: jsonTypeObject}); got != nil {
		t.Errorf("expected nil for empty schema, got %v", got)
	}

	if got := convert(t, schemaOf(map[string]any{})); got != nil {
		t.Errorf("expected nil for empty properties map, got %v", got)
	}
}

func TestConvertMCPSchemaToParams_Enum(t *testing.T) {
	t.Run("string enum", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			attrHvacMode: prop(map[string]any{jsonKeyType: jsonTypeString, jsonKeyEnum: []any{hvacModeHeat, hvacModeCool, stateOff}}),
		}))
		p := params[attrHvacMode]
		if p.Type != schema.String {
			t.Fatalf("Type = %v, want String", p.Type)
		}
		want := []string{hvacModeHeat, hvacModeCool, stateOff}
		if len(p.Enum) != len(want) {
			t.Fatalf("Enum = %v, want %v", p.Enum, want)
		}
		for i, v := range want {
			if p.Enum[i] != v {
				t.Errorf("Enum[%d] = %q, want %q", i, p.Enum[i], v)
			}
		}
		if p.Desc != "" {
			t.Errorf("Desc = %q, want empty", p.Desc)
		}
	})

	t.Run("numeric enum", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			propLevel: prop(map[string]any{jsonKeyType: jsonTypeInteger, jsonKeyEnum: []any{1.0, 2.0, 3.0}}),
		}))
		p := params[propLevel]
		if len(p.Enum) != 0 {
			t.Errorf("Enum should stay empty for numeric enum, got %v", p.Enum)
		}
		if !strings.Contains(p.Desc, "allowed values: 1, 2, 3") {
			t.Errorf("Desc = %q, want allowed values note", p.Desc)
		}
	})

	t.Run("mixed enum", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"weird": prop(map[string]any{jsonKeyEnum: []any{"auto", 0.0, true}}),
		}))
		p := params["weird"]
		if len(p.Enum) != 0 {
			t.Errorf("Enum should stay empty for mixed enum, got %v", p.Enum)
		}
		if !strings.Contains(p.Desc, "auto") || !strings.Contains(p.Desc, "0") || !strings.Contains(p.Desc, "true") {
			t.Errorf("Desc = %q, want all three values", p.Desc)
		}
	})

	t.Run("enum with pre-existing description", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			propLevel: prop(map[string]any{
				jsonKeyType:        jsonTypeInteger,
				jsonKeyDescription: "The fan level",
				jsonKeyEnum:        []any{1.0, 2.0},
			}),
		}))
		p := params[propLevel]
		if !strings.HasPrefix(p.Desc, "The fan level ") {
			t.Errorf("Desc = %q, want description preserved as prefix", p.Desc)
		}
		if !strings.Contains(p.Desc, "allowed values: 1, 2") {
			t.Errorf("Desc = %q, want allowed values note appended", p.Desc)
		}
	})
}

func TestConvertMCPSchemaToParams_Arrays(t *testing.T) {
	t.Run("array of scalars", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"tags": prop(map[string]any{jsonKeyType: jsonTypeArray, jsonKeyItems: map[string]any{jsonKeyType: jsonTypeString}}),
		}))
		p := params["tags"]
		if p.Type != schema.Array {
			t.Fatalf("Type = %v, want Array", p.Type)
		}
		if p.ElemInfo == nil || p.ElemInfo.Type != schema.String {
			t.Fatalf("ElemInfo = %+v, want String element", p.ElemInfo)
		}
	})

	t.Run("array of objects", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			jsonKeyItems: prop(map[string]any{
				jsonKeyType:  jsonTypeArray,
				jsonKeyItems: objectProp(map[string]any{"id": map[string]any{jsonKeyType: jsonTypeString}}),
			}),
		}))
		p := params[jsonKeyItems]
		if p.ElemInfo == nil || p.ElemInfo.Type != schema.Object {
			t.Fatalf("ElemInfo = %+v, want Object element", p.ElemInfo)
		}
		if _, ok := p.ElemInfo.SubParams["id"]; !ok {
			t.Errorf("ElemInfo.SubParams missing %q", "id")
		}
	})

	t.Run("array without items", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"anything": prop(map[string]any{jsonKeyType: jsonTypeArray}),
		}))
		p := params["anything"]
		if p.Type != schema.Array {
			t.Fatalf("Type = %v, want Array", p.Type)
		}
		if p.ElemInfo != nil {
			t.Errorf("ElemInfo = %+v, want nil", p.ElemInfo)
		}
	})

	t.Run("tuple-form items", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"pair": prop(map[string]any{
				jsonKeyType: jsonTypeArray,
				"prefixItems": []any{
					map[string]any{jsonKeyType: jsonTypeString},
					map[string]any{jsonKeyType: jsonTypeNumber},
				},
			}),
		}))
		p := params["pair"]
		if p.ElemInfo != nil {
			t.Errorf("ElemInfo = %+v, want nil for tuple form", p.ElemInfo)
		}
		if !strings.Contains(p.Desc, "heterogeneous positional types") {
			t.Errorf("Desc = %q, want tuple note", p.Desc)
		}
	})
}

func TestConvertMCPSchemaToParams_NestedObjects(t *testing.T) {
	t.Run("single-level nested object with own required", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"location": prop(objectProp(map[string]any{
				"lat": map[string]any{jsonKeyType: jsonTypeNumber},
				"lon": map[string]any{jsonKeyType: jsonTypeNumber},
			}, "lat")),
		}))
		p := params["location"]
		if p.Type != schema.Object {
			t.Fatalf("Type = %v, want Object", p.Type)
		}
		if !p.SubParams["lat"].Required {
			t.Error("SubParams[lat].Required = false, want true")
		}
		if p.SubParams["lon"].Required {
			t.Error("SubParams[lon].Required = true, want false")
		}
	})

	t.Run("object without properties", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"metadata": prop(map[string]any{jsonKeyType: jsonTypeObject}),
		}))
		p := params["metadata"]
		if p.Type != schema.Object {
			t.Fatalf("Type = %v, want Object", p.Type)
		}
		if p.SubParams != nil {
			t.Errorf("SubParams = %v, want nil", p.SubParams)
		}
	})

	t.Run("multi-level nesting: object containing array of objects", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"zone": prop(objectProp(map[string]any{
				"devices": map[string]any{
					jsonKeyType:  jsonTypeArray,
					jsonKeyItems: objectProp(map[string]any{"id": map[string]any{jsonKeyType: jsonTypeString}}),
				},
			})),
		}))
		p := params["zone"]
		devices := p.SubParams["devices"]
		if devices.Type != schema.Array {
			t.Fatalf("devices.Type = %v, want Array", devices.Type)
		}
		if devices.ElemInfo == nil || devices.ElemInfo.Type != schema.Object {
			t.Fatalf("devices.ElemInfo = %+v, want Object element", devices.ElemInfo)
		}
		if _, ok := devices.ElemInfo.SubParams["id"]; !ok {
			t.Error("devices.ElemInfo.SubParams missing id")
		}
	})
}

func TestConvertMCPSchemaToParams_NullableObjectUnion(t *testing.T) {
	params := convert(t, schemaOf(map[string]any{
		"data": prop(map[string]any{
			jsonKeyDescription: "Optional service data.",
			jsonKeyAnyOf: []any{
				map[string]any{jsonKeyType: jsonTypeObject},
				map[string]any{jsonKeyType: string(schema.Null)},
			},
		}),
	}))

	p := params["data"]
	if p.Type != schema.Object {
		t.Fatalf("Type = %v, want Object for object|null", p.Type)
	}
	if !strings.Contains(p.Desc, "Optional service data.") || !strings.Contains(p.Desc, "null is also permitted") {
		t.Fatalf("Desc = %q, want original description and nullable note", p.Desc)
	}
}

func TestConvertMCPSchemaToParams_ConstraintsInDesc(t *testing.T) {
	t.Run("numeric range", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			propBrightness: prop(map[string]any{jsonKeyType: jsonTypeInteger, "minimum": 0.0, "maximum": 255.0}),
		}))
		if !strings.Contains(params[propBrightness].Desc, "range: 0..255") {
			t.Errorf("Desc = %q, want range note", params[propBrightness].Desc)
		}
	})

	t.Run("format", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"when": prop(map[string]any{jsonKeyType: jsonTypeString, jsonKeyFormat: "date-time"}),
		}))
		if !strings.Contains(params["when"].Desc, "format: date-time") {
			t.Errorf("Desc = %q, want format note", params["when"].Desc)
		}
	})

	t.Run("no constraints leaves Desc byte-identical to description", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			attrEntityID: prop(map[string]any{jsonKeyType: jsonTypeString, jsonKeyDescription: descTheEntity}),
		}))
		if params[attrEntityID].Desc != descTheEntity {
			t.Errorf("Desc = %q, want exactly %q", params[attrEntityID].Desc, descTheEntity)
		}
	})
}

func TestConvertMCPSchemaToParams_Degradation(t *testing.T) {
	t.Run("self-referential $ref", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"node": prop(map[string]any{"$ref": "#/$defs/node"}),
		}))
		p := params["node"]
		if p.Type != schema.String {
			t.Fatalf("Type = %v, want String on $ref degradation", p.Type)
		}
		if !strings.Contains(p.Desc, "$ref") {
			t.Errorf("Desc = %q, want $ref note", p.Desc)
		}
	})

	t.Run("depth beyond limit", func(t *testing.T) {
		// Build an object nested one level deeper than maxSchemaDepth.
		deepest := buildNestedObjectSchema(maxSchemaDepth+1, jsonTypeString)
		params := convert(t, schemaOf(map[string]any{
			"root": prop(deepest),
		}))

		// Walk down levels within the limit and confirm they still convert
		// faithfully to Object, then find the point where it degrades to String.
		p := params["root"]
		depth := 1
		for p.Type == schema.Object && p.SubParams != nil {
			next, ok := p.SubParams["next"]
			if !ok {
				break
			}
			p = next
			depth++
		}
		if p.Type != schema.String {
			t.Fatalf("expected degradation to String beyond depth limit, got %v at depth %d", p.Type, depth)
		}
		if !strings.Contains(p.Desc, "nested beyond the supported depth") {
			t.Errorf("Desc = %q, want depth note", p.Desc)
		}
	})

	t.Run("anyOf composition", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"value": prop(map[string]any{
				jsonKeyAnyOf: []any{
					map[string]any{jsonKeyType: jsonTypeString},
					map[string]any{jsonKeyType: jsonTypeNumber},
				},
			}),
		}))
		p := params["value"]
		if p.Type != schema.String {
			t.Fatalf("Type = %v, want String on anyOf degradation", p.Type)
		}
		if !strings.Contains(p.Desc, jsonKeyAnyOf) {
			t.Errorf("Desc = %q, want anyOf note", p.Desc)
		}
	})

	t.Run("oneOf composition", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"value": prop(map[string]any{
				"oneOf": []any{
					map[string]any{jsonKeyType: jsonTypeString},
					map[string]any{jsonKeyType: jsonTypeBoolean},
				},
			}),
		}))
		if params["value"].Type != schema.String {
			t.Fatalf("Type = %v, want String on oneOf degradation", params["value"].Type)
		}
	})

	t.Run("union type with null", func(t *testing.T) {
		params := convert(t, schemaOf(map[string]any{
			"maybe": prop(map[string]any{jsonKeyType: []any{jsonTypeString, "null"}}),
		}))
		p := params["maybe"]
		if p.Type != schema.String {
			t.Fatalf("Type = %v, want String (first non-null type)", p.Type)
		}
		if !strings.Contains(p.Desc, "null is also permitted") {
			t.Errorf("Desc = %q, want null-permitted note", p.Desc)
		}
	})

	t.Run("no panic on any hostile combination", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("conversion panicked: %v", r)
			}
		}()

		convert(t, schemaOf(map[string]any{
			"a": prop(map[string]any{"$ref": "#/a", jsonKeyAnyOf: []any{map[string]any{jsonKeyType: jsonTypeString}}}),
			"b": prop(map[string]any{jsonKeyType: []any{"null"}}),
		}))
	})
}

func TestConvertMCPSchemaToParams_FallbackOnMalformedSchema(t *testing.T) {
	// A "type" value that is a JSON number (not a string or array) makes
	// jsonschema.Schema.UnmarshalJSON return an error for that sub-schema,
	// which propagates up through the whole tool schema.
	malformed := schemaOf(map[string]any{
		"bad": prop(map[string]any{jsonKeyType: 5.0}),
	})

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	params := convertMCPSchemaToParams(logger, "quirky-tool", malformed)

	legacyParams := convertMCPSchemaToParamsLegacy(malformed)
	if len(params) != len(legacyParams) {
		t.Fatalf("fallback params count = %d, want %d (legacy)", len(params), len(legacyParams))
	}
	for name, want := range legacyParams {
		got, ok := params[name]
		if !ok {
			t.Fatalf("fallback missing param %q", name)
		}
		if got.Type != want.Type || got.Desc != want.Desc || got.Required != want.Required {
			t.Errorf("fallback param %q = %+v, want %+v", name, got, want)
		}
	}

	logLines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1
	if strings.TrimSpace(buf.String()) == "" {
		logLines = 0
	}
	if logLines != 1 {
		t.Fatalf("expected exactly 1 DEBUG log record, got %d: %s", logLines, buf.String())
	}
	if !strings.Contains(buf.String(), "quirky-tool") {
		t.Errorf("log record does not name the tool: %s", buf.String())
	}
}

func TestConvertMCPSchemaToParams_NoRegressionInToolCount(t *testing.T) {
	// A catalog mixing a well-formed and a malformed schema: the malformed
	// one falls back but is still present, so both tools remain represented.
	good := convert(t, schemaOf(map[string]any{
		attrEntityID: prop(map[string]any{jsonKeyType: jsonTypeString}),
	}))
	bad := convertMCPSchemaToParams(discardLogger(), "bad-tool", schemaOf(map[string]any{
		"weird": prop(map[string]any{jsonKeyType: 5.0}),
	}))

	if len(good) != 1 {
		t.Errorf("good tool params = %d, want 1", len(good))
	}
	if len(bad) != 1 {
		t.Errorf("bad tool params (via fallback) = %d, want 1", len(bad))
	}
}

func TestConvertMCPSchemaToParams_IntegerDistinctFromNumber(t *testing.T) {
	params := convert(t, schemaOf(map[string]any{
		"count": prop(map[string]any{jsonKeyType: jsonTypeInteger}),
		"ratio": prop(map[string]any{jsonKeyType: jsonTypeNumber}),
	}))

	if params["count"].Type != schema.Integer {
		t.Errorf("count.Type = %v, want Integer", params["count"].Type)
	}
	if params["ratio"].Type != schema.Number {
		t.Errorf("ratio.Type = %v, want Number", params["ratio"].Type)
	}
}
