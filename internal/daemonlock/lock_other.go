//go:build !unix && !windows

package daemonlock

import (
	"fmt"
	"runtime"
)

// tryLock fails closed: a platform without a supported lock primitive never
// runs unowned.
func tryLock(f *os.File, shared bool) (bool, error) {
	return false, fmt.Errorf("%w (%s)", ErrUnsupported, runtime.GOOS)
}
