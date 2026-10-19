package exectool

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

func TestDefaultExecutionPolicy(t *testing.T) {
	got := DefaultExecutionPolicy()
	want := ExecutionPolicy{
		AllowDangerous:   false,
		Timeout:          defaultExecutionTimeout,
		MaxOutputBytes:   defaultExecutionMaxOutputBytes,
		MaxCommands:      defaultExecutionMaxCommands,
		MaxCommandLength: defaultExecutionMaxCommandLength,
	}
	if got != want {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestExecutionPolicyClone(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		var p *ExecutionPolicy
		if got := p.Clone(); got != nil {
			t.Fatalf("expected nil clone, got %#v", got)
		}
	})

	t.Run("value_copy", func(t *testing.T) {
		in := &ExecutionPolicy{
			AllowDangerous:   true,
			Timeout:          time.Second,
			MaxOutputBytes:   1234,
			MaxCommands:      7,
			MaxCommandLength: 99,
		}
		got := in.Clone()
		if got == nil || got == in {
			t.Fatalf("expected independent clone, got %#v", got)
		}
		if *got != *in {
			t.Fatalf("clone mismatch: got %#v want %#v", got, in)
		}

		in.AllowDangerous = false
		in.Timeout = 2 * time.Second
		if !got.AllowDangerous || got.Timeout != time.Second {
			t.Fatalf("clone should not track source mutations: %#v", got)
		}
	})
}

func TestExecToolPolicyClone(t *testing.T) {
	root := t.TempDir()
	fsPol, err := fspolicy.New(root, []string{root}, true)
	if err != nil {
		t.Fatalf("fspolicy.New: %v", err)
	}

	in := &execToolPolicy{
		fsPolicy: fsPol,
		blockedCommands: map[string]struct{}{
			"foo": {},
		},
		defaultShell: ShellNameSh,
		baseEnv: map[string]string{
			"BASE": "1",
		},
		executionPolicy: ExecutionPolicy{
			AllowDangerous:   true,
			Timeout:          2 * time.Second,
			MaxOutputBytes:   321,
			MaxCommands:      4,
			MaxCommandLength: 5,
		},
		runScriptPolicy: RunScriptPolicy{
			AllowedExtensions: []string{".sh", ".py"},
			InterpreterByExtension: map[string]RunScriptInterpreter{
				".sh": {
					Shell: ShellNameSh,
					Mode:  RunScriptModeShell,
					Args:  []string{"-x"},
				},
			},
			ExecutionPolicy: ExecutionPolicy{Timeout: time.Second},
			MaxArgs:         2,
			MaxArgBytes:     3,
		},
	}

	got := in.Clone()
	if got == nil || got == in {
		t.Fatalf("expected independent clone, got %#v", got)
	}
	if got.defaultShell != in.defaultShell || got.executionPolicy != in.executionPolicy {
		t.Fatalf("scalar fields mismatch: got %#v want %#v", got, in)
	}
	if got.fsPolicy.WorkBaseDir() != in.fsPolicy.WorkBaseDir() ||
		got.fsPolicy.BlockSymlinks() != in.fsPolicy.BlockSymlinks() {
		t.Fatalf("fsPolicy should be copied by value")
	}
	if !reflect.DeepEqual(got.baseEnv, in.baseEnv) {
		t.Fatalf("baseEnv got %#v want %#v", got.baseEnv, in.baseEnv)
	}
	if !reflect.DeepEqual(got.blockedCommands, in.blockedCommands) {
		t.Fatalf("blockedCommands got %#v want %#v", got.blockedCommands, in.blockedCommands)
	}
	if !reflect.DeepEqual(got.runScriptPolicy.AllowedExtensions, in.runScriptPolicy.AllowedExtensions) {
		t.Fatalf(
			"runScriptPolicy extensions got %#v want %#v",
			got.runScriptPolicy.AllowedExtensions,
			in.runScriptPolicy.AllowedExtensions,
		)
	}
	if got.runScriptPolicy.InterpreterByExtension[".sh"].Args[0] != "-x" {
		t.Fatalf("expected cloned args, got %#v", got.runScriptPolicy.InterpreterByExtension[".sh"].Args)
	}

	in.blockedCommands["bar"] = struct{}{}
	in.baseEnv["BASE"] = "2"
	in.runScriptPolicy.AllowedExtensions[0] = ".txt"
	interp := in.runScriptPolicy.InterpreterByExtension[".sh"]
	interp.Args[0] = "-y"
	in.runScriptPolicy.InterpreterByExtension[".sh"] = interp
	in.executionPolicy.Timeout = 3 * time.Second

	if _, ok := got.blockedCommands["bar"]; ok {
		t.Fatalf("clone should not track blocked command mutations")
	}
	if got.baseEnv["BASE"] != "1" {
		t.Fatalf("clone should not track baseEnv mutations, got %q", got.baseEnv["BASE"])
	}
	if got.runScriptPolicy.AllowedExtensions[0] != ".sh" {
		t.Fatalf("clone should not track runScriptPolicy mutations: %#v", got.runScriptPolicy.AllowedExtensions)
	}
	if got.runScriptPolicy.InterpreterByExtension[".sh"].Args[0] != "-x" {
		t.Fatalf(
			"clone should not track interpreter arg mutations: %#v",
			got.runScriptPolicy.InterpreterByExtension[".sh"].Args,
		)
	}
	if got.executionPolicy.Timeout != 2*time.Second {
		t.Fatalf("clone should not track execution policy mutations: %#v", got.executionPolicy)
	}
}

func TestNewExecTool_OptionsAndSnapshotPolicy(t *testing.T) {
	root := t.TempDir()
	baseEnv := map[string]string{"BASE": "1"}
	rsPol := RunScriptPolicy{
		AllowedExtensions: []string{" SH ", ".PY", "sh", "Bash"},
		InterpreterByExtension: map[string]RunScriptInterpreter{
			//nolint:gocritic // Intentional.
			" SH ": {
				Shell: ShellName(" sh "),
				Mode:  RunScriptModeShell,
				Args:  []string{"-x"},
			},
			"py": {
				Shell:   ShellName(" "),
				Mode:    RunScriptModeInterpreter,
				Command: " python3 ",
				Args:    []string{"-u"},
			},
		},
	}
	execPol := ExecutionPolicy{
		AllowDangerous:   true,
		Timeout:          3 * time.Second,
		MaxOutputBytes:   4096,
		MaxCommands:      3,
		MaxCommandLength: 99,
	}

	et, err := NewExecTool(
		WithDefaultShell(ShellNameAuto),
		WithBaseEnv(baseEnv),
		WithAllowedRoots([]string{root}),
		WithWorkBaseDir(root),
		WithBlockSymlinks(true),
		WithBlockedCommands([]string{"/bin/FOO"}),
		WithExecutionPolicy(execPol),
		WithRunScriptPolicy(rsPol),
	)
	if err != nil {
		t.Fatalf("NewExecTool: %v", err)
	}

	if !et.cfg.defaultShellSet || et.cfg.defaultShell != ShellNameAuto {
		t.Fatalf("unexpected default shell config: %#v", et.cfg)
	}
	if !et.cfg.baseEnvSet || et.cfg.baseEnv["BASE"] != "1" {
		t.Fatalf("unexpected base env config: %#v", et.cfg.baseEnv)
	}
	if !et.cfg.blockSymlinks {
		t.Fatalf("expected blockSymlinks config to be set")
	}
	if et.toolPolicy == nil {
		t.Fatalf("expected toolPolicy to be initialized")
	}
	canonicalize := func(p string) string {
		p = filepath.Clean(p)
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		return p
	}

	if got, want := canonicalize(et.toolPolicy.fsPolicy.WorkBaseDir()), canonicalize(root); got != want {
		t.Fatalf("fsPolicy.WorkBaseDir got %q want %q", got, want)
	}
	if !et.toolPolicy.fsPolicy.HasAllowedRoots() {
		t.Fatalf("expected allowed roots to be configured")
	}
	roots := et.toolPolicy.fsPolicy.AllowedRoots()
	if len(roots) != 1 || canonicalize(roots[0]) != canonicalize(root) {
		t.Fatalf("unexpected allowed roots: %#v", canonicalize(roots[0]))
	}
	if !et.toolPolicy.fsPolicy.BlockSymlinks() {
		t.Fatalf("expected fs policy to block symlinks")
	}
	if _, ok := et.toolPolicy.blockedCommands["foo"]; !ok {
		t.Fatalf("expected extra blocked command to be normalized and stored: %#v", et.toolPolicy.blockedCommands)
	}
	if et.toolPolicy.executionPolicy != execPol {
		t.Fatalf("execution policy got %#v want %#v", et.toolPolicy.executionPolicy, execPol)
	}
	if !reflect.DeepEqual(et.toolPolicy.runScriptPolicy.AllowedExtensions, []string{".sh", ".py", ".bash"}) {
		t.Fatalf("unexpected normalized extensions: %#v", et.toolPolicy.runScriptPolicy.AllowedExtensions)
	}
	if got := et.toolPolicy.runScriptPolicy.InterpreterByExtension[".sh"]; got.Shell != ShellNameSh ||
		got.Mode != RunScriptModeShell ||
		!reflect.DeepEqual(got.Args, []string{"-x"}) {
		t.Fatalf("normalized .sh interpreter got %+v", got)
	}
	if got := et.toolPolicy.runScriptPolicy.InterpreterByExtension[".py"]; got.Shell != ShellNameAuto ||
		got.Mode != RunScriptModeInterpreter ||
		got.Command != "python3" ||
		!reflect.DeepEqual(got.Args, []string{"-u"}) {
		t.Fatalf("normalized .py interpreter got %+v", got)
	}
	if et.RunScriptTool().Slug != runScriptToolSpec.Slug {
		t.Fatalf("unexpected RunScriptTool slug")
	}
	if et.ShellCommandTool().Slug != shellCommandToolSpec.Slug {
		t.Fatalf("unexpected ShellCommandTool slug")
	}

	snapshot := et.snapshotPolicy()
	if snapshot == nil {
		t.Fatalf("expected snapshot")
	}

	// Mutate the instance and ensure the policy snapshot stays stable.
	et.cfg.baseEnv["BASE"] = "2"
	et.cfg.blockedCommands["bar"] = struct{}{}
	et.cfg.runScriptPolicy.AllowedExtensions[0] = ".txt"
	et.toolPolicy.baseEnv["BASE"] = "3"
	et.toolPolicy.blockedCommands["baz"] = struct{}{}
	et.toolPolicy.runScriptPolicy.AllowedExtensions[0] = ".ps1"
	interp := et.toolPolicy.runScriptPolicy.InterpreterByExtension[".sh"]
	interp.Args[0] = "-y"
	et.toolPolicy.runScriptPolicy.InterpreterByExtension[".sh"] = interp

	if snapshot.baseEnv["BASE"] != "1" {
		t.Fatalf("snapshot baseEnv should be stable, got %q", snapshot.baseEnv["BASE"])
	}
	if _, ok := snapshot.blockedCommands["bar"]; ok {
		t.Fatalf("snapshot should not see cfg blockedCommands mutations")
	}
	if _, ok := snapshot.blockedCommands["baz"]; ok {
		t.Fatalf("snapshot should not see toolPolicy blockedCommands mutations")
	}
	if !reflect.DeepEqual(snapshot.runScriptPolicy.AllowedExtensions, []string{".sh", ".py", ".bash"}) {
		t.Fatalf("snapshot runScriptPolicy extensions changed: %#v", snapshot.runScriptPolicy.AllowedExtensions)
	}
	if snapshot.runScriptPolicy.InterpreterByExtension[".sh"].Args[0] != "-x" {
		t.Fatalf(
			"snapshot runScriptPolicy args changed: %#v",
			snapshot.runScriptPolicy.InterpreterByExtension[".sh"].Args,
		)
	}
}

func TestNewExecTool_OptionValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		opts    []ExecToolOption
		wantErr string
	}{
		{
			name:    "invalid_default_shell",
			opts:    []ExecToolOption{WithDefaultShell(ShellName(" nope "))},
			wantErr: "invalid shell",
		},
		{
			name:    "invalid_base_env",
			opts:    []ExecToolOption{WithDefaultShell(ShellNameAuto), WithBaseEnv(map[string]string{"": "1"})},
			wantErr: "env",
		},
		{
			name: "invalid_blocked_command",
			opts: []ExecToolOption{
				WithDefaultShell(ShellNameAuto),
				WithBaseEnv(nil),
				WithBlockedCommands([]string{"rm -rf"}),
			},
			wantErr: "whitespace",
		},
		{
			name: "invalid_runscript_policy",
			opts: []ExecToolOption{
				WithDefaultShell(ShellNameAuto),
				WithBaseEnv(nil),
				WithRunScriptPolicy(RunScriptPolicy{MaxArgs: -1}),
			},
			wantErr: "MaxArgs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewExecTool(tc.opts...); err == nil {
				t.Fatalf("expected error")
			} else if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErr)) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestSessionOptionsInitializeStore(t *testing.T) {
	var et ExecTool
	if err := WithSessionTTL(0)(&et); err != nil {
		t.Fatalf("WithSessionTTL: %v", err)
	}
	if et.sessions == nil {
		t.Fatalf("WithSessionTTL should initialize session store")
	}
	if err := WithMaxSessions(2)(&et); err != nil {
		t.Fatalf("WithMaxSessions: %v", err)
	}
	if et.sessions == nil {
		t.Fatalf("WithMaxSessions should keep session store initialized")
	}
	if got := et.sessions.Size(); got != 0 {
		t.Fatalf("expected empty session store, got %d", got)
	}
}
