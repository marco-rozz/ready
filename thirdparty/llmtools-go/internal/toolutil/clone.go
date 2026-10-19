package toolutil

import (
	"bytes"

	"github.com/flexigpt/llmtools-go/spec"
)

func CloneTool(t spec.Tool) spec.Tool {
	// ArgSchema: deep copy even if len==0 but slice is non-nil (may have cap>0 backing array).
	if t.ArgSchema != nil {
		t.ArgSchema = spec.JSONSchema(bytes.Clone([]byte(t.ArgSchema)))
	}

	// Tags: deep copy even if len==0 but slice is non-nil (may have cap>0 backing array).
	if t.Tags != nil {
		dst := make([]string, len(t.Tags))
		copy(dst, t.Tags)
		t.Tags = dst
	}

	return t
}

func CloneStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
