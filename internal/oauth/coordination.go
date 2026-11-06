// Credential coordination (per-user-install-agent-isolation): every mutation
// of one target's credential record — daemon-side refresh rotation, login
// commit, forget — serializes cross-process on a per-target lock file inside
// the oauth directory, and every write bumps a monotonic generation so
// stale interactive work cannot clobber newer state and forgotten records
// cannot be resurrected. The daemon's token source re-checks the generation
// on every token fetch, so a replaced/forgotten record is observed instead
// of served from a stale cache.

package oauth

import (
	"errors"
	"time"
)

// ErrGenerationConflict reports that the stored credential for a target
// changed (login/refresh/forget elsewhere) between a snapshot and a commit.
var ErrGenerationConflict = errors.New("oauth credential changed under us")

// ErrTargetLockBusy reports that a per-target coordination lock could not be
// acquired within its budget.
var ErrTargetLockBusy = errors.New("oauth target lock busy")

// targetLockWait bounds each per-target lock acquisition: mutations are
// quick file writes, never human-paced work (interactive authorization
// releases target locks by design — it holds none).
const targetLockWait = 10 * time.Second

// locksDir is the coordination subdirectory inside the oauth store.
const locksDir = "locks"
