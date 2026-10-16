package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/persistence"
)

// Rule is a Session reset rule (sessions.reset, per-channel override
// resolved by the caller).
type Rule struct {
	// Mode is never, daily, idle, or daily+idle.
	Mode string
	// DailyAt is the local HH:MM daily boundary.
	DailyAt string
	// IdleAfter is the idle window (idle modes).
	IdleAfter time.Duration
	// Notify sends the fixed notice on message-oriented channels.
	Notify bool
}

// RuleFrom converts a resolved config rule.
func RuleFrom(c config.SessionResetConfig) Rule {
	return Rule{Mode: c.Mode, DailyAt: c.DailyAt, IdleAfter: c.IdleAfter, Notify: c.NotifyEnabled()}
}

// Due reports whether a Session whose last Turn happened at lastTurn is due
// for reset at now, and the closing reason. It is a pure function of its
// inputs — no timer ever evaluates a rule, and no model decides a reset. A
// Session with no Turn (zero lastTurn) is never due: there is nothing to
// archive. Under daily+idle the daily boundary wins the reason.
func Due(rule Rule, lastTurn, now time.Time) (reason string, due bool) {
	if lastTurn.IsZero() || rule.Mode == config.ResetModeNever {
		return "", false
	}

	if strings.Contains(rule.Mode, config.ResetModeDaily) {
		if boundary, err := lastDailyBoundary(rule.DailyAt, now); err == nil && lastTurn.Before(boundary) {
			return persistence.ReasonResetDaily, true
		}
	}

	if strings.Contains(rule.Mode, config.ResetModeIdle) && rule.IdleAfter > 0 && now.Sub(lastTurn) >= rule.IdleAfter {
		return persistence.ReasonResetIdle, true
	}

	return "", false
}

// lastDailyBoundary returns the most recent instant at or before now whose
// local wall clock reads dailyAt (in now's location). time.Date normalises a
// wall-clock time skipped by a DST jump forward (e.g. 02:30 becomes 03:30)
// and picks the first occurrence of one repeated by a fall-back, so the
// boundary is well defined every day.
func lastDailyBoundary(dailyAt string, now time.Time) (time.Time, error) {
	at, err := time.Parse("15:04", dailyAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid daily_at %q: %w", dailyAt, err)
	}

	y, m, d := now.Date()

	boundary := time.Date(y, m, d, at.Hour(), at.Minute(), 0, 0, now.Location())
	if boundary.After(now) {
		boundary = time.Date(y, m, d-1, at.Hour(), at.Minute(), 0, 0, now.Location())
	}

	return boundary, nil
}
