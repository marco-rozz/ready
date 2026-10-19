package integration

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go"
	"github.com/flexigpt/llmtools-go/exectool"
	"github.com/flexigpt/llmtools-go/fstool"
	"github.com/flexigpt/llmtools-go/imagetool"
	"github.com/flexigpt/llmtools-go/spec"
	"github.com/flexigpt/llmtools-go/texttool"
)

const (
	integrationToolSlugReadFile     = "readfile"
	integrationToolSlugWriteFile    = "writefile"
	integrationToolSlugShellCommand = "shellcommand"
	integrationEncodingText         = "text"
	integrationEncodingBinary       = "binary"
)

type harness struct {
	t    *testing.T
	base string
	r    *llmtools.Registry
}

func newHarness(t *testing.T, base string, execOpts ...exectool.ExecToolOption) *harness {
	t.Helper()

	r, err := llmtools.NewRegistry(
		llmtools.WithDefaultCallTimeout(30 * time.Second),
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Filesystem tools (sandboxed).
	ft, err := fstool.NewFSTool(
		fstool.WithWorkBaseDir(base),
		fstool.WithAllowedRoots([]string{base}),
	)
	if err != nil {
		t.Fatalf("NewFSTool: %v", err)
	}
	if err := llmtools.RegisterOutputsTool(r, ft.ReadFileTool(), ft.ReadFile); err != nil {
		t.Fatalf("register readfile: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, ft.WriteFileTool(), ft.WriteFile); err != nil {
		t.Fatalf("register writefile: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, ft.DeleteFileTool(), ft.DeleteFile); err != nil {
		t.Fatalf("register deletefile: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, ft.ListDirectoryTool(), ft.ListDirectory); err != nil {
		t.Fatalf("register listdirectory: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, ft.SearchFilesTool(), ft.SearchFiles); err != nil {
		t.Fatalf("register searchfiles: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, ft.StatPathTool(), ft.StatPath); err != nil {
		t.Fatalf("register statpath: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, ft.MIMEForPathTool(), ft.MIMEForPath); err != nil {
		t.Fatalf("register mimeforpath: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, ft.MIMEForExtensionTool(), ft.MIMEForExtension); err != nil {
		t.Fatalf("register mimeforextension: %v", err)
	}

	// Image tool.
	it, err := imagetool.NewImageTool(
		imagetool.WithWorkBaseDir(base),
		imagetool.WithAllowedRoots([]string{base}),
	)
	if err != nil {
		t.Fatalf("NewImageTool: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, it.ReadImageTool(), it.ReadImage); err != nil {
		t.Fatalf("register readimage: %v", err)
	}

	// Exec tool (sandboxed).
	execOpts = append([]exectool.ExecToolOption{
		exectool.WithWorkBaseDir(base),
		exectool.WithAllowedRoots([]string{base}),
	}, execOpts...)

	et, err := exectool.NewExecTool(execOpts...)
	if err != nil {
		t.Fatalf("NewExecTool: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, et.ShellCommandTool(), et.ShellCommand); err != nil {
		t.Fatalf("register shellcommand: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, et.RunScriptTool(), et.RunScript); err != nil {
		t.Fatalf("register runscript: %v", err)
	}

	// Text tools (sandboxed).
	tt, err := texttool.NewTextTool(
		texttool.WithWorkBaseDir(base),
		texttool.WithAllowedRoots([]string{base}),
	)
	if err != nil {
		t.Fatalf("NewTextTool: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, tt.ReadTextRangeTool(), tt.ReadTextRange); err != nil {
		t.Fatalf("register readtextrange: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, tt.FindTextTool(), tt.FindText); err != nil {
		t.Fatalf("register findtext: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, tt.InsertTextTool(), tt.InsertText); err != nil {
		t.Fatalf("register inserttext: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, tt.ReplaceTextTool(), tt.ReplaceText); err != nil {
		t.Fatalf("register replacetext: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, tt.DeleteTextTool(), tt.DeleteText); err != nil {
		t.Fatalf("register deletetext: %v", err)
	}
	if err := llmtools.RegisterTypedAsTextTool(r, tt.ApplyUnifiedDiffTool(), tt.ApplyUnifiedDiff); err != nil {
		t.Fatalf("register applyunifieddiff: %v", err)
	}
	return &harness{t: t, base: base, r: r}
}

func callJSON[T any](t *testing.T, r *llmtools.Registry, slug string, args any) T {
	t.Helper()

	rawOut := callRaw(t, r, slug, args)
	if len(rawOut) != 1 || rawOut[0].Kind != spec.ToolOutputKindText || rawOut[0].TextItem == nil {
		t.Fatalf("expected single text output for %s, got: %+v", slug, rawOut)
	}
	var decoded T
	if err := json.Unmarshal([]byte(rawOut[0].TextItem.Text), &decoded); err != nil {
		t.Fatalf("unmarshal %s output JSON: %v\nraw=%s", slug, err, rawOut[0].TextItem.Text)
	}
	return decoded
}

func callRaw(t *testing.T, r *llmtools.Registry, slug string, args any) []spec.ToolOutputUnion {
	t.Helper()

	in, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args for %s: %v", slug, err)
	}

	out, err := r.Call(t.Context(), funcIDBySlug(t, r, slug), in)
	if err != nil {
		t.Fatalf("Call(%s): %v", slug, err)
	}
	return out
}

func funcIDBySlug(t *testing.T, r *llmtools.Registry, slug string) spec.FuncID {
	t.Helper()
	for _, tool := range r.Tools() {
		if tool.Slug == slug {
			return tool.GoImpl.FuncID
		}
	}
	t.Fatalf("tool slug not registered: %q", slug)
	return ""
}

func requireSingleTextOutput(t *testing.T, out []spec.ToolOutputUnion) string {
	t.Helper()
	if len(out) != 1 || out[0].Kind != spec.ToolOutputKindText || out[0].TextItem == nil {
		t.Fatalf("expected single text output, got: %+v", out)
	}
	return out[0].TextItem.Text
}

func requireKind(
	t *testing.T,
	out []spec.ToolOutputUnion,
	kind spec.ToolOutputKind,
) spec.ToolOutputUnion {
	t.Helper()
	for _, item := range out {
		if item.Kind == kind {
			return item
		}
	}
	t.Fatalf("expected output kind %q, got: %+v", kind, out)
	return spec.ToolOutputUnion{}
}

func debugJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("<json error: %v>", err)
	}
	return string(b)
}
