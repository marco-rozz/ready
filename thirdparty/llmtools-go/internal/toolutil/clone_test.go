package toolutil

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go/spec"
)

func TestCloneTool(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name string
		in   spec.Tool
	}{
		{
			name: "zero value",
			in:   spec.Tool{},
		},
		{
			name: "non-empty ArgSchema and Tags are deep-copied",
			in: spec.Tool{
				SchemaVersion: spec.SchemaVersion,
				ID:            "0190f3f3-6a2c-7c1a-9f59-aaaaaaaaaaaa",
				Slug:          "weather",
				Version:       "v1",
				DisplayName:   "Weather",
				Description:   "Gets weather",
				ArgSchema:     spec.JSONSchema([]byte(`{"type":"object","properties":{"q":{"type":"string"}}}`)),
				GoImpl:        spec.GoToolImpl{FuncID: "github.com/acme/tools.Weather"},
				CreatedAt:     ts,
				ModifiedAt:    ts,
				Tags:          []string{"network", "demo"},
			},
		},
		{
			// Regression case: a slice can have len==0 but still have backing storage (cap>0).
			// A correct deep clone must not share that backing array.
			name: "empty-but-non-nil (len==0, cap>0) ArgSchema and Tags must not alias",
			in: spec.Tool{
				SchemaVersion: spec.SchemaVersion,
				ID:            "0190f3f3-6a2c-7c1a-9f59-bbbbbbbbbbbb",
				Slug:          "empty-cap",
				Version:       "v1",
				DisplayName:   "EmptyCap",
				Description:   "Edge case",
				ArgSchema:     spec.JSONSchema(make([]byte, 0, 1)),
				GoImpl:        spec.GoToolImpl{FuncID: "github.com/acme/tools.EmptyCap"},
				CreatedAt:     ts,
				ModifiedAt:    ts,
				Tags:          make([]string, 0, 1),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orig := tc.in

			cloned := CloneTool(orig)

			// Value equality: clone should initially match.
			if !reflect.DeepEqual(orig, cloned) {
				t.Fatalf("clone not equal to original.\norig:  %#v\nclone: %#v", orig, cloned)
			}

			// Deep-copy checks.
			assertBytesDeepCopied(t, []byte(orig.ArgSchema), []byte(cloned.ArgSchema))
			assertStringsDeepCopied(t, orig.Tags, cloned.Tags)
		})
	}
}

func TestCloneStringSlice(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		wantNil bool
		want    []string
	}{
		{
			name:    "nil input returns nil",
			in:      nil,
			wantNil: true,
		},
		{
			name:    "empty input returns nil",
			in:      []string{},
			wantNil: true,
		},
		{
			name:    "empty-but-non-nil input returns nil",
			in:      make([]string, 0, 1),
			wantNil: true,
		},
		{
			name: "non-empty input is copied",
			in:   []string{"alpha", "beta"},
			want: []string{"alpha", "beta"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CloneStringSlice(tc.in)

			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil result, got %#v", got)
				}
				return
			}

			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("unexpected clone. want %#v, got %#v", tc.want, got)
			}

			snap := append([]string(nil), tc.in...)
			got[0] += "-changed"
			if !reflect.DeepEqual(tc.in, snap) {
				t.Fatalf("input slice changed after mutating clone; slices alias")
			}
		})
	}
}

func assertBytesDeepCopied(t *testing.T, orig, cloned []byte) {
	t.Helper()

	switch {
	case len(orig) > 0:
		// Mutating clone must not affect orig.
		snap := append([]byte(nil), orig...)
		cloned[0] ^= 0xFF
		if !bytes.Equal(orig, snap) {
			t.Fatalf("orig ArgSchema changed after mutating clone; slices alias")
		}

	case len(orig) == 0 && cap(orig) > 0:
		// Aliasing detector for empty slices that still have backing arrays:
		// append to clone, then append to orig; if they share backing storage,
		// orig's append can overwrite clone's element.
		c := append(cloned, 0xAA) //nolint:gocritic // We need to detect aliasing here.
		_ = append(orig, 0xBB)

		if len(c) != 1 || c[0] != 0xAA {
			t.Fatalf("ArgSchema appears to alias for len==0,cap>0 (expected 0xAA to remain), got %#v", c)
		}
	}
}

func assertStringsDeepCopied(t *testing.T, orig, cloned []string) {
	t.Helper()

	switch {
	case len(orig) > 0:
		// Mutating clone must not affect orig.
		snap := append([]string(nil), orig...)
		cloned[0] += "-changed"
		if !reflect.DeepEqual(orig, snap) {
			t.Fatalf("orig Tags changed after mutating clone; slices alias")
		}

	case len(orig) == 0 && cap(orig) > 0:
		// Same aliasing detector for []string.
		c := append(cloned, "A") //nolint:gocritic // We need to detect aliasing here.
		_ = append(orig, "B")

		if len(c) != 1 || c[0] != "A" {
			t.Fatalf("Tags appear to alias for len==0,cap>0 (expected %q to remain), got %#v", "A", c)
		}
	}
}
