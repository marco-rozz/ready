//nolint:goconst // literal fixtures are clearer inline than behind constants
package initwizard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
)

const (
	testWindowsOS   = "windows"
	testProfileFlag = "--profile"
	testWizardAdmin = "admin"
)

func TestSavedSetupCommandsUsePATHAndRelativePaths(t *testing.T) {
	if runtime.GOOS == testWindowsOS {
		t.Skip("POSIX shell round-trip; Windows command rendering has a separate test")
	}
	root := t.TempDir()
	program := filepath.Join(root, "ready")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil { //nolint:gosec // Executable fixture confined to the test's private directory.
		t.Fatal(err)
	}
	var output strings.Builder
	var err error
	o := baseOptions(freshInterviewAnswer+"n\n", true, &output)
	o.WorkingDir, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	o.ExplicitAgentDir = filepath.Join(root, "agent's data")
	o.ReadyGuideCheck = func() bool { return false }
	outcome, err := Run(t.Context(), o)
	if err != nil || !outcome.Committed {
		t.Fatalf("setup: committed=%v err=%v", outcome.Committed, err)
	}
	if strings.Index(output.String(), "Next steps") < strings.Index(output.String(), "Optional checks") {
		t.Fatal("launch commands must be printed after optional checks")
	}
	commands := printedCommands(output.String(), "ready")
	want := [][]string{
		{"serve", "--agent-dir", "./agent's data", testProfileFlag, testWizardAdmin},
		{"chat", "--agent-dir", "./agent's data", "--address", defaultZMQBind},
		{"chat", "--agent-dir", "./agent's data"},
		{"serve", "--agent-dir", "./agent's data"},
	}
	if len(commands) != len(want) {
		t.Fatalf("got %d launch commands, want %d", len(commands), len(want))
	}
	for i, command := range commands {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"))
		data, err := cmd.Output()
		if err != nil {
			t.Fatalf("copyable command %d failed from the stated working directory: %v", i, err)
		}
		args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if !reflect.DeepEqual(args, want[i]) {
			t.Fatalf("command %d changed its arguments: got %q want %q", i, args, want[i])
		}
	}
}

func printedCommands(output, program string) []string {
	var commands []string
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, program+" ") {
			commands = append(commands, line)
		}
	}

	return commands
}

func TestChatAddressUsesConnectableHost(t *testing.T) {
	for bind, want := range map[string]string{
		"tcp://0.0.0.0:7700": "tcp://127.0.0.1:7700",
		"tcp://[::]:7700":    "tcp://[::1]:7700",
		"tcp://*:7700":       "tcp://127.0.0.1:7700",
		"tcp://host:7700":    "tcp://host:7700",
		"ipc:///tmp/chat":    "ipc:///tmp/chat",
	} {
		if got := chatAddress(bind); got != want {
			t.Errorf("chatAddress(%s) = %s, want %s", bind, got, want)
		}
	}
}

func TestFailedOptionalCheckKeepsConfigAndLaunchCommands(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	var output strings.Builder
	o := baseOptions("ollama\nlocal\n"+server.URL+"\nmodel\ny\n", true, &output)
	o.ExplicitAgentDir = filepath.Join(t.TempDir(), "agent")
	o.ReadyGuideCheck = func() bool { return true }
	outcome, err := Run(t.Context(), o)
	if err != nil || !outcome.Committed {
		t.Fatalf("optional failure must preserve setup: committed=%v err=%v", outcome.Committed, err)
	}
	if _, err := config.Load(t.Context(), outcome.Resolution.Path); err != nil {
		t.Fatal(err)
	}
	failure := strings.Index(output.String(), "Connection/model check failed")
	if failure < 0 || strings.Index(output.String(), "Next steps") < failure || !strings.Contains(output.String(), executableName(runtime.GOOS)+" chat --agent-dir") {
		t.Fatal("failed check must explain the failure before handing off the saved launch commands")
	}
}

func TestSimpleRelativePathsAreMinimal(t *testing.T) {
	root := t.TempDir()
	o := Options{WorkingDir: root}
	path := filepath.Join(root, "test_ready", "config.yaml")
	want := "." + string(filepath.Separator) + filepath.Join("test_ready", "config.yaml")
	if got := o.pathArgument(path); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestExecutableNamesByPlatform(t *testing.T) {
	for goos, want := range map[string]string{testWindowsOS: "ready.exe", "linux": "ready", "darwin": "ready"} {
		if got := executableName(goos); got != want {
			t.Errorf("%s executable = %s, want %s", goos, got, want)
		}
	}
}
