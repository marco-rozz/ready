package jsonutil

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type person struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestEncodeToJSONRaw(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         any
		wantJSON   string
		wantErrSub string
	}{
		{
			name:     "struct_happy",
			in:       person{Name: "alice", Age: 30},
			wantJSON: `{"name":"alice","age":30}`,
		},
		{
			name:     "slice_happy",
			in:       []int{1, 2, 3},
			wantJSON: `[1,2,3]`,
		},
		{
			name:       "marshal_error_unsupported_type",
			in:         make(chan int),
			wantErrSub: "encode JSON:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := EncodeToJSONRaw(tt.in)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErrSub, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !json.Valid(got) {
				t.Fatalf("expected valid JSON, got: %q", string(got))
			}
			if string(got) != tt.wantJSON {
				t.Fatalf("unexpected JSON.\nwant: %s\ngot:  %s", tt.wantJSON, string(got))
			}
		})
	}
}

func TestDecodeJSONRaw_BlankReturnsZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "nil", raw: nil},
		{name: "empty", raw: json.RawMessage{}},
		{name: "whitespace", raw: json.RawMessage(" \n\t ")},
	}

	for _, tt := range tests {

		t.Run(tt.name+"_struct", func(t *testing.T) {
			t.Parallel()
			got, err := DecodeJSONRaw[person](tt.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != (person{}) {
				t.Fatalf("expected zero value, got %#v", got)
			}
		})

		t.Run(tt.name+"_int", func(t *testing.T) {
			t.Parallel()
			got, err := DecodeJSONRaw[int](tt.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != 0 {
				t.Fatalf("expected zero value, got %v", got)
			}
		})

		t.Run(tt.name+"_slice", func(t *testing.T) {
			t.Parallel()
			got, err := DecodeJSONRaw[[]int](tt.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != nil {
				t.Fatalf("expected nil slice (zero value), got %#v", got)
			}
		})

		t.Run(tt.name+"_pointer", func(t *testing.T) {
			t.Parallel()
			got, err := DecodeJSONRaw[*person](tt.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != nil {
				t.Fatalf("expected nil pointer (zero value), got %#v", got)
			}
		})
	}
}

func TestDecodeJSONRaw_Happy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  json.RawMessage
		want person
	}{
		{
			name: "object",
			raw:  json.RawMessage(`{"name":"bob","age":42}`),
			want: person{Name: "bob", Age: 42},
		},
		{
			name: "object_with_whitespace",
			raw:  json.RawMessage(" \n\t " + `{"name":"bob","age":42}` + " \n\t "),
			want: person{Name: "bob", Age: 42},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := DecodeJSONRaw[person](tt.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unexpected value.\nwant: %#v\ngot:  %#v", tt.want, got)
			}
		})
	}

	t.Run("pointer_null", func(t *testing.T) {
		t.Parallel()

		got, err := DecodeJSONRaw[*person](json.RawMessage(`null`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("expected nil pointer, got %#v", got)
		}
	})
}

func TestDecodeJSONRaw_Errors(t *testing.T) {
	t.Parallel()
	const errDecodeJSONSub = `decode JSON:`
	tests := []struct {
		name       string
		raw        json.RawMessage
		wantErrSub string
		// If true, also assert the returned value is the zero value.
		assertZero bool
	}{
		{
			name:       "unknown_field_disallowed",
			raw:        json.RawMessage(`{"name":"x","age":1,"extra":true}`),
			wantErrSub: errDecodeJSONSub,
			assertZero: true,
		},
		{
			name:       "invalid_json",
			raw:        json.RawMessage(`{"name":`),
			wantErrSub: errDecodeJSONSub,
			assertZero: true,
		},
		{
			name:       "wrong_type",
			raw:        json.RawMessage(`{"name":"x","age":"not-a-number"}`),
			wantErrSub: errDecodeJSONSub,
			assertZero: true,
		},
		{
			name:       "trailing_valid_json_value",
			raw:        json.RawMessage(`{"name":"x","age":1} {"name":"y","age":2}`),
			wantErrSub: `trailing data validation`,
			assertZero: true,
		},
		{
			name:       "trailing_invalid_json",
			raw:        json.RawMessage(`{"name":"x","age":1} {`),
			wantErrSub: `trailing data validation:`,
			assertZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := DecodeJSONRaw[person](tt.raw)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil (value=%#v)", tt.wantErrSub, got)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErrSub, err.Error())
			}
			if tt.assertZero && got != (person{}) {
				t.Fatalf("expected zero value on error, got %#v", got)
			}
		})
	}
}

func TestDecodeBytes_Options(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		data            []byte
		disallowUnknown bool
		requireEOF      bool
		want            person
		wantErrSub      string
	}{
		{
			name:            "disallowUnknown_true_rejects_unknown",
			data:            []byte(`{"name":"a","age":1,"extra":true}`),
			disallowUnknown: true,
			requireEOF:      true,
			wantErrSub:      `decode JSON:`,
		},
		{
			name:            "disallowUnknown_false_allows_unknown",
			data:            []byte(`{"name":"a","age":1,"extra":true}`),
			disallowUnknown: false,
			requireEOF:      true,
			want:            person{Name: "a", Age: 1},
		},
		{
			name:            "requireEOF_true_rejects_trailing_valid_json",
			data:            []byte(`{"name":"a","age":1} {"name":"b","age":2}`),
			disallowUnknown: true,
			requireEOF:      true,
			wantErrSub:      `trailing data validation`,
		},
		{
			name:            "requireEOF_false_allows_trailing_valid_json",
			data:            []byte(`{"name":"a","age":1} {"name":"b","age":2}`),
			disallowUnknown: true,
			requireEOF:      false,
			want:            person{Name: "a", Age: 1},
		},
		{
			name:            "requireEOF_false_allows_trailing_invalid_json",
			data:            []byte(`{"name":"a","age":1} {`),
			disallowUnknown: true,
			requireEOF:      false,
			want:            person{Name: "a", Age: 1},
		},
		{
			name:            "requireEOF_true_trailing_whitespace_ok",
			data:            []byte(`{"name":"a","age":1}` + "  \n\t "),
			disallowUnknown: true,
			requireEOF:      true,
			want:            person{Name: "a", Age: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got person
			err := decodeBytes(tt.data, &got, tt.disallowUnknown, tt.requireEOF)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErrSub, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unexpected value.\nwant: %#v\ngot:  %#v", tt.want, got)
			}
		})
	}
}

func TestRequireNoTrailing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		decodeInto any
		wantErrSub string
	}{
		{
			name:       "only_one_value_ok",
			input:      `{"a":1}`,
			decodeInto: &map[string]int{},
		},
		{
			name:       "trailing_whitespace_ok",
			input:      `{"a":1}` + " \n\t ",
			decodeInto: &map[string]int{},
		},
		{
			name:       "trailing_second_value_errors",
			input:      `{"a":1} {"b":2}`,
			decodeInto: &map[string]int{},
			wantErrSub: "unexpected trailing data after JSON value",
		},
		{
			name:       "trailing_invalid_json_errors_wrapped",
			input:      `{"a":1} {`,
			decodeInto: &map[string]int{},
			wantErrSub: "trailing data validation:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dec := json.NewDecoder(bytes.NewReader([]byte(tt.input)))

			if err := dec.Decode(tt.decodeInto); err != nil {
				t.Fatalf("unexpected error decoding first value: %v", err)
			}

			err := requireNoTrailing(dec)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErrSub, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestIsBlankJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []byte
		want bool
	}{
		{name: "nil", in: nil, want: true},
		{name: "empty", in: []byte(""), want: true},
		{name: "spaces", in: []byte("   "), want: true},
		{name: "tabs_newlines", in: []byte("\n\t\r"), want: true},
		{name: "null_is_not_blank", in: []byte("null"), want: false},
		{name: "empty_object_is_not_blank", in: []byte("{}"), want: false},
		{name: "zero_is_not_blank", in: []byte("0"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isBlankJSON(tt.in)
			if got != tt.want {
				t.Fatalf("isBlankJSON(%q) = %v, want %v", string(tt.in), got, tt.want)
			}
		})
	}
}

func TestNewDecoder_DisallowUnknownFieldsBehavior(t *testing.T) {
	t.Parallel()

	type onlyA struct {
		A int `json:"a"`
	}

	input := []byte(`{"a":1,"b":2}`)

	t.Run("disallowUnknown_true_rejects_unknown_fields", func(t *testing.T) {
		t.Parallel()

		dec := newDecoder(bytes.NewReader(input), true)
		var out onlyA
		err := dec.Decode(&out)
		if err == nil {
			t.Fatalf("expected error, got nil (out=%#v)", out)
		}
		if !strings.Contains(err.Error(), `unknown field`) {
			t.Fatalf("expected unknown field error, got %q", err.Error())
		}
	})

	t.Run("disallowUnknown_false_allows_unknown_fields", func(t *testing.T) {
		t.Parallel()

		dec := newDecoder(bytes.NewReader(input), false)
		var out onlyA
		err := dec.Decode(&out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(out, onlyA{A: 1}) {
			t.Fatalf("unexpected out: %#v", out)
		}
	})
}

func TestRequireNoTrailing_EOFCheck(t *testing.T) {
	t.Parallel()

	// Specifically ensure requireNoTrailing treats io.EOF as success and any other state as error.
	dec := json.NewDecoder(bytes.NewReader([]byte(`{"a":1}`)))
	var out map[string]int
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("unexpected error decoding first value: %v", err)
	}
	if err := requireNoTrailing(dec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDecodeBytes_ErrorWrapping(t *testing.T) {
	t.Parallel()

	var out person
	err := decodeBytes([]byte(`{"name":`), &out, true, true)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "decode JSON:") {
		t.Fatalf("expected wrapped error containing %q, got %q", "decode JSON:", err.Error())
	}
}
