package exectool

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"strings"
	"sync"

	"github.com/flexigpt/llmtools-go/internal/executil"
	"github.com/flexigpt/llmtools-go/internal/logutil"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

const (
	optionLIC            = "-lic"
	optionCommand        = "-Command"
	optionNoLogo         = "-NoLogo"
	optionNonInteractive = "-NonInteractive"

	envKeyPath          = "PATH"
	envKeyHome          = "HOME"
	envKeyGitSSHCommand = "GIT_SSH_COMMAND"
	envPrefixASDF       = "ASDF_"
	envPrefixPYENV      = "PYENV_"
	envPrefixRBENV      = "RBENV_"
	envPrefixNVM        = "NVM_"
	envPrefixVOLTA      = "VOLTA_"
	envPrefixSDKMAN     = "SDKMAN_"
	envPrefixCONDA      = "CONDA_"
	envKeyUser          = "USER"
	envKeyLogname       = "LOGNAME"
	envKeyShell         = "SHELL"
	envKeyLang          = "LANG"
	envKeyLCAll         = "LC_ALL"
	envPrefixLC         = "LC_"
)

var (
	newExecToolBootstrapOnce sync.Once
	newExecToolBootstrapDefs *BootstrappedDefaults
	errNewExecToolBootstrap  error
)

func cachedBootstrapDefaultsForNewExecTool() (*BootstrappedDefaults, error) {
	newExecToolBootstrapOnce.Do(func() {
		newExecToolBootstrapDefs, errNewExecToolBootstrap = BootstrapDefaults(context.Background())
	})
	return cloneBootstrappedDefaults(newExecToolBootstrapDefs), errNewExecToolBootstrap
}

func cloneBootstrappedDefaults(in *BootstrappedDefaults) *BootstrappedDefaults {
	if in == nil {
		return nil
	}
	return &BootstrappedDefaults{
		DefaultShell: in.DefaultShell,
		BaseEnv:      maps.Clone(in.BaseEnv),
	}
}

// BootstrapDefaults best-effort detects the preferred host shell and a narrow,
// tool-useful base environment suitable for command/script execution.
func BootstrapDefaults(
	ctx context.Context, //nolint:contextcheck // Need independent cancellations.
) (*BootstrappedDefaults, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Shell detection has its own short budget so that a slow host probe
	// cannot eat the env bootstrap budget.
	detectCtx, detectCancel := context.WithTimeout(ctx, defaultBootstrapDetectionTimeout)
	sel, err := selectShell(detectCtx, ShellNameAuto)
	detectCancel()
	if err != nil {
		return nil, err
	}

	out := &BootstrappedDefaults{DefaultShell: sel.Name}
	envCtx, envCancel := context.WithTimeout(ctx, defaultBootstrapEnvTimeout)
	defer envCancel()
	env, envErr := bootstrapBaseEnv(envCtx, sel)
	if envErr != nil {
		return out, envErr
	}
	out.BaseEnv = env
	return out, nil
}

func bootstrapBaseEnv(ctx context.Context, sel executil.SelectedShell) (map[string]string, error) {
	args, err := bootstrapCommandArgs(sel)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, errors.New("invalid bootstrap command")
	}

	// In Flatpak: try running the bootstrap command on the host first so that
	// the user's real login shell, PATH, and tool-manager vars are captured.
	if executil.HostSpawnAvailable(ctx) {
		hostArgs, _ := executil.PrependHostSpawn(ctx, args)
		out, err := executil.RunManagedCombinedOutput(ctx, hostArgs, "", nil)
		if env, parseErr := parseAndFilterBootstrapEnv(string(out)); parseErr == nil {
			if err != nil {
				logutil.WarnContext(ctx, "exectool: bootstrap via host-spawn returned error but env parsed", "err", err)
			}
			return env, nil
		} else {
			if err == nil {
				logutil.WarnContext(
					ctx,
					"exectool: bootstrap parse via host-spawn failed: retrying direct",
					"err",
					parseErr,
				)
			} else {
				logutil.WarnContext(ctx, "exectool: bootstrap exec via host-spawn failed: retrying direct", "err", err)
			}
		}
	}

	// Direct execution (non-Flatpak, or host-spawn fallback).
	out, err := executil.RunManagedCombinedOutput(ctx, args, "", nil)
	if env, parseErr := parseAndFilterBootstrapEnv(string(out)); parseErr == nil {
		if err != nil {
			logutil.WarnContext(ctx, "exectool: bootstrap returned error but env parsed", "err", err)
		}
		return env, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bootstrap env via %s failed: %w", sel.Name, err)
	}

	return parseAndFilterBootstrapEnv(string(out))
}

// parseAndFilterBootstrapEnv parses raw bootstrap output, filters to useful
// variables, and validates the result.
func parseAndFilterBootstrapEnv(output string) (map[string]string, error) {
	raw, err := parseBootstrappedEnv(output)
	if err != nil {
		return nil, err
	}
	filtered := filterBootstrappedEnv(raw)
	if len(filtered) == 0 {
		return nil, errors.New("bootstrap env produced no usable variables")
	}
	if err := validateExecutionEnvMap(filtered); err != nil {
		return nil, err
	}
	return filtered, nil
}

func bootstrapCommandArgs(sel executil.SelectedShell) ([]string, error) {
	switch sel.Name {
	case ShellNameBash, ShellNameZsh:
		cmd := fmt.Sprintf(
			"printf '%%s\\n' '%s'; env; printf '%%s\\n' '%s'",
			bootstrapEnvBeginMarker,
			bootstrapEnvEndMarker,
		)
		return []string{sel.Path, optionLIC, cmd}, nil
	case ShellNameFish:
		cmd := fmt.Sprintf(
			"printf '%%s\\n' '%s'; env; printf '%%s\\n' '%s'",
			bootstrapEnvBeginMarker,
			bootstrapEnvEndMarker,
		)
		return []string{sel.Path, "-l", "-i", "-c", cmd}, nil
	case ShellNameSh, ShellNameDash:
		cmd := fmt.Sprintf(
			"printf '%%s\\n' '%s'; env; printf '%%s\\n' '%s'",
			bootstrapEnvBeginMarker,
			bootstrapEnvEndMarker,
		)
		// POSIX sh and dash do not portably support "-l". In particular,
		// dash rejects "sh -lc ...", which made bootstrap fail on systems
		// where /bin/sh is selected.
		return []string{sel.Path, "-c", cmd}, nil
	case ShellNameKsh:
		cmd := fmt.Sprintf(
			"printf '%%s\\n' '%s'; env; printf '%%s\\n' '%s'",
			bootstrapEnvBeginMarker,
			bootstrapEnvEndMarker,
		)
		return []string{sel.Path, "-lc", cmd}, nil
	case ShellNamePwsh, ShellNamePowershell:
		cmd := fmt.Sprintf(
			"[Console]::Out.WriteLine('%s'); [Environment]::GetEnvironmentVariables().GetEnumerator() | ForEach-Object { [Console]::Out.WriteLine(('{0}={1}' -f $_.Key, $_.Value)) }; [Console]::Out.WriteLine('%s')",
			bootstrapEnvBeginMarker,
			bootstrapEnvEndMarker,
		)
		return []string{sel.Path, optionNoLogo, optionNonInteractive, optionCommand, cmd}, nil
	case ShellNameCmd:
		cmd := fmt.Sprintf("echo %s & set & echo %s", bootstrapEnvBeginMarker, bootstrapEnvEndMarker)
		return []string{sel.Path, "/d", "/s", "/v:off", "/c", cmd}, nil
	default:
		return nil, fmt.Errorf("unsupported shell for bootstrap: %s", sel.Name)
	}
}

func parseBootstrappedEnv(out string) (map[string]string, error) {
	text := strings.ReplaceAll(out, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	begin := -1
	end := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == bootstrapEnvBeginMarker && begin < 0 {
			begin = i
			continue
		}
		if trimmed == bootstrapEnvEndMarker && begin >= 0 {
			end = i
			break
		}
	}
	if begin < 0 || end <= begin {
		return nil, errors.New("could not parse bootstrapped env output")
	}

	outMap := map[string]string{}
	for _, line := range lines[begin+1 : end] {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kk := strings.TrimSpace(k)
		if kk == "" {
			continue
		}
		outMap[kk] = v
	}
	return outMap, nil
}

func filterBootstrappedEnv(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}

	out := make(map[string]string)
	if runtime.GOOS == toolutil.GOOSWindows {
		exact := map[string]struct{}{
			envKeyPath:                {},
			"PATHEXT":                 {},
			"SYSTEMROOT":              {},
			"COMSPEC":                 {},
			"USERPROFILE":             {},
			"HOMEDRIVE":               {},
			"HOMEPATH":                {},
			envKeyHome:                {},
			"APPDATA":                 {},
			"LOCALAPPDATA":            {},
			"PROGRAMDATA":             {},
			"PROGRAMFILES":            {},
			"PROGRAMFILES(X86)":       {},
			"COMMONPROGRAMFILES":      {},
			"COMMONPROGRAMFILES(X86)": {},
			"TMP":                     {},
			"TEMP":                    {},
			"ONEDRIVE":                {},
			"CHOCOLATEYINSTALL":       {},
			"JAVA_HOME":               {},
			"GOBIN":                   {},
			"GOPATH":                  {},
			"GOROOT":                  {},
			"PNPM_HOME":               {},
			"BUN_INSTALL":             {},
			"CARGO_HOME":              {},
			"RUSTUP_HOME":             {},
			"VIRTUAL_ENV":             {},
			"CONDA_PREFIX":            {},
			"CONDA_DEFAULT_ENV":       {},
			"CONDA_EXE":               {},
			"GOPROXY":                 {},
			"GOPRIVATE":               {},
			"GONOPROXY":               {},
			"GONOSUMDB":               {},
			"GOSUMDB":                 {},
			"GOFLAGS":                 {},
			"GOTOOLCHAIN":             {},
			"GOINSECURE":              {},
			"GOVCS":                   {},
			"HTTP_PROXY":              {},
			"HTTPS_PROXY":             {},
			"NO_PROXY":                {},
			"ALL_PROXY":               {},
			envKeyGitSSHCommand:       {},
			"GIT_ASKPASS":             {},
			"GIT_SSH":                 {},
			"SSH_AUTH_SOCK":           {},
			"SSL_CERT_FILE":           {},
			"SSL_CERT_DIR":            {},
		}
		prefixes := []string{
			envPrefixASDF,
			envPrefixPYENV,
			envPrefixRBENV,
			envPrefixNVM,
			envPrefixVOLTA,
			envPrefixSDKMAN,
			envPrefixCONDA,
			"GIT_",
		}
		for k, v := range raw {
			ck := strings.ToUpper(strings.TrimSpace(k))
			if _, ok := exact[ck]; ok || hasAnyPrefix(ck, prefixes) {
				out[k] = v
			}
		}
		return out
	}

	exact := map[string]struct{}{
		envKeyPath:          {},
		envKeyHome:          {},
		envKeyUser:          {},
		envKeyLogname:       {},
		envKeyShell:         {},
		"TMPDIR":            {},
		"TMP":               {},
		"TEMP":              {},
		envKeyLang:          {},
		envKeyLCAll:         {},
		"LC_CTYPE":          {},
		"TERM":              {},
		"COLORTERM":         {},
		"XDG_CONFIG_HOME":   {},
		"XDG_CACHE_HOME":    {},
		"XDG_DATA_HOME":     {},
		"GOBIN":             {},
		"GOPATH":            {},
		"GOROOT":            {},
		"JAVA_HOME":         {},
		"PNPM_HOME":         {},
		"BUN_INSTALL":       {},
		"CARGO_HOME":        {},
		"RUSTUP_HOME":       {},
		"VIRTUAL_ENV":       {},
		"CONDA_PREFIX":      {},
		"CONDA_DEFAULT_ENV": {},
		"CONDA_EXE":         {},
		"GOPROXY":           {},
		"GOPRIVATE":         {},
		"GONOPROXY":         {},
		"GONOSUMDB":         {},
		"GOSUMDB":           {},
		"GOFLAGS":           {},
		"GOTOOLCHAIN":       {},
		"GOINSECURE":        {},
		"GOVCS":             {},
		"HTTP_PROXY":        {},
		"HTTPS_PROXY":       {},
		"NO_PROXY":          {},
		"ALL_PROXY":         {},
		"http_proxy":        {},
		"https_proxy":       {},
		"no_proxy":          {},
		"all_proxy":         {},
		envKeyGitSSHCommand: {},
		"GIT_ASKPASS":       {},
		"GIT_SSH":           {},
		"SSH_AUTH_SOCK":     {},
		"SSL_CERT_FILE":     {},
		"SSL_CERT_DIR":      {},
	}
	prefixes := []string{
		envPrefixASDF,
		envPrefixPYENV,
		envPrefixRBENV,
		envPrefixNVM,
		envPrefixVOLTA,
		envPrefixSDKMAN,
		envPrefixLC,
		envPrefixCONDA,
		"GIT_",
	}
	for k, v := range raw {
		ck := strings.TrimSpace(k)
		if _, ok := exact[ck]; ok || hasAnyPrefix(ck, prefixes) {
			out[k] = v
		}
	}
	return out
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
