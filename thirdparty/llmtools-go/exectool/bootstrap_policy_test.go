package exectool

import (
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/executil"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
)

func TestCloneBootstrappedDefaults(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := cloneBootstrappedDefaults(nil); got != nil {
			t.Fatalf("expected nil clone, got %#v", got)
		}
	})

	t.Run("deep_copy", func(t *testing.T) {
		in := &BootstrappedDefaults{
			DefaultShell: ShellNameSh,
			BaseEnv: map[string]string{
				envKeyPath: "/bin",
				envKeyHome: "/home/test",
			},
		}

		got := cloneBootstrappedDefaults(in)
		if got == nil {
			t.Fatalf("expected clone")
		}
		if got == in {
			t.Fatalf("expected independent copy")
		}
		if got.DefaultShell != in.DefaultShell {
			t.Fatalf("DefaultShell got %q want %q", got.DefaultShell, in.DefaultShell)
		}
		if !reflect.DeepEqual(got.BaseEnv, in.BaseEnv) {
			t.Fatalf("BaseEnv got %#v want %#v", got.BaseEnv, in.BaseEnv)
		}

		in.BaseEnv[envKeyPath] = "/tmp"
		if got.BaseEnv[envKeyPath] != "/bin" {
			t.Fatalf("clone should not track source mutations, got %s=%q", envKeyPath, got.BaseEnv[envKeyPath])
		}

		got.BaseEnv[envKeyHome] = "/other"
		if in.BaseEnv[envKeyHome] != "/home/test" {
			t.Fatalf("source should not track clone mutations, got %s=%q", envKeyHome, in.BaseEnv[envKeyHome])
		}
	})
}

func TestParseBootstrappedEnv(t *testing.T) {
	input := strings.Join([]string{
		"preamble",
		bootstrapEnvBeginMarker,
		envKeyPath + "=/bin",
		"EMPTY=",
		"FOO=bar=baz",
		"NOT_AN_ENV_LINE",
		bootstrapEnvEndMarker,
		"ignored=after",
	}, "\n")

	got, err := parseBootstrappedEnv(input)
	if err != nil {
		t.Fatalf("parseBootstrappedEnv: %v", err)
	}
	want := map[string]string{
		envKeyPath: "/bin",
		"EMPTY":    "",
		"FOO":      "bar=baz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestParseBootstrappedEnvErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{
			name: "missing_markers",
			in:   envKeyPath + "=/bin\n" + envKeyHome + "=/tmp",
		},
		{
			name: "end_before_begin",
			in: strings.Join([]string{
				bootstrapEnvEndMarker,
				bootstrapEnvBeginMarker,
				envKeyPath + "=/bin",
			}, "\n"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBootstrappedEnv(tc.in); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestParseAndFilterBootstrapEnv(t *testing.T) {
	var input string
	var want map[string]string

	if runtime.GOOS == "windows" {
		input = strings.Join([]string{
			bootstrapEnvBeginMarker,
			strings.ToLower(envKeyPath) + "=C:\\Tools",
			"pathext=.EXE;.CMD",
			"systemroot=C:\\Windows",
			"comspec=C:\\Windows\\System32\\cmd.exe",
			strings.ToLower(envKeyGitSSHCommand) + "=ssh -i key",
			"foo=bar",
			bootstrapEnvEndMarker,
		}, "\n")
		want = map[string]string{
			"path":            "C:\\Tools",
			"pathext":         ".EXE;.CMD",
			"systemroot":      "C:\\Windows",
			"comspec":         "C:\\Windows\\System32\\cmd.exe",
			"git_ssh_command": "ssh -i key",
		}
	} else {
		input = strings.Join([]string{
			bootstrapEnvBeginMarker,
			envKeyPath + "=/bin",
			envKeyHome + "=/home/test",
			envKeyLCAll + "=C",
			envKeyGitSSHCommand + "=ssh -i key",
			envPrefixASDF + "DIR=/opt/asdf",
			"foo=bar",
			bootstrapEnvEndMarker,
		}, "\n")
		want = map[string]string{
			envKeyPath:            "/bin",
			envKeyHome:            "/home/test",
			envKeyLCAll:           "C",
			envKeyGitSSHCommand:   "ssh -i key",
			envPrefixASDF + "DIR": "/opt/asdf",
		}
	}

	got, err := parseAndFilterBootstrapEnv(input)
	if err != nil {
		t.Fatalf("parseAndFilterBootstrapEnv: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}

	_, err = parseAndFilterBootstrapEnv(strings.Join([]string{
		bootstrapEnvBeginMarker,
		"FOO=bar",
		bootstrapEnvEndMarker,
	}, "\n"))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "usable") {
		t.Fatalf("expected no-usable-vars error, got %v", err)
	}
}

func TestFilterBootstrappedEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		raw := map[string]string{
			strings.ToLower(envKeyPath): "C:\\Tools",
			"pathext":                   ".EXE;.CMD",
			"systemroot":                "C:\\Windows",
			"comspec":                   "C:\\Windows\\System32\\cmd.exe",
			envPrefixASDF + "dir":       "C:\\asdf",
			"foo":                       "bar",
		}
		want := map[string]string{
			strings.ToLower(envKeyPath): "C:\\Tools",
			"pathext":                   ".EXE;.CMD",
			"systemroot":                "C:\\Windows",
			"comspec":                   "C:\\Windows\\System32\\cmd.exe",
			envPrefixASDF + "dir":       "C:\\asdf",
		}
		got := filterBootstrappedEnv(raw)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
		return
	}

	raw := map[string]string{
		envKeyPath:            "/bin",
		envKeyHome:            "/home/test",
		envKeyUser:            "tester",
		envKeyLogname:         "tester",
		envKeyShell:           "/bin/sh",
		envKeyLang:            "C.UTF-8",
		envKeyLCAll:           "C",
		envPrefixASDF + "DIR": "/opt/asdf",
		envKeyGitSSHCommand:   "ssh -i key",
		"foo":                 "bar",
	}
	want := map[string]string{
		envKeyPath:            "/bin",
		envKeyHome:            "/home/test",
		envKeyUser:            "tester",
		envKeyLogname:         "tester",
		envKeyShell:           "/bin/sh",
		envKeyLang:            "C.UTF-8",
		envKeyLCAll:           "C",
		envPrefixASDF + "DIR": "/opt/asdf",
		envKeyGitSSHCommand:   "ssh -i key",
	}
	got := filterBootstrappedEnv(raw)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestBootstrapCommandArgs(t *testing.T) {
	cases := []struct {
		name        string
		sel         executil.SelectedShell
		wantPrefix  []string
		wantContain []string
	}{
		{
			name:       "bash",
			sel:        executil.SelectedShell{Name: ShellNameBash, Path: "/bin/bash"},
			wantPrefix: []string{"/bin/bash", optionLIC},
			wantContain: []string{
				bootstrapEnvBeginMarker,
				bootstrapEnvEndMarker,
				"env",
			},
		},
		{
			name:        "zsh",
			sel:         executil.SelectedShell{Name: ShellNameZsh, Path: "/bin/zsh"},
			wantPrefix:  []string{"/bin/zsh", optionLIC},
			wantContain: []string{bootstrapEnvBeginMarker, bootstrapEnvEndMarker},
		},
		{
			name:        "fish",
			sel:         executil.SelectedShell{Name: ShellNameFish, Path: "/usr/bin/fish"},
			wantPrefix:  []string{"/usr/bin/fish", "-l", "-i", "-c"},
			wantContain: []string{bootstrapEnvBeginMarker, bootstrapEnvEndMarker},
		},
		{
			name:        "sh",
			sel:         executil.SelectedShell{Name: ShellNameSh, Path: "/bin/sh"},
			wantPrefix:  []string{"/bin/sh", "-c"},
			wantContain: []string{bootstrapEnvBeginMarker, bootstrapEnvEndMarker, "env"},
		},
		{
			name:        "dash",
			sel:         executil.SelectedShell{Name: ShellNameDash, Path: "/bin/dash"},
			wantPrefix:  []string{"/bin/dash", "-c"},
			wantContain: []string{bootstrapEnvBeginMarker, bootstrapEnvEndMarker},
		},
		{
			name:        "ksh",
			sel:         executil.SelectedShell{Name: ShellNameKsh, Path: "/bin/ksh"},
			wantPrefix:  []string{"/bin/ksh", "-lc"},
			wantContain: []string{bootstrapEnvBeginMarker, bootstrapEnvEndMarker},
		},
		{
			name:        "pwsh",
			sel:         executil.SelectedShell{Name: ShellNamePwsh, Path: "/usr/bin/pwsh"},
			wantPrefix:  []string{"/usr/bin/pwsh", optionNoLogo, optionNonInteractive, optionCommand},
			wantContain: []string{bootstrapEnvBeginMarker, bootstrapEnvEndMarker, "GetEnvironmentVariables"},
		},
		{
			name: "powershell",
			sel: executil.SelectedShell{
				Name: ShellNamePowershell,
				Path: "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
			},
			wantPrefix: []string{
				"C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
				optionNoLogo,
				optionNonInteractive,
				optionCommand,
			},
			wantContain: []string{bootstrapEnvBeginMarker, bootstrapEnvEndMarker, "GetEnvironmentVariables"},
		},
		{
			name:        "cmd",
			sel:         executil.SelectedShell{Name: ShellNameCmd, Path: "C:\\Windows\\System32\\cmd.exe"},
			wantPrefix:  []string{"C:\\Windows\\System32\\cmd.exe", "/d", "/s", "/v:off", "/c"},
			wantContain: []string{bootstrapEnvBeginMarker, bootstrapEnvEndMarker, "set"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bootstrapCommandArgs(tc.sel)
			if err != nil {
				t.Fatalf("bootstrapCommandArgs: %v", err)
			}
			if len(got) != len(tc.wantPrefix)+1 {
				t.Fatalf("got %d args want %d (%#v)", len(got), len(tc.wantPrefix)+1, got)
			}
			for i, want := range tc.wantPrefix {
				if got[i] != want {
					t.Fatalf("arg[%d] got %q want %q", i, got[i], want)
				}
			}
			cmd := got[len(got)-1]
			for _, s := range tc.wantContain {
				if !strings.Contains(cmd, s) {
					t.Fatalf("command %q does not contain %q", cmd, s)
				}
			}
		})
	}

	if _, err := bootstrapCommandArgs(
		executil.SelectedShell{Name: ShellName("nope"), Path: "/bin/nope"},
	); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "unsupported shell") {
		t.Fatalf("expected unsupported shell error, got %v", err)
	}
}

func TestDefaultRunScriptPolicy(t *testing.T) {
	got := DefaultRunScriptPolicy()

	if got.MaxArgs != defaultRunScriptMaxArgs {
		t.Fatalf("MaxArgs got %d want %d", got.MaxArgs, defaultRunScriptMaxArgs)
	}
	if got.MaxArgBytes != defaultRunScriptMaxArgBytes {
		t.Fatalf("MaxArgBytes got %d want %d", got.MaxArgBytes, defaultRunScriptMaxArgBytes)
	}
	if got.ExecutionPolicy != (ExecutionPolicy{}) {
		t.Fatalf("ExecutionPolicy should be zero value, got %#v", got.ExecutionPolicy)
	}
	if !slices.Contains(got.AllowedExtensions, string(ioutil.ExtShell)) ||
		!slices.Contains(got.AllowedExtensions, string(ioutil.ExtPS1)) ||
		!slices.Contains(got.AllowedExtensions, string(ioutil.ExtPY)) {
		t.Fatalf("missing expected default allowed extensions: %#v", got.AllowedExtensions)
	}
	if runtime.GOOS == "windows" {
		if !slices.Contains(got.AllowedExtensions, string(ioutil.ExtBAT)) ||
			!slices.Contains(got.AllowedExtensions, string(ioutil.ExtCMD)) {
			t.Fatalf("windows defaults should include bat/cmd: %#v", got.AllowedExtensions)
		}
		py := got.InterpreterByExtension[string(ioutil.ExtPY)]
		if py.Command != defaultPythonCommand || py.Shell != ShellNamePowershell {
			t.Fatalf("windows python interpreter got %+v", py)
		}
		if got.InterpreterByExtension[string(ioutil.ExtBAT)].Shell != ShellNameCmd ||
			got.InterpreterByExtension[string(ioutil.ExtCMD)].Shell != ShellNameCmd {
			t.Fatalf("windows bat/cmd should use cmd: %#v", got.InterpreterByExtension)
		}
	} else {
		if slices.Contains(got.AllowedExtensions, string(ioutil.ExtBAT)) ||
			slices.Contains(got.AllowedExtensions, string(ioutil.ExtCMD)) {
			t.Fatalf("unix defaults should not include bat/cmd: %#v", got.AllowedExtensions)
		}
		py := got.InterpreterByExtension[string(ioutil.ExtPY)]
		if py.Command != defaultPython3Command || py.Shell != ShellNameSh {
			t.Fatalf("unix python interpreter got %+v", py)
		}
	}

	if sh := got.InterpreterByExtension[string(ioutil.ExtShell)]; sh.Mode != RunScriptModeShell ||
		sh.Shell != ShellNameSh {
		t.Fatalf("shell interpreter got %+v", sh)
	}
	if ps := got.InterpreterByExtension[string(ioutil.ExtPS1)]; ps.Mode != RunScriptModeDirect {
		t.Fatalf("ps1 interpreter should be direct, got %+v", ps)
	} else if runtime.GOOS == "windows" {
		if ps.Shell != ShellNamePowershell {
			t.Fatalf("windows ps1 should use powershell, got %+v", ps)
		}
	} else if ps.Shell != ShellNamePwsh {
		t.Fatalf("unix ps1 should use pwsh, got %+v", ps)
	}
}

func TestNormalizeRunScriptPolicy(t *testing.T) {
	t.Run("normalization_and_clone", func(t *testing.T) {
		in := RunScriptPolicy{
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
					Command: " " + defaultPython3Command + " ",
					Args:    []string{"-u"},
				},
			},
		}

		got, err := NormalizeRunScriptPolicy(in)
		if err != nil {
			t.Fatalf("NormalizeRunScriptPolicy: %v", err)
		}

		wantExts := []string{".sh", ".py", ".bash"}
		if !reflect.DeepEqual(got.AllowedExtensions, wantExts) {
			t.Fatalf("AllowedExtensions got %#v want %#v", got.AllowedExtensions, wantExts)
		}
		if got.MaxArgs != defaultRunScriptMaxArgs {
			t.Fatalf("MaxArgs got %d want %d", got.MaxArgs, defaultRunScriptMaxArgs)
		}
		if got.MaxArgBytes != defaultRunScriptMaxArgBytes {
			t.Fatalf("MaxArgBytes got %d want %d", got.MaxArgBytes, defaultRunScriptMaxArgBytes)
		}

		sh := got.InterpreterByExtension[".sh"]
		if sh.Shell != ShellNameSh || sh.Mode != RunScriptModeShell || !reflect.DeepEqual(sh.Args, []string{"-x"}) {
			t.Fatalf("normalized .sh interpreter got %+v", sh)
		}
		py := got.InterpreterByExtension[".py"]
		if py.Shell != ShellNameAuto || py.Mode != RunScriptModeInterpreter || py.Command != defaultPython3Command ||
			!reflect.DeepEqual(py.Args, []string{"-u"}) {
			t.Fatalf("normalized .py interpreter got %+v", py)
		}

		in.AllowedExtensions[0] = "changed"
		tmp := in.InterpreterByExtension[" SH "]
		tmp.Args[0] = "-z"
		in.InterpreterByExtension[" SH "] = tmp

		if got.AllowedExtensions[0] != ".sh" {
			t.Fatalf("normalized slice should be independent, got %#v", got.AllowedExtensions)
		}
		if got.InterpreterByExtension[".sh"].Args[0] != "-x" {
			t.Fatalf(
				"normalized interpreter args should be independent, got %#v",
				got.InterpreterByExtension[".sh"].Args,
			)
		}
	})

	tests := []struct {
		name    string
		in      RunScriptPolicy
		wantErr string
	}{
		{
			name:    "negative_max_args",
			in:      RunScriptPolicy{MaxArgs: -1},
			wantErr: "MaxArgs",
		},
		{
			name:    "negative_max_arg_bytes",
			in:      RunScriptPolicy{MaxArgBytes: -1},
			wantErr: "MaxArgBytes",
		},
		{
			name:    "allowed_extensions_with_nul",
			in:      RunScriptPolicy{AllowedExtensions: []string{".sh\x00"}},
			wantErr: "AllowedExtensions",
		},
		{
			name: "interpreter_key_with_nul",
			in: RunScriptPolicy{
				InterpreterByExtension: map[string]RunScriptInterpreter{".sh\x00": {Mode: RunScriptModeShell}},
			},
			wantErr: "NUL",
		},
		{
			name: "invalid_shell",
			in: RunScriptPolicy{
				InterpreterByExtension: map[string]RunScriptInterpreter{
					".sh": {Shell: ShellName("nope"), Mode: RunScriptModeShell},
				},
			},
			wantErr: "invalid shell",
		},
		{
			name: "invalid_mode",
			in: RunScriptPolicy{
				InterpreterByExtension: map[string]RunScriptInterpreter{
					".sh": {Shell: ShellNameSh, Mode: RunScriptMode("nope")},
				},
			},
			wantErr: "invalid mode",
		},
		{
			name: "interpreter_mode_requires_command",
			in: RunScriptPolicy{
				InterpreterByExtension: map[string]RunScriptInterpreter{
					".py": {Shell: ShellNameSh, Mode: RunScriptModeInterpreter, Command: "   "},
				},
			},
			wantErr: "empty Command",
		},
		{
			name: "command_with_nul",
			in: RunScriptPolicy{
				InterpreterByExtension: map[string]RunScriptInterpreter{
					".py": {Shell: ShellNameSh, Mode: RunScriptModeInterpreter, Command: defaultPythonCommand + "\x00"},
				},
			},
			wantErr: "contains NUL",
		},
		{
			name: "arg_with_nul",
			in: RunScriptPolicy{
				InterpreterByExtension: map[string]RunScriptInterpreter{
					".py": {
						Shell:   ShellNameSh,
						Mode:    RunScriptModeInterpreter,
						Command: defaultPythonCommand,
						Args:    []string{"ok", "bad\x00"},
					},
				},
			},
			wantErr: "args[1]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeRunScriptPolicy(tc.in)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErr)) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestRunScriptPolicyClone(t *testing.T) {
	in := &RunScriptPolicy{
		AllowedExtensions: []string{".sh", ".py"},
		InterpreterByExtension: map[string]RunScriptInterpreter{
			".sh": {
				Shell: ShellNameSh,
				Mode:  RunScriptModeShell,
				Args:  []string{"-x"},
			},
		},
		ExecutionPolicy: ExecutionPolicy{Timeout: 2},
		MaxArgs:         3,
		MaxArgBytes:     4,
	}

	got := in.Clone()
	if got == nil {
		t.Fatalf("expected clone")
	}
	if got == in {
		t.Fatalf("expected independent copy")
	}
	if !reflect.DeepEqual(got.AllowedExtensions, in.AllowedExtensions) {
		t.Fatalf("AllowedExtensions got %#v want %#v", got.AllowedExtensions, in.AllowedExtensions)
	}
	if !reflect.DeepEqual(got.InterpreterByExtension, in.InterpreterByExtension) {
		t.Fatalf("InterpreterByExtension got %#v want %#v", got.InterpreterByExtension, in.InterpreterByExtension)
	}
	if got.ExecutionPolicy != in.ExecutionPolicy || got.MaxArgs != in.MaxArgs || got.MaxArgBytes != in.MaxArgBytes {
		t.Fatalf("clone fields differ: got %#v want %#v", got, in)
	}

	in.AllowedExtensions[0] = ".txt"
	interp := in.InterpreterByExtension[".sh"]
	interp.Args[0] = "-y"
	in.InterpreterByExtension[".sh"] = interp
	in.ExecutionPolicy.Timeout = 3

	if got.AllowedExtensions[0] != ".sh" {
		t.Fatalf("clone should not track AllowedExtensions mutations: %#v", got.AllowedExtensions)
	}
	if got.InterpreterByExtension[".sh"].Args[0] != "-x" {
		t.Fatalf("clone should not track interpreter args mutations: %#v", got.InterpreterByExtension[".sh"].Args)
	}
	if got.ExecutionPolicy.Timeout != 2 {
		t.Fatalf("clone should not track execution policy mutations: %#v", got.ExecutionPolicy)
	}
}

func TestHasAnyPrefix(t *testing.T) {
	if !hasAnyPrefix(envKeyLCAll, []string{envPrefixLC, envPrefixASDF}) {
		t.Fatalf("expected prefix match")
	}
	if hasAnyPrefix(envKeyHome, []string{envPrefixLC, envPrefixASDF}) {
		t.Fatalf("did not expect prefix match")
	}
}
