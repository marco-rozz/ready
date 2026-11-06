package llmtools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/flexigpt/llmtools-go/spec"
)

const builtinSchemaDraft07URI = "http://json-schema.org/draft-07/schema#"

var builtinSchemaTypes = map[string]struct{}{
	"array":   {},
	"boolean": {},
	"integer": {},
	"null":    {},
	"number":  {},
	"object":  {},
	"string":  {},
}

func TestBuiltinToolSchemas(t *testing.T) {
	registry, err := NewBuiltinRegistry(WithDefaultCallTimeout(0))
	if err != nil {
		t.Fatalf("NewBuiltinRegistry() error = %v", err)
	}

	tools := registry.Tools()
	if len(tools) == 0 {
		t.Fatal("Tools() returned no built-in tools")
	}

	for _, tool := range tools {
		t.Run(tool.Slug, func(t *testing.T) {
			assertBuiltinSchema(t, "argSchema", tool.ArgSchema)
			assertBuiltinSchema(t, "outputSchema", tool.OutputSchema)
		})
	}
}

func assertBuiltinSchema(
	t *testing.T,
	name string,
	raw spec.JSONSchema,
) {
	t.Helper()

	if len(bytes.TrimSpace(raw)) == 0 {
		t.Fatalf("%s is empty", name)
	}
	if !json.Valid(raw) {
		t.Fatalf("%s is not valid JSON: %s", name, raw)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}

	root, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s root type is %T, want object", name, value)
	}

	schemaURI, ok := root["$schema"].(string)
	if !ok || schemaURI != builtinSchemaDraft07URI {
		t.Fatalf(
			"%s $schema = %#v, want %q",
			name,
			root["$schema"],
			builtinSchemaDraft07URI,
		)
	}

	if err := validateBuiltinSchemaNode(root, name); err != nil {
		t.Fatal(err)
	}
}

func validateBuiltinSchemaNode(
	value any,
	location string,
) error {
	node, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: schema node must be an object", location)
	}

	rawType, hasType := node["type"]
	rawOneOf, hasOneOf := node["oneOf"]
	_, hasConst := node["const"]

	if !hasType && !hasOneOf && !hasConst {
		return fmt.Errorf(
			"%s: schema node must declare type, oneOf, or const",
			location,
		)
	}

	var schemaTypes []string
	if hasType {
		var err error
		schemaTypes, err = builtinSchemaTypeNames(rawType, location)
		if err != nil {
			return err
		}
	}

	if hasOneOf {
		branches, ok := rawOneOf.([]any)
		if !ok || len(branches) == 0 {
			return fmt.Errorf("%s.oneOf must be a non-empty array", location)
		}
		for index, branch := range branches {
			if err := validateBuiltinSchemaNode(
				branch,
				fmt.Sprintf("%s.oneOf[%d]", location, index),
			); err != nil {
				return err
			}
		}
	}

	rawAdditionalProperties, hasAdditionalProperties := node["additionalProperties"]
	rawProperties, hasProperties := node["properties"]

	var properties map[string]any
	if hasProperties {
		if !builtinSchemaContainsType(schemaTypes, "object") {
			return fmt.Errorf("%s.properties requires object type", location)
		}

		var ok bool
		properties, ok = rawProperties.(map[string]any)
		if !ok {
			return fmt.Errorf("%s.properties must be an object", location)
		}

		closed, ok := rawAdditionalProperties.(bool)
		if !hasAdditionalProperties || !ok || closed {
			return fmt.Errorf(
				"%s: objects with properties must set additionalProperties to false",
				location,
			)
		}

		propertyNames := make([]string, 0, len(properties))
		for propertyName := range properties {
			propertyNames = append(propertyNames, propertyName)
		}
		sort.Strings(propertyNames)

		for _, propertyName := range propertyNames {
			if err := validateBuiltinSchemaNode(
				properties[propertyName],
				location+".properties."+propertyName,
			); err != nil {
				return err
			}
		}
	}

	if hasAdditionalProperties {
		switch additionalProperties := rawAdditionalProperties.(type) {
		case bool:
			if additionalProperties {
				return fmt.Errorf(
					"%s.additionalProperties must not be true",
					location,
				)
			}

		case map[string]any:
			if hasProperties {
				return fmt.Errorf(
					"%s: additionalProperties schema is only valid for dynamic map objects",
					location,
				)
			}
			if !builtinSchemaContainsType(schemaTypes, "object") {
				return fmt.Errorf(
					"%s.additionalProperties schema requires object type",
					location,
				)
			}
			if err := validateBuiltinSchemaNode(
				additionalProperties,
				location+".additionalProperties",
			); err != nil {
				return err
			}

		default:
			return fmt.Errorf(
				"%s.additionalProperties must be false or a schema object",
				location,
			)
		}
	}

	if builtinSchemaContainsType(schemaTypes, "object") &&
		!hasProperties &&
		!hasAdditionalProperties &&
		!hasOneOf {
		return fmt.Errorf(
			"%s: object schema has neither properties nor additionalProperties",
			location,
		)
	}

	if rawRequired, found := node["required"]; found {
		if !hasProperties {
			return fmt.Errorf("%s.required requires properties", location)
		}

		required, err := builtinSchemaStringList(
			rawRequired,
			location+".required",
		)
		if err != nil {
			return err
		}

		seen := make(map[string]struct{}, len(required))
		for _, propertyName := range required {
			if _, duplicate := seen[propertyName]; duplicate {
				return fmt.Errorf(
					"%s.required repeats %q",
					location,
					propertyName,
				)
			}
			if _, found := properties[propertyName]; !found {
				return fmt.Errorf(
					"%s.required references unknown property %q",
					location,
					propertyName,
				)
			}
			seen[propertyName] = struct{}{}
		}
	}

	rawItems, hasItems := node["items"]
	if hasItems {
		if !builtinSchemaContainsType(schemaTypes, "array") {
			return fmt.Errorf("%s.items requires array type", location)
		}
		if err := validateBuiltinSchemaNode(rawItems, location+".items"); err != nil {
			return err
		}
	}

	if builtinSchemaContainsType(schemaTypes, "array") && !hasItems {
		return fmt.Errorf("%s: array schema requires items", location)
	}

	return nil
}

func builtinSchemaTypeNames(
	value any,
	location string,
) ([]string, error) {
	switch typed := value.(type) {
	case string:
		if _, supported := builtinSchemaTypes[typed]; !supported {
			return nil, fmt.Errorf(
				"%s.type has unsupported value %q",
				location,
				typed,
			)
		}
		return []string{typed}, nil

	case []any:
		if len(typed) == 0 {
			return nil, fmt.Errorf("%s.type must not be empty", location)
		}

		seen := make(map[string]struct{}, len(typed))
		types := make([]string, 0, len(typed))
		for _, rawType := range typed {
			schemaType, ok := rawType.(string)
			if !ok {
				return nil, fmt.Errorf(
					"%s.type values must be strings",
					location,
				)
			}
			if _, supported := builtinSchemaTypes[schemaType]; !supported {
				return nil, fmt.Errorf(
					"%s.type has unsupported value %q",
					location,
					schemaType,
				)
			}
			if _, duplicate := seen[schemaType]; duplicate {
				return nil, fmt.Errorf(
					"%s.type repeats %q",
					location,
					schemaType,
				)
			}

			seen[schemaType] = struct{}{}
			types = append(types, schemaType)
		}
		return types, nil

	default:
		return nil, fmt.Errorf(
			"%s.type must be a string or string array",
			location,
		)
	}
}

func builtinSchemaContainsType(types []string, wanted string) bool {
	return slices.Contains(types, wanted)
}

func builtinSchemaStringList(
	value any,
	location string,
) ([]string, error) {
	rawValues, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", location)
	}

	values := make([]string, 0, len(rawValues))
	for _, rawValue := range rawValues {
		value, ok := rawValue.(string)
		if !ok {
			return nil, fmt.Errorf("%s values must be strings", location)
		}
		values = append(values, value)
	}
	return values, nil
}
