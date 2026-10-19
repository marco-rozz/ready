package exectool

import "testing"

func TestWithBaseEnvClonesInput(t *testing.T) {
	baseEnv := map[string]string{"BASE": "1"}

	et, err := NewExecTool(WithDefaultShell(ShellNameAuto), WithBaseEnv(baseEnv))
	if err != nil {
		t.Fatalf("NewExecTool: %v", err)
	}

	baseEnv["BASE"] = "2"
	if et.cfg.baseEnv["BASE"] != "1" {
		t.Fatalf("expected cloned base env, got %q", et.cfg.baseEnv["BASE"])
	}
}

func TestWithRunScriptPolicyClonesInput(t *testing.T) {
	rsPol := RunScriptPolicy{
		AllowedExtensions: []string{" SH ", ".PY"},
		InterpreterByExtension: map[string]RunScriptInterpreter{
			" SH ": {
				Shell: ShellNameSh,
				Mode:  RunScriptModeShell,
				Args:  []string{"-x"},
			},
		},
	}

	et, err := NewExecTool(
		WithDefaultShell(ShellNameAuto),
		WithBaseEnv(nil),
		WithRunScriptPolicy(rsPol),
	)
	if err != nil {
		t.Fatalf("NewExecTool: %v", err)
	}

	rsPol.AllowedExtensions[0] = "changed"
	tmp := rsPol.InterpreterByExtension[" SH "]
	tmp.Args[0] = "-y"
	rsPol.InterpreterByExtension[" SH "] = tmp

	if et.cfg.runScriptPolicy.AllowedExtensions[0] != ".sh" {
		t.Fatalf("expected normalized cloned extensions, got %#v", et.cfg.runScriptPolicy.AllowedExtensions)
	}
	if et.cfg.runScriptPolicy.InterpreterByExtension[".sh"].Args[0] != "-x" {
		t.Fatalf("expected cloned interpreter args, got %#v", et.cfg.runScriptPolicy.InterpreterByExtension[".sh"].Args)
	}
}
