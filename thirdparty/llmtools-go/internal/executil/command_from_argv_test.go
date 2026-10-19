package executil

import (
	"strings"
	"testing"
)

const (
	argvEcho        = "echo"
	argvHi          = "hi"
	argvHelloWorld  = "hello world"
	argvFooBar      = "foo'bar"
	argvNUL         = "a\x00b"
	argvProgramPath = `C:\Program Files\app.exe`
	argvXY          = "x y"
	argvAB          = "a'b"
)

func TestCommandFromArgv_ValidationAndDialects(t *testing.T) {
	cases := []struct {
		name          string
		shell         ShellName
		argv          []string
		want          string
		wantErrSubstr string
	}{
		{
			name:          "invalid_empty_argv",
			shell:         ShellNameSh,
			argv:          nil,
			wantErrSubstr: "invalid args",
		},
		{
			name:          "invalid_shell_auto",
			shell:         ShellNameAuto,
			argv:          []string{argvEcho, argvHi},
			wantErrSubstr: "invalid args",
		},
		{
			name:  "cmd_quotes_external_argv",
			shell: ShellNameCmd,
			argv:  []string{argvProgramPath, argvXY, "100%"},
			want:  "\"C:\\Program Files\\app.exe\" \"x y\" \"100%%\"",
		},
		{name: "cmd_rejects_newline", shell: ShellNameCmd, argv: []string{argvEcho, "a\nb"}, wantErrSubstr: "newline"},

		{
			name:  "sh_quotes_all_args_posix_single_quote",
			shell: ShellNameSh,
			argv:  []string{argvEcho, argvHelloWorld},
			want:  "'echo' 'hello world'",
		},
		{
			name:  "sh_quotes_empty_arg",
			shell: ShellNameSh,
			argv:  []string{argvEcho, ""},
			want:  "'echo' ''",
		},
		{
			name:  "sh_escapes_single_quotes_posix_strategy",
			shell: ShellNameSh,
			argv:  []string{argvEcho, argvFooBar},
			want:  "'echo' 'foo'\"'\"'bar'",
		},
		{
			name:          "sh_rejects_nul_in_arg",
			shell:         ShellNameSh,
			argv:          []string{argvEcho, argvNUL},
			wantErrSubstr: "nul",
		},
		{
			name:  "powershell_prefixes_call_operator_and_quotes",
			shell: ShellNamePwsh,
			argv:  []string{argvProgramPath, argvXY},
			want:  "& 'C:\\Program Files\\app.exe' 'x y'",
		},
		{
			name:  "powershell_escapes_single_quote_by_doubling",
			shell: ShellNamePwsh,
			argv:  []string{argvEcho, argvAB},
			want:  "& 'echo' 'a''b'",
		},
		{
			name:          "powershell_rejects_nul_in_arg",
			shell:         ShellNamePowershell,
			argv:          []string{argvEcho, argvNUL},
			wantErrSubstr: "nul",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CommandFromArgv(tc.shell, tc.argv)
			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErrSubstr)) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
