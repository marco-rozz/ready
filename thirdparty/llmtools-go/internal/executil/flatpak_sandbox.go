package executil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/flexigpt/llmtools-go/internal/logutil"
)

// Flatpak sandbox detection and host-spawn helpers.
//
// When running inside a Flatpak sandbox the process sees the sandbox's shells,
// PATH, and env — not the host user's.  These helpers allow transparent escape
// via "flatpak-spawn --host" so that bootstrap and command execution use the
// real host environment.
//
// Required Flatpak finish-args:
//
//	--talk-name=org.freedesktop.Flatpak   (portal access for host-spawn)
//	--filesystem=host                      (consistent path mapping)
var (
	flatpakDetectOnce sync.Once
	flatpakDetected   bool

	hostSpawnProbeOnce sync.Once
	hostSpawnOK        bool
	hostSpawnBin       string // absolute path to flatpak-spawn inside the sandbox
)

var errHostSpawnUnavailable = errors.New("host spawn not available")

// PrependHostSpawn wraps args with a robust flatpak-spawn invocation:
//
//	--host         run on host
//	--watch-bus    host child is killed if the sandbox parent dies
//	--directory=…  set a sane cwd on host (defaults to $HOME on host)
//	--clear-env    do not leak sandbox env into host
//	--env=KEY=VAL  forward a filtered subset (PATH-related, locale, etc.)
func PrependHostSpawn(ctx context.Context, args []string) ([]string, bool) {
	if !HostSpawnAvailable(ctx) || len(args) == 0 {
		return args, false
	}

	envList := filterEnvForHostSpawn(os.Environ())

	out := make([]string, 0, 6+len(envList)+len(args))
	out = append(out, hostSpawnBin, "--host", "--watch-bus", "--clear-env")

	if home := os.Getenv("HOME"); strings.TrimSpace(home) != "" {
		out = append(out, "--directory="+home)
	}
	for _, kv := range envList {
		out = append(out, "--env="+kv)
	}

	out = append(out, args...)
	return out, true
}

// buildHostSpawnArgs builds a flatpak-spawn --host invocation with --env=
// flags for every entry in envList (format: "KEY=VALUE") and a concrete host cwd.
func buildHostSpawnArgs(cmdArgs, envList []string, workdir string) []string {
	out := make([]string, 0, 3+len(envList)+len(cmdArgs))
	out = append(out, hostSpawnBin, "--host")
	if strings.TrimSpace(workdir) != "" {
		out = append(out, "--directory="+workdir)
	}
	for _, kv := range envList {
		out = append(out, "--env="+kv)
	}
	out = append(out, cmdArgs...)
	return out
}

// filterEnvForHostSpawn removes Flatpak-sandbox-internal variables from env
// before forwarding to a host command via --env= flags.
func filterEnvForHostSpawn(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		if isSandboxInternalVar(strings.TrimSpace(k)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// isSandboxInternalVar returns true for env var names set by the Flatpak
// runtime/sandbox that should not be forwarded to host commands.
func isSandboxInternalVar(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, "FLATPAK_") {
		return true
	}
	switch upper {
	case "CONTAINER", // systemd/Flatpak container indicator
		"LD_LIBRARY_PATH",            // sandbox library paths
		"LD_PRELOAD",                 // sandbox preloads
		"GI_TYPELIB_PATH",            // GObject introspection sandbox paths
		"GST_PLUGIN_SYSTEM_PATH",     // GStreamer sandbox
		"GST_PLUGIN_SYSTEM_PATH_1_0", // GStreamer sandbox
		"GST_PLUGIN_SCANNER",         // GStreamer sandbox
		"GDK_PIXBUF_MODULE_FILE",     // GDK sandbox
		"FONTCONFIG_FILE",            // Fontconfig sandbox
		"XDG_DATA_DIRS",              // includes sandbox data directories
		"XDG_CONFIG_DIRS":            // includes sandbox config directories
		return true
	}
	return false
}

// ResolveHostAutoShell detects the preferred shell on the host system via
// flatpak-spawn --host.  Falls back through $SHELL, getent, and common
// shell probes.
func ResolveHostAutoShell(ctx context.Context) (SelectedShell, bool) {
	if ctx == nil {
		ctx = context.Background() //nolint:contextcheck // Need independent cancellations.
	}
	// Per-probe budget. Total budget is bounded by the caller's ctx; this
	// prevents one slow probe from starving the fallbacks.
	const perProbe = 1500 * time.Millisecond

	hostExecBounded := func(args ...string) ([]byte, error) {
		pctx, pcancel := context.WithTimeout(ctx, perProbe)
		defer pcancel()
		return HostExec(pctx, args...)
	}
	// 1. Host's $SHELL.
	if out, err := hostExecBounded("sh", "-c", "echo $SHELL"); err == nil {
		if sel, ok := hostShellFromPath(strings.TrimSpace(string(out))); ok {
			return sel, true
		}
	}

	// 2. "getent passwd" on the host.
	if u, err := user.Current(); err == nil && u != nil {
		username := strings.TrimSpace(u.Username)
		if username != "" {
			if out, err := hostExecBounded("getent", "passwd", username); err == nil {
				line := strings.TrimSpace(string(out))
				parts := strings.Split(line, ":")
				if len(parts) >= 7 {
					if sel, ok := hostShellFromPath(strings.TrimSpace(parts[6])); ok {
						return sel, true
					}
				}
			}
		}
	}

	// 3. Probe common shells on the host.
	for _, name := range []string{"bash", "zsh", "sh"} {
		if out, err := hostExecBounded("sh", "-c", "command -v "+name); err == nil {
			if sel, ok := hostShellFromPath(strings.TrimSpace(string(out))); ok {
				return sel, true
			}
		}
	}

	return SelectedShell{}, false
}

// HostExec runs a command on the host via flatpak-spawn --host and returns its
// stdout.  Returns an error when host spawn is unavailable or the command fails.
func HostExec(ctx context.Context, args ...string) ([]byte, error) {
	if !HostSpawnAvailable(ctx) {
		return nil, errHostSpawnUnavailable
	}
	full := make([]string, 0, 2+len(args))
	full = append(full, hostSpawnBin, "--host")
	full = append(full, args...)
	//nolint:gosec // Host discovery hand-crafted.
	return exec.CommandContext(ctx, full[0], full[1:]...).Output()
}

// HostSpawnAvailable reports whether flatpak-spawn --host can be used to run
// commands on the host.  Returns false when not inside Flatpak or the probe
// command fails.  The result is cached after the first probe.
func HostSpawnAvailable(ctx context.Context) bool {
	if !IsFlatpak() {
		return false
	}
	hostSpawnProbeOnce.Do(func() {
		p, err := exec.LookPath("flatpak-spawn")
		if err != nil {
			logutil.Warn("executil: flatpak-spawn not found: host commands will run inside the sandbox", "err", err)
			return
		}
		hostSpawnBin = p

		// Hard cap probe so it can never consume bootstrap budget.
		ctxTimed, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := exec.CommandContext(ctxTimed, p, "--host", "--watch-bus", "true").Run(); err != nil {
			logutil.Warn(
				"executil: flatpak-spawn --host probe failed: ensure --talk-name=org.freedesktop.Flatpak is in finish-args",
				"err",
				err,
			)
			return
		}
		hostSpawnOK = true
		logutil.Info("executil: flatpak-spawn --host available commands will execute on the host")
	})
	return hostSpawnOK
}

// IsFlatpak reports whether the current process runs inside a Flatpak sandbox.
// The result is cached after the first call.
func IsFlatpak() bool {
	flatpakDetectOnce.Do(func() {
		_, err := os.Stat("/.flatpak-info")
		flatpakDetected = err == nil
		if flatpakDetected {
			logutil.Info("executil: Flatpak sandbox detected")
		}
	})
	return flatpakDetected
}

// hostShellFromPath extracts a SelectedShell from a host shell absolute path.
func hostShellFromPath(path string) (SelectedShell, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return SelectedShell{}, false
	}
	base := strings.ToLower(filepath.Base(path))
	norm, err := NormalizeShellName(ShellName(base))
	if err != nil || norm == ShellNameAuto {
		return SelectedShell{}, false
	}
	return SelectedShell{Name: norm, Path: path}, true
}
