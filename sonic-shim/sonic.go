// Package sonic is a drop-in shim for github.com/bytedance/sonic that delegates
// to encoding/json. It exposes only the subset of the sonic API actually used by
// github.com/cloudwego/eino, making cross-compilation to 32-bit architectures
// (e.g. GOARCH=arm) possible without sacrificing correctness.
package sonic

import (
	"encoding/json"
	"fmt"
)

// Marshal serialises v to JSON bytes.
func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// MarshalString serialises v to a JSON string.
func MarshalString(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Unmarshal deserialises JSON data into v.
func Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// UnmarshalString deserialises a JSON string into v.
func UnmarshalString(data string, v any) error {
	return json.Unmarshal([]byte(data), v)
}

// Node represents a JSON value extracted by GetFromString.
type Node struct {
	raw []byte
}

// MarshalJSON returns the raw JSON bytes of the node.
func (n Node) MarshalJSON() ([]byte, error) {
	return n.raw, nil
}

// GetFromString navigates the JSON string along the provided key path and
// returns the matching node. Each key must be a string (object field name).
func GetFromString(data string, keys ...any) (Node, error) {
	if len(keys) == 0 {
		return Node{raw: []byte(data)}, nil
	}

	var current any
	if err := json.Unmarshal([]byte(data), &current); err != nil {
		return Node{}, fmt.Errorf("sonic shim: failed to parse JSON: %w", err)
	}

	for i, k := range keys {
		key, ok := k.(string)
		if !ok {
			return Node{}, fmt.Errorf("sonic shim: key[%d] must be a string, got %T", i, k)
		}
		m, ok := current.(map[string]any)
		if !ok {
			return Node{}, fmt.Errorf("sonic shim: value at key[%d] is not an object", i)
		}
		current, ok = m[key]
		if !ok {
			return Node{}, fmt.Errorf("sonic shim: key %q not found", key)
		}
	}

	raw, err := json.Marshal(current)
	if err != nil {
		return Node{}, fmt.Errorf("sonic shim: failed to re-marshal node: %w", err)
	}
	return Node{raw: raw}, nil
}
