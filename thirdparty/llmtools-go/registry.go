package llmtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/flexigpt/llmtools-go/exectool"
	"github.com/flexigpt/llmtools-go/fstool"
	"github.com/flexigpt/llmtools-go/imagetool"
	"github.com/flexigpt/llmtools-go/internal/jsonutil"
	"github.com/flexigpt/llmtools-go/internal/logutil"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
	"github.com/flexigpt/llmtools-go/texttool"
)

const defaultBuiltinCallTimeout = 10 * time.Minute

// Registry provides lookup/register for Go tools by funcID, with json.RawMessage I/O.
type Registry struct {
	mu     sync.RWMutex
	logger *slog.Logger

	toolMap     map[spec.FuncID]spec.ToolFunc
	toolSpecMap map[spec.FuncID]spec.Tool

	timeout time.Duration
}

type RegistryOption func(*Registry) error

// NewBuiltinRegistry returns a Registry with all built-in tools registered.
// By default it applies a 10mins timeout, but callers can override it by passing
// WithDefaultCallTimeout as a later option. This is a default, not a hard cap.
func NewBuiltinRegistry(opts ...RegistryOption) (*Registry, error) {
	defaults := make([]RegistryOption, 0, 1+len(opts))
	defaults = append(defaults, WithDefaultCallTimeout(defaultBuiltinCallTimeout))
	defaults = append(defaults, opts...)
	r, err := NewRegistry(defaults...)
	if err != nil {
		return nil, err
	}
	if err := RegisterBuiltins(r); err != nil {
		return nil, err
	}
	return r, nil
}

func WithDefaultCallTimeout(d time.Duration) RegistryOption {
	return func(gr *Registry) error {
		gr.timeout = d
		return nil
	}
}

func WithLogger(logger *slog.Logger) RegistryOption {
	return func(ps *Registry) error {
		ps.logger = logger
		return nil
	}
}

func NewRegistry(opts ...RegistryOption) (*Registry, error) {
	r := &Registry{
		toolMap:     make(map[spec.FuncID]spec.ToolFunc),
		toolSpecMap: make(map[spec.FuncID]spec.Tool),
	}
	for _, o := range opts {
		if err := o(r); err != nil {
			return nil, err
		}
	}
	if r.logger != nil {
		logutil.SetDefault(r.logger)
	} else {
		logutil.SetDefault(nil)
	}
	return r, nil
}

// RegisterBuiltins registers the built-in tools into r.
func RegisterBuiltins(r *Registry) error {
	ft, err := fstool.NewFSTool()
	if err != nil {
		return err
	}

	if err := RegisterOutputsTool(r, ft.ReadFileTool(), ft.ReadFile); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, ft.SearchFilesTool(), ft.SearchFiles); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, ft.WriteFileTool(), ft.WriteFile); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, ft.DeleteFileTool(), ft.DeleteFile); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, ft.ListDirectoryTool(), ft.ListDirectory); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, ft.StatPathTool(), ft.StatPath); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, ft.MIMEForPathTool(), ft.MIMEForPath); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, ft.MIMEForExtensionTool(), ft.MIMEForExtension); err != nil {
		return err
	}

	it, err := imagetool.NewImageTool()
	if err != nil {
		return err
	}

	if err := RegisterTypedAsTextTool(r, it.ReadImageTool(), it.ReadImage); err != nil {
		return err
	}

	et, err := exectool.NewExecTool(
	// Defaults are fine for builtins; hosts should instantiate their own tool with custom policy/sessions/env/workdir
	// settings as needed.
	)
	if err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, et.ShellCommandTool(), et.ShellCommand); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, et.RunScriptTool(), et.RunScript); err != nil {
		return err
	}

	tt, err := texttool.NewTextTool()
	if err != nil {
		return err
	}

	if err := RegisterTypedAsTextTool(r, tt.ReadTextRangeTool(), tt.ReadTextRange); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, tt.FindTextTool(), tt.FindText); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, tt.InsertTextTool(), tt.InsertText); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, tt.ReplaceTextTool(), tt.ReplaceText); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, tt.DeleteTextTool(), tt.DeleteText); err != nil {
		return err
	}
	if err := RegisterTypedAsTextTool(r, tt.ApplyUnifiedDiffTool(), tt.ApplyUnifiedDiff); err != nil {
		return err
	}

	return nil
}

// RegisterOutputsTool registers a typed tool function that directly returns []ToolOutputUnion.
// This is a function and not a method on struct as methods cannot have type params in go.
func RegisterOutputsTool[T any](
	r *Registry,
	tool spec.Tool,
	fn func(context.Context, T) ([]spec.ToolOutputUnion, error),
) error {
	return r.RegisterTool(tool, typedToOutputs(fn))
}

// RegisterTypedAsTextTool registers a typed tool function whose output R is JSON-encodable.
// The JSON representation of R is wrapped into a single text block.
// This is a function and not a method on struct as methods cannot have type params in go.
func RegisterTypedAsTextTool[T, R any](
	r *Registry,
	tool spec.Tool,
	fn func(context.Context, T) (R, error),
) error {
	return r.RegisterTool(tool, typedToText(fn))
}

func (r *Registry) RegisterTool(tool spec.Tool, fn spec.ToolFunc) error {
	if tool.GoImpl.FuncID == "" {
		return errors.New("invalid tool: missing funcID")
	}

	if tool.SchemaVersion == "" {
		return errors.New("invalid tool: missing schemaVersion")
	}
	if tool.SchemaVersion != spec.SchemaVersion {
		return fmt.Errorf(
			"invalid tool: schemaVersion %q does not match library schemaVersion %q",
			tool.SchemaVersion,
			spec.SchemaVersion,
		)
	}
	if len(tool.ArgSchema) > 0 && !json.Valid(tool.ArgSchema) {
		return errors.New("invalid tool: argSchema is not valid JSON")
	}
	if fn == nil {
		return errors.New("invalid tool: nil func")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.toolMap[tool.GoImpl.FuncID]; exists {
		return fmt.Errorf("go-tool already registered: %s", tool.GoImpl.FuncID)
	}
	r.toolMap[tool.GoImpl.FuncID] = fn
	r.toolSpecMap[tool.GoImpl.FuncID] = toolutil.CloneTool(tool)

	return nil
}

type callOptions struct {
	timeout *time.Duration
}

// CallOption configures per-call behavior.
type CallOption func(*callOptions)

// WithCallTimeout overrides the timeout for this single call.
// 0 means "no timeout" for this call (even if tool/registry default is non-zero).
func WithCallTimeout(d time.Duration) CallOption {
	dd := d
	return func(o *callOptions) {
		o.timeout = &dd
	}
}

func (r *Registry) Call(
	ctx context.Context,
	funcID spec.FuncID,
	in json.RawMessage,
	callOpts ...CallOption,
) ([]spec.ToolOutputUnion, error) {
	return toolutil.WithRecoveryResp(func() ([]spec.ToolOutputUnion, error) {
		var co callOptions
		for _, o := range callOpts {
			if o != nil {
				o(&co)
			}
		}

		// Resolve timeout: call override > registry default.
		r.mu.RLock()
		effectiveTimeout := r.timeout
		if co.timeout != nil {
			effectiveTimeout = *co.timeout
		}
		r.mu.RUnlock()

		// Treat negative like "no timeout" (avoid surprising immediate cancellation).
		if effectiveTimeout < 0 {
			effectiveTimeout = 0
		}

		fnCtx := ctx
		if effectiveTimeout > 0 {
			var cancel context.CancelFunc
			fnCtx, cancel = context.WithTimeout(ctx, effectiveTimeout)
			defer cancel()
		}

		fn, ok := r.Lookup(funcID)
		if !ok {
			return nil, fmt.Errorf("unknown tool: %s", funcID)
		}
		return fn(fnCtx, in)
	})
}

func (r *Registry) Lookup(funcID spec.FuncID) (spec.ToolFunc, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.toolMap[funcID]
	return fn, ok
}

func (r *Registry) Tools() []spec.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]spec.Tool, 0, len(r.toolSpecMap))
	for _, t := range r.toolSpecMap {
		out = append(out, toolutil.CloneTool(t))
	}
	sort.Slice(out, func(i, j int) bool {
		// Stable tool manifests matter for prompts and tests.
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		return out[i].GoImpl.FuncID < out[j].GoImpl.FuncID
	})
	return out
}

// typedToOutputs wraps a typed function (ctx, T) -> ([]ToolOutputUnion, error)
// into a spec.ToolFunc that strictly decodes input into T.
func typedToOutputs[T any](
	fn func(context.Context, T) ([]spec.ToolOutputUnion, error),
) spec.ToolFunc {
	return func(ctx context.Context, in json.RawMessage) ([]spec.ToolOutputUnion, error) {
		// Decode input strictly into T (rejects unknown fields and trailing data).
		args, err := jsonutil.DecodeJSONRaw[T](in)
		if err != nil {
			return nil, fmt.Errorf("invalid input: %w", err)
		}
		return fn(ctx, args)
	}
}

// typedToText wraps a typed function (ctx, T) -> (R, error) into a spec.ToolFunc
// that JSON-encodes R and returns it as a single text output block.
func typedToText[T, R any](fn func(context.Context, T) (R, error)) spec.ToolFunc {
	return func(ctx context.Context, in json.RawMessage) ([]spec.ToolOutputUnion, error) {
		// Decode input strictly into T (rejects unknown fields and trailing data).
		args, err := jsonutil.DecodeJSONRaw[T](in)
		if err != nil {
			return nil, fmt.Errorf("invalid input: %w", err)
		}

		out, err := fn(ctx, args)
		if err != nil {
			return nil, err
		}
		raw, err := jsonutil.EncodeToJSONRaw(out)
		if err != nil {
			return nil, fmt.Errorf("encode output: %w", err)
		}

		text := string(raw)
		if text == "" || text == "null" {
			return nil, nil
		}
		return []spec.ToolOutputUnion{
			{
				Kind: spec.ToolOutputKindText,
				TextItem: &spec.ToolOutputText{
					Text: text,
				},
			},
		}, nil
	}
}
