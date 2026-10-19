package executil

import (
	"fmt"
	"strings"
)

func NormalizeShellName(shell ShellName) (ShellName, error) {
	s := ShellName(strings.ToLower(strings.TrimSpace(string(shell))))
	switch s {
	case ShellNameAuto,
		ShellNameBash,
		ShellNameZsh,
		ShellNameSh,
		ShellNameDash,
		ShellNameKsh,
		ShellNameFish,
		ShellNamePwsh,
		ShellNamePowershell,
		ShellNameCmd:
		return s, nil
	default:
		return "", fmt.Errorf("invalid shell: %q", shell)
	}
}
