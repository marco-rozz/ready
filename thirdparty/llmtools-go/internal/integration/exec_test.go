package integration

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/exectool"
	"github.com/flexigpt/llmtools-go/fstool"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

// Shell sessions + env persistence + runscript.

const (
	execTestShellVarName     = "MYVAR"
	execTestShellHelloValue  = "hello"
	execTestShellByeValue    = "bye"
	execTestShellTxtPath     = "shell.txt"
	execTestShellFromShell   = "from-shell"
	execTestShellScriptsDir  = "scripts"
	execTestShellPS1FileName = "hello.ps1"
	execTestShellSHFileName  = "hello.sh"
	execTestShellWorldArg    = "world"
	execTestShellWriteOutput = "Write-Output $env:" + execTestShellVarName
	execTestShellEchoEnv     = "echo \"$" + execTestShellVarName + "\""
	execTestShellSetContent  = "Set-Content -NoNewline -Path \"" + execTestShellTxtPath + "\" -Value \"" + execTestShellFromShell + "\""
	execTestShellPrintf      = "printf \"%s\" \"" + execTestShellFromShell + "\" > " + execTestShellTxtPath
	execTestShellPS1Content  = "param([string]$Name)\n" +
		"Write-Output \"NAME=$Name\"\n" +
		"Write-Output \"ENV=$env:" + execTestShellVarName + "\"\n"
	execTestShellSHContent = "echo \"NAME=$1\"\n" +
		"echo \"ENV=$" + execTestShellVarName + "\"\n"
)

func TestE2E_Exec_ShellCommand_SessionEnvWorkdir(t *testing.T) {
	base := t.TempDir()
	h := newHarness(t, base)

	var shell exectool.ShellName
	var cmds1, cmds2, cmds3 []string
	var expectEnv1, expectEnv2 string

	if runtime.GOOS == toolutil.GOOSWindows {
		shell = exectool.ShellNamePowershell
		cmds1 = []string{
			execTestShellSetContent,
			execTestShellWriteOutput,
		}
		cmds2 = []string{
			execTestShellWriteOutput,
		}
		cmds3 = []string{
			execTestShellWriteOutput,
		}
		expectEnv1 = execTestShellHelloValue
		expectEnv2 = execTestShellByeValue
	} else {
		shell = exectool.ShellNameSh
		cmds1 = []string{
			execTestShellPrintf,
			execTestShellEchoEnv,
		}
		cmds2 = []string{execTestShellEchoEnv}
		cmds3 = []string{execTestShellEchoEnv}
		expectEnv1 = execTestShellHelloValue
		expectEnv2 = execTestShellByeValue
	}

	// 1) First call: create session + set env.
	resp1 := callJSON[exectool.ShellCommandOut](t, h.r, integrationToolSlugShellCommand, exectool.ShellCommandArgs{
		Shell:    shell,
		Commands: cmds1,
		Env:      map[string]string{execTestShellVarName: execTestShellHelloValue},
	})
	if strings.TrimSpace(resp1.SessionID) == "" {
		t.Fatalf("expected sessionID, got: %s", debugJSON(t, resp1))
	}
	if len(resp1.Results) < 2 {
		t.Fatalf("expected >=2 results, got: %s", debugJSON(t, resp1))
	}
	if !strings.Contains(resp1.Results[len(resp1.Results)-1].Stdout, expectEnv1) {
		t.Fatalf("expected stdout to contain %q, got: %s", expectEnv1, debugJSON(t, resp1.Results))
	}

	// Verify file created via shell by reading it with readfile.
	out := callRaw(
		t,
		h.r,
		integrationToolSlugReadFile,
		fstool.ReadFileArgs{Path: execTestShellTxtPath, Encoding: integrationEncodingText},
	)
	got := requireSingleTextOutput(t, out)
	if got != execTestShellFromShell {
		t.Fatalf("shell.txt content mismatch: got=%q want=%q", got, execTestShellFromShell)
	}

	// 2) Second call: same session, no env provided => session env should persist.
	resp2 := callJSON[exectool.ShellCommandOut](t, h.r, integrationToolSlugShellCommand, exectool.ShellCommandArgs{
		Shell:     shell,
		Commands:  cmds2,
		SessionID: resp1.SessionID,
	})
	if !strings.Contains(resp2.Results[0].Stdout, expectEnv1) {
		t.Fatalf("expected persisted env stdout to contain %q, got: %s", expectEnv1, debugJSON(t, resp2))
	}

	// 3) Third call: override env, and that override becomes part of session state.
	resp3 := callJSON[exectool.ShellCommandOut](t, h.r, integrationToolSlugShellCommand, exectool.ShellCommandArgs{
		Shell:     shell,
		Commands:  cmds2,
		Env:       map[string]string{execTestShellVarName: execTestShellByeValue},
		SessionID: resp1.SessionID,
	})
	if !strings.Contains(resp3.Results[0].Stdout, expectEnv2) {
		t.Fatalf("expected overridden env stdout to contain %q, got: %s", expectEnv2, debugJSON(t, resp3))
	}

	// 4) Fourth call: no env passed => should still be "bye".
	resp4 := callJSON[exectool.ShellCommandOut](t, h.r, integrationToolSlugShellCommand, exectool.ShellCommandArgs{
		Shell:     shell,
		Commands:  cmds3,
		SessionID: resp1.SessionID,
	})
	if !strings.Contains(resp4.Results[0].Stdout, expectEnv2) {
		t.Fatalf("expected session env stdout to contain %q, got: %s", expectEnv2, debugJSON(t, resp4))
	}
}

func TestE2E_Exec_RunScript(t *testing.T) {
	base := t.TempDir()

	// Make runscript robust on Windows by forcing .ps1 to run with ExecutionPolicy Bypass.
	var execOpts []exectool.ExecToolOption
	if runtime.GOOS == toolutil.GOOSWindows {
		pol := exectool.DefaultRunScriptPolicy()
		if pol.InterpreterByExtension == nil {
			pol.InterpreterByExtension = map[string]exectool.RunScriptInterpreter{}
		}
		pol.InterpreterByExtension[".ps1"] = exectool.RunScriptInterpreter{
			Shell:   exectool.ShellNamePwsh,
			Mode:    exectool.RunScriptModeInterpreter,
			Command: "powershell",
			Args:    []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File"},
		}
		execOpts = append(execOpts, exectool.WithRunScriptPolicy(pol))
	}

	h := newHarness(t, base, execOpts...)

	scriptsDirRel := execTestShellScriptsDir

	if runtime.GOOS == toolutil.GOOSWindows {
		scriptRel := filepath.Join(scriptsDirRel, execTestShellPS1FileName)

		_ = callJSON[fstool.WriteFileOut](t, h.r, integrationToolSlugWriteFile, fstool.WriteFileArgs{
			Path:          scriptRel,
			Encoding:      integrationEncodingText,
			Content:       execTestShellPS1Content,
			CreateParents: true,
		})

		res := callJSON[exectool.RunScriptOut](t, h.r, "runscript", exectool.RunScriptArgs{
			Path:    execTestShellPS1FileName,
			Args:    []string{execTestShellWorldArg},
			Env:     map[string]string{execTestShellVarName: execTestShellHelloValue},
			WorkDir: scriptsDirRel,
		})
		if res.ExitCode != 0 {
			t.Fatalf("runscript exit != 0: %s", debugJSON(t, res))
		}
		if !strings.Contains(res.Stdout, "NAME=world") || !strings.Contains(res.Stdout, "ENV=hello") {
			t.Fatalf("unexpected stdout: %s", debugJSON(t, res))
		}
	} else {
		scriptRel := filepath.Join(scriptsDirRel, execTestShellSHFileName)

		_ = callJSON[fstool.WriteFileOut](t, h.r, integrationToolSlugWriteFile, fstool.WriteFileArgs{
			Path:          scriptRel,
			Encoding:      integrationEncodingText,
			Content:       execTestShellSHContent,
			CreateParents: true,
		})

		res := callJSON[exectool.RunScriptOut](t, h.r, "runscript", exectool.RunScriptArgs{
			Path:    execTestShellSHFileName,
			Args:    []string{execTestShellWorldArg},
			Env:     map[string]string{execTestShellVarName: execTestShellHelloValue},
			WorkDir: scriptsDirRel,
		})
		if res.ExitCode != 0 {
			t.Fatalf("runscript exit != 0: %s", debugJSON(t, res))
		}
		if !strings.Contains(res.Stdout, "NAME=world") || !strings.Contains(res.Stdout, "ENV=hello") {
			t.Fatalf("unexpected stdout: %s", debugJSON(t, res))
		}
	}
}
