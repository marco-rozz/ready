package llmtools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go/spec"
)

const errInvalidInputStr = "invalid input"

func TestNewRegistry_Options(t *testing.T) {
	tests := []struct {
		name    string
		opts    []RegistryOption
		wantDur time.Duration
	}{
		{
			name:    "no options => zero timeout",
			opts:    nil,
			wantDur: 0,
		},
		{
			name:    "WithDefaultCallTimeout sets timeout",
			opts:    []RegistryOption{WithDefaultCallTimeout(123 * time.Millisecond)},
			wantDur: 123 * time.Millisecond,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRegistry(tc.opts...)
			if err != nil {
				t.Fatalf("NewRegistry error: %v", err)
			}
			if r.timeout != tc.wantDur {
				t.Fatalf("timeout: got %v want %v", r.timeout, tc.wantDur)
			}
		})
	}
}

func TestRegistry_RegisterTool_Validation(t *testing.T) {
	okFn := func(context.Context, json.RawMessage) ([]spec.ToolOutputUnion, error) { return nil, nil }

	tests := []struct {
		name            string
		tool            spec.Tool
		fn              spec.ToolFunc
		wantErrContains string
	}{
		{
			name: "missing funcID",
			tool: func() spec.Tool {
				tl := mkTool("x", "s")
				tl.GoImpl.FuncID = ""
				return tl
			}(),
			fn:              okFn,
			wantErrContains: "missing funcID",
		},
		{
			name: "missing schemaVersion",
			tool: func() spec.Tool {
				tl := mkTool("x", "s")
				tl.SchemaVersion = ""
				return tl
			}(),
			fn:              okFn,
			wantErrContains: "missing schemaVersion",
		},
		{
			name: "schemaVersion mismatch",
			tool: func() spec.Tool {
				tl := mkTool("x", "s")
				tl.SchemaVersion = "1900-01-01"
				return tl
			}(),
			fn:              okFn,
			wantErrContains: "does not match",
		},
		{
			name: "argSchema invalid JSON",
			tool: func() spec.Tool {
				tl := mkTool("x", "s")
				tl.ArgSchema = spec.JSONSchema([]byte(`{"nope":`))
				return tl
			}(),
			fn:              okFn,
			wantErrContains: "argSchema is not valid JSON",
		},
		{
			name:            "nil func",
			tool:            mkTool("x", "s"),
			fn:              nil,
			wantErrContains: "nil func",
		},
		{
			name: "ok",
			tool: mkTool("x", "s"),
			fn:   okFn,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRegistry()
			if err != nil {
				t.Fatalf("NewRegistry error: %v", err)
			}
			err = r.RegisterTool(tc.tool, tc.fn)

			if tc.wantErrContains == "" {
				if err != nil {
					t.Fatalf("RegisterTool unexpected error: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("RegisterTool expected error containing %q, got nil", tc.wantErrContains)
			}
			if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Fatalf("RegisterTool error: got %q want contains %q", err.Error(), tc.wantErrContains)
			}
		})
	}
}

func TestRegistry_RegisterTool_Duplicate(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry error: %v", err)
	}

	tool := mkTool("github.com/acme/tools.Dupe", "dupe")
	fn := func(context.Context, json.RawMessage) ([]spec.ToolOutputUnion, error) { return nil, nil }

	if err := r.RegisterTool(tool, fn); err != nil {
		t.Fatalf("first RegisterTool error: %v", err)
	}
	err = r.RegisterTool(tool, fn)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("second RegisterTool: got %v want already registered error", err)
	}
}

func TestRegistry_Lookup(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry error: %v", err)
	}

	fn := func(context.Context, json.RawMessage) ([]spec.ToolOutputUnion, error) { return textOut("ok"), nil }
	tool := mkTool("github.com/acme/tools.Lookup", "lookup")

	if err := r.RegisterTool(tool, fn); err != nil {
		t.Fatalf("RegisterTool error: %v", err)
	}

	got, ok := r.Lookup(tool.GoImpl.FuncID)
	if !ok || got == nil {
		t.Fatalf("Lookup: got (ok=%v, fn=%v), want ok=true, fn!=nil", ok, got)
	}

	got, ok = r.Lookup("unknown")
	if ok || got != nil {
		t.Fatalf("Lookup unknown: got (ok=%v, fn=%v), want ok=false, fn=nil", ok, got)
	}
}

func TestRegistry_Tools_SortedAndCloned(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry error: %v", err)
	}

	dummy := func(context.Context, json.RawMessage) ([]spec.ToolOutputUnion, error) { return nil, nil }

	// Register in intentionally unsorted order.
	t1 := mkTool("github.com/acme/tools.Z", "a") // slug a, func Z
	t2 := mkTool("github.com/acme/tools.M", "a") // slug a, func M
	t3 := mkTool("github.com/acme/tools.B", "b") // slug b

	for _, tl := range []spec.Tool{t1, t2, t3} {
		if err := r.RegisterTool(tl, dummy); err != nil {
			t.Fatalf("RegisterTool(%s) error: %v", tl.GoImpl.FuncID, err)
		}
	}

	got := r.Tools()
	if len(got) != 3 {
		t.Fatalf("Tools len: got %d want %d", len(got), 3)
	}

	// Sorted by Slug, then FuncID.
	wantOrder := []spec.FuncID{
		t2.GoImpl.FuncID, // "a" + "M..."
		t1.GoImpl.FuncID, // "a" + "Z..."
		t3.GoImpl.FuncID, // "b" + "B..."
	}
	for i := range wantOrder {
		if got[i].GoImpl.FuncID != wantOrder[i] {
			t.Fatalf("Tools order[%d]: got %q want %q", i, got[i].GoImpl.FuncID, wantOrder[i])
		}
	}

	// Verify: modifying returned tool does not mutate registry state.
	if len(got[0].ArgSchema) == 0 || len(got[0].Tags) == 0 {
		t.Fatalf("test requires non-empty ArgSchema and Tags")
	}
	got[0].ArgSchema[0] ^= 0xFF
	got[0].Tags[0] = "mutated"

	got2 := r.Tools()
	if reflect.DeepEqual(got, got2) {
		t.Fatalf("expected Tools() to return fresh clones; got identical values after mutation")
	}
	if got2[0].Tags[0] == "mutated" {
		t.Fatalf("registry state mutated via Tools() return value (Tags alias)")
	}
	if got2[0].ArgSchema[0] == got[0].ArgSchema[0] {
		t.Fatalf("registry state mutated via Tools() return value (ArgSchema alias)")
	}

	// Verify: modifying original tool after registration does not mutate registry state.
	orig := mkTool("github.com/acme/tools.Orig", "orig")
	orig.ArgSchema = spec.JSONSchema([]byte(`{"k":1}`))
	orig.Tags = []string{"x", "y"}
	if err := r.RegisterTool(orig, dummy); err != nil {
		t.Fatalf("RegisterTool(orig) error: %v", err)
	}

	orig.ArgSchema[0] ^= 0xFF
	orig.Tags[0] = "changed"

	all := r.Tools()
	var reg spec.Tool
	for _, tl := range all {
		if tl.GoImpl.FuncID == "github.com/acme/tools.Orig" {
			reg = tl
			break
		}
	}
	if reg.GoImpl.FuncID == "" {
		t.Fatalf("expected to find registered tool")
	}
	if reg.Tags[0] == "changed" {
		t.Fatalf("registry state mutated via caller changing tool after RegisterTool (Tags alias)")
	}
	if len(reg.ArgSchema) > 0 && reg.ArgSchema[0] == orig.ArgSchema[0] {
		t.Fatalf("registry state mutated via caller changing tool after RegisterTool (ArgSchema alias)")
	}
}

func TestRegistry_Call_UnknownTool(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry error: %v", err)
	}
	_, err = r.Call(t.Context(), "nope", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("Call unknown: got %v want unknown tool error", err)
	}
}

func TestRegistry_Call_TimeoutResolution(t *testing.T) {
	const (
		defaultTout = 20 * time.Millisecond
		shortTout   = 10 * time.Millisecond
		watchdog    = 5 * time.Second
	)

	tests := []struct {
		name           string
		regTimeout     time.Duration
		callOpt        CallOption
		wantErrIs      error
		wantTextOutput string
	}{
		{
			name:       "registry default timeout cancels call",
			regTimeout: defaultTout,
			wantErrIs:  context.DeadlineExceeded,
		},
		{
			name:           "call timeout override 0 disables registry timeout",
			regTimeout:     defaultTout,
			callOpt:        WithCallTimeout(0),
			wantTextOutput: "ok",
		},
		{
			name:           "negative registry timeout treated as no timeout",
			regTimeout:     -1,
			wantTextOutput: "ok",
		},
		{
			name:       "call timeout override shorter than default cancels call",
			regTimeout: defaultTout,
			callOpt:    WithCallTimeout(shortTout),
			wantErrIs:  context.DeadlineExceeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRegistry(WithDefaultCallTimeout(tc.regTimeout))
			if err != nil {
				t.Fatalf("NewRegistry error: %v", err)
			}

			tool := mkTool("github.com/acme/tools.Sleepy", "sleepy")
			fn := func(ctx context.Context, _ json.RawMessage) ([]spec.ToolOutputUnion, error) {
				// For timeout cases, block until the registry-provided context is canceled.
				// This avoids wall-clock flakiness on slower macOS/Windows CI runners.
				if tc.wantErrIs != nil {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return textOut("ok"), nil
			}
			if err := r.RegisterTool(tool, fn); err != nil {
				t.Fatalf("RegisterTool error: %v", err)
			}

			opts := []CallOption(nil)
			if tc.callOpt != nil {
				opts = append(opts, tc.callOpt)
			}
			type callResult struct {
				out []spec.ToolOutputUnion
				err error
			}

			resultCh := make(chan callResult, 1)
			go func() {
				out, err := r.Call(t.Context(), tool.GoImpl.FuncID, json.RawMessage(`{}`), opts...)
				resultCh <- callResult{out: out, err: err}
			}()

			select {
			case res := <-resultCh:
				if tc.wantErrIs != nil {
					if res.err == nil || !errors.Is(res.err, tc.wantErrIs) {
						t.Fatalf("Call error: got %v want errors.Is(%v)=true", res.err, tc.wantErrIs)
					}
					return
				}

				if res.err != nil {
					t.Fatalf("Call unexpected error: %v", res.err)
				}
				if len(res.out) != 1 || res.out[0].Kind != spec.ToolOutputKindText || res.out[0].TextItem == nil {
					t.Fatalf("Call output shape: got %#v want single text output", res.out)
				}
				if res.out[0].TextItem.Text != tc.wantTextOutput {
					t.Fatalf("Call output text: got %q want %q", res.out[0].TextItem.Text, tc.wantTextOutput)
				}

			case <-time.After(watchdog):
				t.Fatalf("Call did not return within %v", watchdog)
			}
		})
	}
}

func TestRegisterTypedAsTextTool_StrictDecode_And_TextWrapping(t *testing.T) {
	type args struct {
		A int `json:"a"`
	}
	type ret struct {
		Sum int `json:"sum"`
	}

	tests := []struct {
		name            string
		in              json.RawMessage
		fn              func(context.Context, args) (ret, error)
		wantErrContains string
		wantSum         int
	}{
		{
			name: "ok",
			in:   json.RawMessage(`{"a":1}`),
			fn: func(_ context.Context, a args) (ret, error) {
				return ret{Sum: a.A + 1}, nil
			},
			wantSum: 2,
		},
		{
			name:            "unknown field rejected",
			in:              json.RawMessage(`{"a":1,"b":2}`),
			fn:              func(_ context.Context, a args) (ret, error) { return ret{Sum: a.A}, nil },
			wantErrContains: errInvalidInputStr,
		},
		{
			name:            "trailing data rejected",
			in:              json.RawMessage(`{"a":1} {"a":2}`),
			fn:              func(_ context.Context, a args) (ret, error) { return ret{Sum: a.A}, nil },
			wantErrContains: errInvalidInputStr,
		},
		{
			name:            "invalid JSON rejected",
			in:              json.RawMessage(`{"a":`),
			fn:              func(_ context.Context, a args) (ret, error) { return ret{Sum: a.A}, nil },
			wantErrContains: errInvalidInputStr,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRegistry()
			if err != nil {
				t.Fatalf("NewRegistry error: %v", err)
			}

			tool := mkTool("github.com/acme/tools.TypedText", "typedtext")
			if err := RegisterTypedAsTextTool(r, tool, tc.fn); err != nil {
				t.Fatalf("RegisterTypedAsTextTool error: %v", err)
			}

			out, err := r.Call(t.Context(), tool.GoImpl.FuncID, tc.in)
			if tc.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("Call error: got %v want contains %q", err, tc.wantErrContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("Call unexpected error: %v", err)
			}
			if len(out) != 1 || out[0].Kind != spec.ToolOutputKindText || out[0].TextItem == nil {
				t.Fatalf("Call output shape: got %#v want single text output", out)
			}

			var decoded ret
			if derr := json.Unmarshal([]byte(out[0].TextItem.Text), &decoded); derr != nil {
				t.Fatalf("output text is not JSON (%q): %v", out[0].TextItem.Text, derr)
			}
			if decoded.Sum != tc.wantSum {
				t.Fatalf("decoded.Sum: got %d want %d (text=%q)", decoded.Sum, tc.wantSum, out[0].TextItem.Text)
			}
		})
	}
}

func TestRegisterTypedAsTextTool_NullOutputBecomesNoOutputs(t *testing.T) {
	type args struct {
		A int `json:"a"`
	}
	type ret struct {
		Sum int `json:"sum"`
	}

	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry error: %v", err)
	}

	tool := mkTool("github.com/acme/tools.NullOut", "nullout")
	fn := func(context.Context, args) (*ret, error) { return nil, nil } //nolint:nilnil // NUll json test.

	if err := RegisterTypedAsTextTool(r, tool, fn); err != nil {
		t.Fatalf("RegisterTypedAsTextTool error: %v", err)
	}

	out, err := r.Call(t.Context(), tool.GoImpl.FuncID, json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("Call unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected no outputs for null JSON, got %#v", out)
	}
}

func TestRegisterOutputsTool_StrictDecode(t *testing.T) {
	type args struct {
		A int `json:"a"`
	}

	tests := []struct {
		name            string
		in              json.RawMessage
		wantErrContains string
		wantText        string
	}{
		{
			name:     "ok",
			in:       json.RawMessage(`{"a":7}`),
			wantText: "7",
		},
		{
			name:            "unknown field rejected",
			in:              json.RawMessage(`{"a":7,"b":1}`),
			wantErrContains: errInvalidInputStr,
		},
		{
			name:            "trailing data rejected",
			in:              json.RawMessage(`{"a":7} true`),
			wantErrContains: errInvalidInputStr,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRegistry()
			if err != nil {
				t.Fatalf("NewRegistry error: %v", err)
			}

			tool := mkTool("github.com/acme/tools.TypedOutputs", "typedoutputs")
			fn := func(_ context.Context, a args) ([]spec.ToolOutputUnion, error) {
				return textOut(strconv.Itoa(a.A)), nil
			}

			if err := RegisterOutputsTool(r, tool, fn); err != nil {
				t.Fatalf("RegisterOutputsTool error: %v", err)
			}

			out, err := r.Call(t.Context(), tool.GoImpl.FuncID, tc.in)
			if tc.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("Call error: got %v want contains %q", err, tc.wantErrContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("Call unexpected error: %v", err)
			}
			if len(out) != 1 || out[0].Kind != spec.ToolOutputKindText || out[0].TextItem == nil {
				t.Fatalf("Call output shape: got %#v want single text output", out)
			}
			if out[0].TextItem.Text != tc.wantText {
				t.Fatalf("Call output text: got %q want %q", out[0].TextItem.Text, tc.wantText)
			}
		})
	}
}

func mkTool(funcID, slug string) spec.Tool {
	return spec.Tool{
		SchemaVersion: spec.SchemaVersion,
		ID:            "0190f3f3-6a2c-7c1a-9f59-aaaaaaaaaaaa",
		Slug:          slug,
		Version:       "v1",
		DisplayName:   slug,
		Description:   "desc",
		ArgSchema:     spec.JSONSchema([]byte(`{}`)),
		GoImpl:        spec.GoToolImpl{FuncID: spec.FuncID(funcID)},
		CreatedAt:     spec.SchemaStartTime,
		ModifiedAt:    spec.SchemaStartTime,
		Tags:          []string{"t1", "t2"},
	}
}

func textOut(s string) []spec.ToolOutputUnion {
	return []spec.ToolOutputUnion{
		{
			Kind: spec.ToolOutputKindText,
			TextItem: &spec.ToolOutputText{
				Text: s,
			},
		},
	}
}
