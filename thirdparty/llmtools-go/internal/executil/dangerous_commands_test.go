package executil

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

const (
	dangerousBlockedCommandSubstr = "blocked command"
	dangerousSudoSubstr           = "sudo"
	dangerousForkBombSubstr       = "fork bomb"
	dangerousBackgroundSubstr     = "background"
	dangerousRmSubstr             = "rm"
	dangerousMkfsSubstr           = "mkfs"
	dangerousFormatSubstr         = "format"
	dangerousDiskpartSubstr       = "diskpart"
	dangerousShutdownSubstr       = "shutdown"

	dangerousForkBombClassicCmd  = ":(){ :|:& };:"
	dangerousBackgroundSleepCmd  = "sleep 1 &"
	dangerousRmRfRootCmd         = "rm -rf /"
	dangerousMkfsExt4Cmd         = "mkfs.ext4 /dev/sda1"
	dangerousShellPathUnix       = "/bin/sh"
	dangerousShellPathWindowsCmd = `C:\Windows\System32\cmd.exe`
	dangerousDiskpartFullPath    = `C:\Windows\System32\diskpart.exe`
	dangerousPowerShellShellName = "powershell"

	dangerousShutdownCmd = "shutdown"
	dangerousRebootCmd   = "reboot"
	dangerousHaltCmd     = "halt"
	dangerousPoweroffCmd = "poweroff"
	dangerousTopCmd      = "top"
	dangerousDiskpartCmd = "diskpart"
	dangerousFormatCmd   = "format"
)

type rejectTC struct {
	name       string
	cmd        string
	shellName  string
	shellPath  string
	wantErr    bool
	wantSubstr string // optional substring to assert in error message
}

func TestRejectDangerous_EmptyAndWhitespace(t *testing.T) {
	runRejectCases(t, []rejectTC{
		{name: "empty", cmd: "", wantErr: false},
		{name: "spaces", cmd: "   ", wantErr: false},
		{name: "tabs_newlines", cmd: "\n\t  \n", wantErr: false},
	})
}

func TestRejectDangerous_Unix_SafeInputs(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-focused expectations")
	}

	runRejectCases(t, []rejectTC{
		// Basic "boundary" false positives we want to avoid.
		{name: "echo_sudo_argument", cmd: "echo sudo", wantErr: false},
		{name: "printf_sudo_argument", cmd: "printf %s sudo", wantErr: false},
		{name: "quoted_sudo", cmd: `echo "sudo"`, wantErr: false},
		{name: "single_quoted_sudo", cmd: `echo 'sudo'`, wantErr: false},

		// Separators inside quotes should not split commands.
		{name: "sudo_inside_quotes_semicolon", cmd: `echo "x; sudo ls"`, wantErr: false},
		{name: "sudo_inside_quotes_andand", cmd: `echo "x && sudo ls"`, wantErr: false},
		{name: "sudo_inside_quotes_pipe", cmd: `echo "x | sudo ls"`, wantErr: false},
		{name: "hash_inside_quotes_not_comment", cmd: `echo "# sudo ls"`, wantErr: false},

		// Comments: sudo is in comment => should not be rejected.
		{name: "sudo_in_comment", cmd: "echo hi # sudo ls", wantErr: false},
		{name: "hash_not_comment_when_not_preceded_by_space", cmd: "echo hi#sudo", wantErr: false},

		// "&" handling: should not reject redirections or escaped ampersand.
		{name: "allow_andand", cmd: "echo a && echo b", wantErr: false},
		{name: "escaped_ampersand", cmd: `echo hi \&`, wantErr: false},
		{name: "amp_redir_stdout", cmd: `echo hi &>out.txt`, wantErr: false},
		{name: "amp_redir_stderr_to_stdout", cmd: `echo hi 2>&1`, wantErr: false},
		{name: "pipe_stderr_too", cmd: `echo hi |& tee out.txt`, wantErr: false},

		// Blocklist should not false-positive on arguments.
		{name: "echo_rm_argument", cmd: "echo rm", wantErr: false},
		{name: "echo_curl_argument", cmd: "echo curl", wantErr: false},

		// "mkfs / shutdown / editors" as arguments (not commands) should be allowed.
		{name: "echo_mkfs_argument", cmd: "echo mkfs /dev/sda", wantErr: false},
		{name: "echo_shutdown_argument", cmd: "echo shutdown -h now", wantErr: false},
		{name: "echo_vim_argument", cmd: "echo vim foo.txt", wantErr: false},
	})
}

func TestRejectDangerous_Unix_RejectedInputs(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-focused expectations")
	}

	runRejectCases(t, []rejectTC{
		// Sudo/su.
		{name: "sudo_simple", cmd: "sudo ls -h", wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{name: "su_simple", cmd: "su -c id", wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{name: "sudo_after_semicolon", cmd: "echo x; sudo ls", wantErr: true, wantSubstr: dangerousSudoSubstr},
		{name: "sudo_after_andand", cmd: "echo x && sudo ls", wantErr: true, wantSubstr: dangerousSudoSubstr},
		{name: "sudo_after_oror", cmd: "echo x || sudo ls", wantErr: true, wantSubstr: dangerousSudoSubstr},
		{name: "sudo_after_pipe", cmd: "echo x | sudo ls", wantErr: true, wantSubstr: dangerousSudoSubstr},
		{name: "sudo_in_parens", cmd: "(sudo ls)", wantErr: true, wantSubstr: dangerousSudoSubstr},
		{name: "sudo_with_path", cmd: "/usr/bin/sudo ls", wantErr: true, wantSubstr: dangerousSudoSubstr},

		// Fork bomb (whitespace variations).
		{
			name:       "fork_bomb_classic",
			cmd:        dangerousForkBombClassicCmd,
			wantErr:    true,
			wantSubstr: dangerousForkBombSubstr,
		},
		{
			name:       "fork_bomb_extra_spaces",
			cmd:        ": ( ) {  : | : & } ; :",
			wantErr:    true,
			wantSubstr: dangerousForkBombSubstr,
		},
		{
			name:       "fork_bomb_with_newlines",
			cmd:        ":\n(){\n:|:&\n};:\n",
			wantErr:    true,
			wantSubstr: dangerousForkBombSubstr,
		},

		// "rm" is ALWAYS blocked  (any target).
		{name: "rm_simple", cmd: "rm foo", wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{name: "rm_rf_root", cmd: dangerousRmRfRootCmd, wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{name: "rm_fr_root_glob", cmd: "rm -fr /*", wantErr: true, wantSubstr: dangerousRmSubstr},
		{name: "rm_r_root_no_force", cmd: "rm -r /", wantErr: true, wantSubstr: dangerousRmSubstr},
		{name: "rm_R_root", cmd: "rm -R /", wantErr: true, wantSubstr: dangerousRmSubstr},
		{name: "rm_long_recursive_root", cmd: "rm --recursive /", wantErr: true, wantSubstr: dangerousRmSubstr},
		{name: "rm_recursive_with_double_dash", cmd: "rm -r -- /", wantErr: true, wantSubstr: dangerousRmSubstr},
		{name: "rm_recursive_root_quoted", cmd: `rm -r "/"`, wantErr: true, wantSubstr: dangerousRmSubstr},
		{name: "rm_recursive_root_glob_quoted", cmd: `rm -r '/*'`, wantErr: true, wantSubstr: dangerousRmSubstr},
		{name: "rm_recursive_root_via_clean_dot", cmd: `rm -r /./`, wantErr: true, wantSubstr: dangerousRmSubstr},
		{
			name:       "rm_recursive_root_via_clean_double_slash",
			cmd:        `rm -r //`,
			wantErr:    true,
			wantSubstr: dangerousRmSubstr,
		},
		{
			name:       "rm_recursive_root_via_clean_parent",
			cmd:        `rm -r /tmp/..`,
			wantErr:    true,
			wantSubstr: dangerousRmSubstr,
		},
		{name: "rm_recursive_root_via_clean_parent2", cmd: `rm -R /../`, wantErr: true, wantSubstr: dangerousRmSubstr},

		// Mkfs.
		{name: "mkfs_plain", cmd: "mkfs /dev/sda", wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{
			name:       "mkfs_variant_ext4",
			cmd:        dangerousMkfsExt4Cmd,
			wantErr:    true,
			wantSubstr: dangerousBlockedCommandSubstr,
		},

		// Network tools.
		{
			name:       "curl_blocked",
			cmd:        "curl https://example.com",
			wantErr:    true,
			wantSubstr: dangerousBlockedCommandSubstr,
		},

		// Shutdown/reboot/halt/poweroff.
		{
			name:       dangerousShutdownCmd,
			cmd:        dangerousShutdownCmd + " -h now",
			wantErr:    true,
			wantSubstr: dangerousBlockedCommandSubstr,
		},
		{name: dangerousRebootCmd, cmd: dangerousRebootCmd, wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{name: dangerousHaltCmd, cmd: dangerousHaltCmd, wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{
			name:       dangerousPoweroffCmd,
			cmd:        dangerousPoweroffCmd,
			wantErr:    true,
			wantSubstr: dangerousBlockedCommandSubstr,
		},

		// Interactive tools.
		{name: "vim", cmd: "vim foo.txt", wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{name: "less", cmd: "less /var/log/syslog", wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{name: dangerousTopCmd, cmd: dangerousTopCmd, wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},
		{name: "pipeline_into_less", cmd: "echo hi | less", wantErr: true, wantSubstr: dangerousBlockedCommandSubstr},

		// Backgrounding/chaining with "&".
		{
			name:       "background_trailing",
			cmd:        dangerousBackgroundSleepCmd,
			wantErr:    true,
			wantSubstr: dangerousBackgroundSubstr,
		},
		{name: "background_no_space", cmd: "sleep 1&", wantErr: true, wantSubstr: dangerousBackgroundSubstr},
		{
			name:       "chaining_single_ampersand",
			cmd:        "echo a & echo b",
			wantErr:    true,
			wantSubstr: dangerousBackgroundSubstr,
		},
	})
}

func TestRejectDangerous_Unix_WrappersAndAssignments(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-focused expectations")
	}

	runRejectCases(t, []rejectTC{
		// Env assignments should be skipped to find the real command.
		{name: "assignment_then_sudo", cmd: "X=1 sudo ls", wantErr: true, wantSubstr: dangerousSudoSubstr},
		{name: "multiple_assignments_then_su", cmd: "A=1 B=2 su -c id", wantErr: true, wantSubstr: "su"},
		{name: "assignment_then_echo_sudo_is_safe", cmd: "X=1 echo sudo", wantErr: false},

		// Unwrap env/command/builtin wrappers.
		{name: "env_wraps_sudo", cmd: "env sudo ls", wantErr: true, wantSubstr: dangerousSudoSubstr},
		{
			name:       "env_with_flags_wraps_sudo",
			cmd:        "env -i FOO=bar sudo ls",
			wantErr:    true,
			wantSubstr: dangerousSudoSubstr,
		},
		{name: "command_wraps_sudo", cmd: "command sudo ls", wantErr: true, wantSubstr: dangerousSudoSubstr},
		{name: "builtin_wraps_sudo", cmd: "builtin sudo ls", wantErr: true, wantSubstr: dangerousSudoSubstr},
	})
}

func TestRejectDangerous_Windows_Patterns(t *testing.T) {
	if runtime.GOOS != toolutil.GOOSWindows {
		t.Skip("windows-only patterns")
	}

	runRejectCases(t, []rejectTC{
		// Diskpart anywhere on Windows.
		{
			name:       dangerousDiskpartCmd,
			cmd:        dangerousDiskpartCmd,
			wantErr:    true,
			wantSubstr: dangerousBlockedCommandSubstr,
		},
		{
			name:       dangerousDiskpartCmd + "_exe",
			cmd:        dangerousDiskpartCmd + ".exe",
			wantErr:    true,
			wantSubstr: dangerousBlockedCommandSubstr,
		},
		{
			name:       dangerousDiskpartCmd + "_full_path",
			cmd:        dangerousDiskpartFullPath,
			wantErr:    true,
			wantSubstr: dangerousDiskpartSubstr,
		},
		{
			name:       dangerousDiskpartCmd + "_after_andand",
			cmd:        "echo hi && " + dangerousDiskpartCmd,
			wantErr:    true,
			wantSubstr: dangerousDiskpartSubstr,
		},
		{name: "echo_diskpart_argument_safe", cmd: "echo diskpart", wantErr: false},

		// Format.com anywhere.
		{
			name:       dangerousFormatCmd + "_com",
			cmd:        dangerousFormatCmd + ".com C:",
			wantErr:    true,
			wantSubstr: dangerousBlockedCommandSubstr,
		},

		// Format only when using cmd.exe shellName.
		{
			name:       dangerousFormatCmd + "_cmd_shell_reject",
			cmd:        dangerousFormatCmd + " C:",
			shellName:  string(ShellNameCmd),
			wantErr:    true,
			wantSubstr: dangerousBlockedCommandSubstr,
		},
		{
			name:       dangerousFormatCmd + "_exe_cmd_shell_reject",
			cmd:        dangerousFormatCmd + ".exe C:",
			shellName:  string(ShellNameCmd),
			wantErr:    true,
			wantSubstr: dangerousFormatSubstr,
		},
		{
			name:      "format_powershell_allow",
			cmd:       dangerousFormatCmd + " C:",
			shellName: dangerousPowerShellShellName,
			wantErr:   false,
		},

		// Ensure we don't accidentally block "Format-Table" style cmdlets by prefix.
		{name: "format_table_allowed", cmd: "Format-Table", shellName: dangerousPowerShellShellName, wantErr: false},

		// Cross-platform blocks still apply on Windows too.
		{
			name:       dangerousShutdownCmd + "_blocked_on_windows",
			cmd:        dangerousShutdownCmd + " /s /t 0",
			wantErr:    true,
			wantSubstr: dangerousShutdownSubstr,
		},
		{name: "sudo_blocked_on_windows_too", cmd: "sudo whoami", wantErr: true, wantSubstr: dangerousSudoSubstr},
		{
			name:       "mkfs_blocked_on_windows_too",
			cmd:        dangerousMkfsExt4Cmd,
			wantErr:    true,
			wantSubstr: dangerousMkfsSubstr,
		},
	})
}

func TestForEachSegment(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		dialect shellDialect
		want    []string
	}{
		{
			name:    "sh_splits_oror_and_pipe",
			in:      "echo x || sudo ls -a | cat",
			dialect: dialectSh,
			want:    []string{"echo x", "sudo ls -a", "cat"},
		},
		{
			name:    "sh_does_not_split_inside_quotes",
			in:      `echo "a|b"; sudo ls`,
			dialect: dialectSh,
			want:    []string{`echo "a|b"`, "sudo ls"},
		},
		{
			name:    "sh_hash_comment_only_when_word_startish",
			in:      "echo hi # sudo ls; rm -rf /",
			dialect: dialectSh,
			want:    []string{"echo hi"},
		},
		{
			name:    "powershell_hash_comment_anywhere",
			in:      "echo hiT# rm; sudo",
			dialect: dialectPowerShell,
			want:    []string{"echo hiT"},
		},
		{
			name:    "cmd_splits_ampersand_and_andand",
			in:      "echo hi & echo bye && echo ok",
			dialect: dialectCmd,
			want:    []string{"echo hi", "echo bye", "echo ok"},
		},
		{
			name:    "cmd_caret_escapes_metacharacters_best_effort",
			in:      "echo hi ^& echo bye",
			dialect: dialectCmd,
			want:    []string{"echo hi ^& echo bye"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var segs []string
			err := forEachSegment(tc.in, tc.dialect, func(seg string) error {
				segs = append(segs, seg)
				return nil
			})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !reflect.DeepEqual(segs, tc.want) {
				t.Fatalf("segments mismatch:\n got: %#v\nwant: %#v", segs, tc.want)
			}
		})
	}
}

func TestRejectDangerous_PowerShell_CallOperatorAndComments(t *testing.T) {
	cases := []struct {
		name       string
		cmd        string
		wantErr    bool
		wantSubstr string
	}{
		{
			name:       "call_operator_blocks_underlying_command",
			cmd:        `& rm -rf /`,
			wantErr:    true,
			wantSubstr: "rm",
		},
		{
			name:       "call_operator_blocks_quoted_underlying_command",
			cmd:        `& 'rm' -rf /`,
			wantErr:    true,
			wantSubstr: "rm",
		},
		{
			name:    "comment_anywhere_prevents_scanning_rest",
			cmd:     `echo hi# rm -rf /`,
			wantErr: false,
		},
		{
			name:    "quoted_hash_is_not_a_comment",
			cmd:     `echo "# rm -rf /"`,
			wantErr: false,
		},
		{
			name:       "semicolon_splits_segments",
			cmd:        `echo hi; rm -rf /`,
			wantErr:    true,
			wantSubstr: "rm",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectDangerousCommand(tc.cmd, "pwsh", ShellNamePwsh, HardBlockedCommands, true)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("did not expect error: %v", err)
			}
			if tc.wantErr && tc.wantSubstr != "" &&
				!strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantSubstr)) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestRejectDangerous_HeuristicChecksToggle(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		shell   ShellName
		enable  bool
		wantErr bool
		substr  string
	}{
		{
			name:    "fork_bomb_blocked_when_enabled",
			cmd:     dangerousForkBombClassicCmd,
			shell:   ShellNameSh,
			enable:  true,
			wantErr: true,
			substr:  dangerousForkBombSubstr,
		},
		{
			name:    "fork_bomb_allowed_when_disabled",
			cmd:     dangerousForkBombClassicCmd,
			shell:   ShellNameSh,
			enable:  false,
			wantErr: false,
		},
		{
			name:    "background_ampersand_blocked_when_enabled",
			cmd:     dangerousBackgroundSleepCmd,
			shell:   ShellNameSh,
			enable:  true,
			wantErr: true,
			substr:  dangerousBackgroundSubstr,
		},
		{
			name:    "background_ampersand_allowed_when_disabled",
			cmd:     dangerousBackgroundSleepCmd,
			shell:   ShellNameSh,
			enable:  false,
			wantErr: false,
		},
		{
			name:    "hard_block_still_blocks_even_when_heuristics_disabled",
			cmd:     dangerousRmRfRootCmd,
			shell:   ShellNameSh,
			enable:  false,
			wantErr: true,
			substr:  dangerousBlockedCommandSubstr,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectDangerousCommand(tc.cmd, dangerousShellPathUnix, tc.shell, HardBlockedCommands, tc.enable)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("did not expect error: %v", err)
			}
			if tc.wantErr && tc.substr != "" &&
				!strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.substr)) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.substr)
			}
		})
	}
}

func runRejectCases(t *testing.T, cases []rejectTC) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Helper()
			if tc.shellName == "" {
				if runtime.GOOS == toolutil.GOOSWindows {
					tc.shellName = string(ShellNameCmd)
				} else {
					tc.shellName = string(ShellNameSh)
				}
			}
			if tc.shellPath == "" {
				if runtime.GOOS == toolutil.GOOSWindows {
					tc.shellPath = dangerousShellPathWindowsCmd
				} else {
					tc.shellPath = dangerousShellPathUnix
				}
			}

			err := RejectDangerousCommand(tc.cmd, tc.shellPath, ShellName(tc.shellName), HardBlockedCommands, true)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected reject for %q", tc.cmd)
				}
				if tc.wantSubstr != "" &&
					!strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantSubstr)) {
					t.Fatalf("error %q does not contain %q (cmd=%q)", err.Error(), tc.wantSubstr, tc.cmd)
				}
			} else if err != nil {
				t.Fatalf("did not expect reject for %q: %v", tc.cmd, err)
			}
		})
	}
}
