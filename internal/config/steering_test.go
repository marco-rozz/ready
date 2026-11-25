package config

import (
	"context"
	"strings"
	"testing"
	"time"
)

const steeringTestBase = `
config_version: 2
agent:
  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001
llm:
  default: "ollama-gemma::gemma4:31b"
  providers:
    ollama-gemma:
      provider: "ollama"
      api: ollama
channels:
  cli:
    enabled: true
`

func TestSteering_Defaults(t *testing.T) {
	cfg, err := Load(context.Background(), writeTempFile(t, steeringTestBase))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	s := cfg.Steering
	if !s.SteeringEnabled() || s.MaxSteersPerTurn != 8 || s.MaxRestartsPerTurn != 4 || s.RemainingBudget() != 30*time.Second {
		t.Errorf("unexpected defaults: %+v", s)
	}

	for _, ch := range []string{chanCLI, segZmqcli, chanSerial, segTelegram} {
		if !cfg.EffectiveSteering(ch) {
			t.Errorf("channel %s: want steering on by default", ch)
		}
	}
}

func TestSteering_OutOfRangeKeysNameTheKey(t *testing.T) {
	cases := map[string]string{
		"max_steers_per_turn":   "  max_steers_per_turn: 33\n",
		"max_steers_per_turn_0": "  max_steers_per_turn: -1\n",
		"max_restarts_per_turn": "  max_restarts_per_turn: 17\n",
		"min_remaining_budget":  "  min_remaining_budget: 301s\n",
	}

	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(context.Background(), writeTempFile(t, steeringTestBase+"steering:\n"+block))
			if err == nil {
				err = cfg.Validate()
			}

			if err == nil {
				t.Fatal("expected a validation error")
			}

			want := "steering." + strings.TrimSuffix(strings.TrimSuffix(name, "_0"), "")
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		})
	}
}

func TestSteering_ChannelOverrideWins(t *testing.T) {
	content := steeringTestBase + "  " + segTelegram + ":\n    steering: false\nsteering:\n  enabled: true\n"

	cfg, err := Load(context.Background(), writeTempFile(t, content))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.EffectiveSteering("telegram") {
		t.Error("telegram override false must win over enabled: true")
	}

	if !cfg.EffectiveSteering("cli") {
		t.Error("cli must inherit the global switch")
	}

	off := steeringTestBase + "  zmqcli:\n    steering: true\nsteering:\n  enabled: false\n"

	cfg, err = Load(context.Background(), writeTempFile(t, off))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.EffectiveSteering("zmqcli") || cfg.EffectiveSteering("cli") {
		t.Error("zmqcli override true must win; cli must follow the disabled global switch")
	}
}

func TestSteering_AdditiveOnVersion2(t *testing.T) {
	content := steeringTestBase + "steering:\n  max_steers_per_turn: 2\n  min_remaining_budget: 0s\n"

	cfg, err := Load(context.Background(), writeTempFile(t, content))
	if err != nil {
		t.Fatalf("load on config_version 2: %v", err)
	}

	if cfg.Steering.MaxSteersPerTurn != 2 || cfg.Steering.RemainingBudget() != 0 {
		t.Errorf("unexpected values: %+v", cfg.Steering)
	}
}
