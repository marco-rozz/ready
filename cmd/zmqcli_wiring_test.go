package main

import (
	"testing"

	"github.com/marco-rozz/ready/internal/channel/cli"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/config"
)

// TestFormatterRegistry_SingleCLIEntry verifies that exactly one "cli" entry
// appears in the formatter registry regardless of whether the legacy stdin cli
// or the zmqcli transport is enabled. The two channels are mutually exclusive
// (enforced by config.Validate), but the registry logic must reflect that
// invariant — a typo here would silently double-register "cli".
func TestFormatterRegistry_SingleCLIEntry(t *testing.T) {
	t.Run("legacy cli enabled", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Channels.CLI.Enabled = true
		cfg.Channels.CLI.FormatterSkill = "test-fmt"

		reg := buildFormatterRegistry(cfg)

		// Only one "cli" entry.
		n := 0
		for name := range reg {
			if name == cli.ChannelName {
				n++
			}
		}

		if n != 1 {
			t.Fatalf("expected exactly 1 'cli' entry in formatterRegistry (legacy cli), got %d", n)
		}

		// The entry should carry the formatter config set above.
		if reg[cli.ChannelName].skillName != "test-fmt" {
			t.Fatalf("formatterRegistry['cli'].skillName = %q, want %q", reg[cli.ChannelName].skillName, "test-fmt")
		}
	})

	t.Run("zmqcli enabled", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Channels.ZMQCLI.Enabled = true
		cfg.Channels.ZMQCLI.FormatterSkill = "zmq-fmt"

		reg := buildFormatterRegistry(cfg)

		// zmqcli uses the same ChannelName constant as the stdin cli ("cli").
		if zmqcli.ChannelName != cli.ChannelName {
			t.Fatalf("zmqcli.ChannelName (%q) != cli.ChannelName (%q); the constants must match", zmqcli.ChannelName, cli.ChannelName)
		}

		n := 0
		for name := range reg {
			if name == zmqcli.ChannelName {
				n++
			}
		}

		if n != 1 {
			t.Fatalf("expected exactly 1 'cli' entry in formatterRegistry (zmqcli), got %d", n)
		}

		if reg[zmqcli.ChannelName].skillName != "zmq-fmt" {
			t.Fatalf("formatterRegistry['cli'].skillName = %q, want %q", reg[zmqcli.ChannelName].skillName, "zmq-fmt")
		}
	})

	t.Run("neither enabled — no cli entry", func(t *testing.T) {
		cfg := &config.Config{}

		reg := buildFormatterRegistry(cfg)

		if _, ok := reg[cli.ChannelName]; ok {
			t.Fatalf("expected no 'cli' entry in formatterRegistry when neither cli nor zmqcli is enabled")
		}
	})
}
