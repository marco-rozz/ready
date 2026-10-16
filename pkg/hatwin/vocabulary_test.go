//nolint:goconst
package hatwin

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestResolveIntent_LightColor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		params  map[string]any
		want    map[string]any
		wantErr string
	}{
		{
			name:   "rgb_color",
			params: map[string]any{"rgb_color": []any{0.0, 255.0, 0.0}},
			want:   map[string]any{"rgb_color": []float64{0, 255, 0}},
		},
		{
			name:   "hs_color",
			params: map[string]any{"hs_color": []any{120.0, 100.0}},
			want:   map[string]any{"hs_color": []float64{120, 100}},
		},
		{
			name:   "color_name with brightness",
			params: map[string]any{"color_name": "green", "brightness": 200.0},
			want:   map[string]any{"color_name": "green", "brightness": 200.0},
		},
		{
			name:   "color_temp_kelvin",
			params: map[string]any{"color_temp_kelvin": 3000.0},
			want:   map[string]any{"color_temp_kelvin": 3000.0},
		},
		{name: "rgb out of range", params: map[string]any{"rgb_color": []any{0.0, 256.0, 0.0}}, wantErr: "element 1"},
		{name: "rgb wrong length", params: map[string]any{"rgb_color": []any{0.0, 255.0}}, wantErr: "must be an array"},
		{name: "rgb not an array", params: map[string]any{"rgb_color": "green"}, wantErr: "must be an array"},
		{name: "empty color_name", params: map[string]any{"color_name": " "}, wantErr: "non-empty string"},
		{
			name:    "two colour modes",
			params:  map[string]any{"color_name": "green", "rgb_color": []any{0.0, 255.0, 0.0}},
			wantErr: "at most one colour parameter",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			service, data, err := resolveIntent("light", "set", tc.params)
			if tc.wantErr != "" {
				if !errors.Is(err, ErrInvalidParams) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want ErrInvalidParams containing %q", err, tc.wantErr)
				}

				return
			}
			if err != nil {
				t.Fatalf("resolveIntent: %v", err)
			}
			if service != "turn_on" {
				t.Fatalf("service = %q, want turn_on", service)
			}
			if !reflect.DeepEqual(data, tc.want) {
				t.Fatalf("data = %#v, want %#v", data, tc.want)
			}
		})
	}
}

func TestRenderVocabularyDescription_LightColorHints(t *testing.T) {
	t.Parallel()

	desc := RenderVocabularyDescription([]string{"light"})
	for _, want := range []string{"rgb_color ([r, g, b], each 0–255)", "color_name (CSS colour name, e.g. green)", "hs_color (", "color_temp_kelvin (1000–10000)"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q:\n%s", want, desc)
		}
	}
}
