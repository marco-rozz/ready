package config

import (
	"fmt"
	"time"
)

// Channel config keys (the YAML keys under `channels:`), shared by every
// per-channel override table.
const (
	chanCLI    = "cli"
	chanSerial = "serial"
)

// Steering defaults and bounds (turn-steering S13/S18).
const (
	defaultMaxSteersPerTurn    = 8
	minMaxSteersPerTurn        = 1
	maxMaxSteersPerTurn        = 32
	defaultMaxRestartsPerTurn  = 4
	minMaxRestartsPerTurn      = 1
	maxMaxRestartsPerTurn      = 16
	defaultMinRemainingBudget  = 30 * time.Second
	maxMinRemainingBudgetLimit = 300 * time.Second // the daemon's agent-run timeout
)

// SteeringConfig holds the top-level `steering` block: whether a human
// message sent while their Turn runs enters that Turn, and the per-Turn
// limits. Reaching a limit defers candidates to the next Turn; it never
// produces an error.
type SteeringConfig struct {
	// Enabled toggles steering for every channel (default true); a channel's
	// own `steering` key overrides it.
	Enabled *bool `yaml:"enabled"`
	// MaxSteersPerTurn bounds the candidates one Turn absorbs (default 8,
	// 1..32). Beyond it the steering window closes.
	MaxSteersPerTurn int `yaml:"max_steers_per_turn"`
	// MaxRestartsPerTurn bounds the preemption restarts of one Turn (default
	// 4, 1..16). Beyond it hooks stop preempting and candidates defer.
	MaxRestartsPerTurn int `yaml:"max_restarts_per_turn"`
	// MinRemainingBudget is the least remaining outer-deadline budget at which
	// a claim is still performed (default 30s, 0..300s).
	MinRemainingBudget *time.Duration `yaml:"min_remaining_budget"`
}

// SteeringEnabled reports the global steering switch (default true).
func (s SteeringConfig) SteeringEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// RemainingBudget returns the effective minimum remaining budget.
func (s SteeringConfig) RemainingBudget() time.Duration {
	if s.MinRemainingBudget == nil {
		return defaultMinRemainingBudget
	}

	return *s.MinRemainingBudget
}

func (c *Config) applySteeringDefaults() {
	s := &c.Steering

	if s.MaxSteersPerTurn == 0 {
		s.MaxSteersPerTurn = defaultMaxSteersPerTurn
	}

	if s.MaxRestartsPerTurn == 0 {
		s.MaxRestartsPerTurn = defaultMaxRestartsPerTurn
	}

	if s.MinRemainingBudget == nil {
		d := defaultMinRemainingBudget
		s.MinRemainingBudget = &d
	}
}

// channelSteering maps each channel config key to its optional override.
func (c *Config) channelSteering() map[string]*bool {
	return map[string]*bool{
		chanCLI:     c.Channels.CLI.Steering,
		segZmqcli:   c.Channels.ZMQCLI.Steering,
		chanSerial:  c.Channels.Serial.Steering,
		segTelegram: c.Channels.Telegram.Steering,
	}
}

// EffectiveSteering reports whether steering is enabled for a channel config
// key (cli, zmqcli, serial, telegram): the channel override wins over the
// global switch. An unknown key follows the global switch.
func (c *Config) EffectiveSteering(channelKey string) bool {
	if v := c.channelSteering()[channelKey]; v != nil {
		return *v
	}

	return c.Steering.SteeringEnabled()
}

func (c *Config) validateSteering() error {
	s := c.Steering

	if s.MaxSteersPerTurn != 0 && (s.MaxSteersPerTurn < minMaxSteersPerTurn || s.MaxSteersPerTurn > maxMaxSteersPerTurn) {
		return fmt.Errorf("steering.max_steers_per_turn must be between %d and %d, got %d", minMaxSteersPerTurn, maxMaxSteersPerTurn, s.MaxSteersPerTurn) // user-facing config error
	}

	if s.MaxRestartsPerTurn != 0 && (s.MaxRestartsPerTurn < minMaxRestartsPerTurn || s.MaxRestartsPerTurn > maxMaxRestartsPerTurn) {
		return fmt.Errorf("steering.max_restarts_per_turn must be between %d and %d, got %d", minMaxRestartsPerTurn, maxMaxRestartsPerTurn, s.MaxRestartsPerTurn) // user-facing config error
	}

	if d := s.RemainingBudget(); d < 0 || d > maxMinRemainingBudgetLimit {
		return fmt.Errorf("steering.min_remaining_budget must be between 0s and %s, got %s", maxMinRemainingBudgetLimit, d) // user-facing config error
	}

	return nil
}
