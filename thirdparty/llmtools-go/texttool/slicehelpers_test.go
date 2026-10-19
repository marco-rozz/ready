package texttool

import (
	"testing"

	"github.com/flexigpt/llmtools-go/internal/ioutil"
)

func Test_insertLines_Bounds(t *testing.T) {
	lines := []string{"A", "B"}
	toInsert := []string{"X"}

	tests := []struct {
		name string
		idx  int
		want []string
	}{
		{"idx_negative_clamps_to_0", -10, []string{"X", "A", "B"}},
		{"idx_zero", 0, []string{"X", "A", "B"}},
		{"idx_middle", 1, []string{"A", "X", "B"}},
		{"idx_len", 2, []string{"A", "B", "X"}},
		{"idx_too_large_clamps_to_len", 100, []string{"A", "B", "X"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ioutil.ReplaceStringRange(lines, tt.idx, tt.idx, toInsert)
			if len(got) != len(tt.want) {
				t.Fatalf("len: want %d got %d (%v)", len(tt.want), len(got), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("got[%d]=%q want %q (%v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

func Test_replaceLinesSlice_Bounds(t *testing.T) {
	lines := []string{"A", "B", "C", "D"}
	repl := []string{"X"}

	tests := []struct {
		name       string
		start, end int
		want       []string
	}{
		{"start_negative_clamps_to_0", -10, 1, []string{"X", "B", "C", "D"}},
		{"end_less_than_start_clamps", 2, 1, []string{"A", "B", "X", "C", "D"}}, // replaces empty at index 2
		{"start_gt_len_clamps_to_len", 100, 200, []string{"A", "B", "C", "D", "X"}},
		{"replace_middle", 1, 3, []string{"A", "X", "D"}},
		{"replace_entire", 0, 4, []string{"X"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ioutil.ReplaceStringRange(lines, tt.start, tt.end, repl)
			if len(got) != len(tt.want) {
				t.Fatalf("len: want %d got %d (%v)", len(tt.want), len(got), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("got[%d]=%q want %q (%v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}
