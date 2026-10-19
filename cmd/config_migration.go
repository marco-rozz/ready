package main

import (
	"context"
	"fmt"
	"os"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
)

// prepareServeConfig is the sole command-side config migration writer. The
// explicit registry parameter lets tests exercise future chains without globals.
func prepareServeConfig(ctx context.Context, path string, opts ServeOptions, registry *config.MigrationRegistry) (*config.Config, config.MigrationReport, error) {
	if registry == nil {
		var err error
		registry, err = config.ProductionMigrations()
		if err != nil {
			return nil, config.MigrationReport{}, err
		}
	}

	return registry.Prepare(ctx, path, func(cfg *config.Config) error {
		if !cfg.HasEnabledChannels() {
			return fmt.Errorf("no channels enabled in configuration")
		}
		if _, _, err := cfg.ResolveProfile(opts.Profile, os.Getenv("READY_MCP_PROFILE")); err != nil {
			return fmt.Errorf("failed to resolve MCP profile: %w", err)
		}

		return nil
	})
}

func logConfigMigration(logger zerolog.Logger, report config.MigrationReport) {
	if report.Committed {
		for _, step := range report.Steps {
			logger.Info().Str("config", report.Path).Int("migration_version", step.Version).
				Str("description", step.Description).Dur("duration", step.Duration).Msg("config migration applied")
		}
		logger.Info().Str("config", report.Path).Int("source_version", report.From).
			Int("target_version", report.To).Str("backup", report.BackupPath).Bool("committed", true).Msg("config migration committed")
	}
	for _, warning := range report.Warnings {
		logger.Warn().Str("config", report.Path).Str("warning", warning).Msg("config backup retention failed")
	}
}
