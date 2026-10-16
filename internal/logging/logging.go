// Package logging constructs the root zerolog.Logger from application config:
// a console (stderr) sink and an optional rotating JSON log file, each with
// its own level (see the architecture decision records and openspec change log-file-rotation).
package logging

import (
	"fmt"
	"io"
	"os"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
	"golang.org/x/term"
)

const (
	debugLabel = "debug"
	infoLabel  = "info"
	traceLabel = "trace"
	warnLabel  = "warn"
	errorLabel = "error"
)

// envLevel is the documented level-override environment variable. The
// former READY_LOG_LEVEL spelling replaces both the old READY_HOME prefixed
// name and the deprecated HOME_AGENT_LOG_LEVEL alias, which are not
// recognised at all (unreleased project: break, no deprecation window).
const envLevel = "READY_LOG_LEVEL"

// Result is what New builds.
type Result struct {
	// Logger is the root logger; its level is the lower of the two sinks'.
	Logger zerolog.Logger
	// Closer closes the log file (a no-op without file_path). Log the final
	// record before calling it: later records never reach the file.
	Closer io.Closer
	// FileLevel and ConsoleLevel are the effective per-sink levels, after
	// the environment override. FileLevel is meaningful only when HasFile.
	FileLevel    zerolog.Level
	ConsoleLevel zerolog.Level
	HasFile      bool
	// Warnings are boot WARNs for the caller to log once the logger exists
	// (deprecated or invalid env override, legacy log file, ...).
	Warnings []string
}

// nopCloser is a no-op io.Closer returned when no file sink is configured.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// New builds the root logger for ready serve from cfg, reading the level
// override from the process environment and writing the console log to
// os.Stderr (coloured only when stderr is a terminal). Failure to create the
// log directory, take its lock, or open the first segment is returned.
func New(cfg config.LoggingConfig) (Result, error) {
	color := term.IsTerminal(int(os.Stderr.Fd()))

	return build(cfg, os.Getenv, os.Stderr, color, nil)
}

// build is New with injectable environment, console output, colour
// detection and rotation options (tests).
func build(cfg config.LoggingConfig, getenv func(string) string, stderr io.Writer, color bool,
	mod func(*rotateOptions),
) (Result, error) {
	// TimeFormatUnixMs is idempotent — setting it on every call is safe and
	// avoids a package-level sync.Once variable.
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs

	fileLevel, consoleLevel, warnings := resolveLevels(cfg, getenv)
	console := buildStderrWriter(cfg.Format, stderr, color)

	res := Result{Closer: nopCloser{}, FileLevel: fileLevel, ConsoleLevel: consoleLevel, Warnings: warnings}

	if cfg.FilePath == "" {
		res.Logger = zerolog.New(console).Level(consoleLevel).With().Timestamp().Logger()

		return res, nil
	}

	rf, rfWarnings, err := openRotatingFile(fileRotateOptions(cfg, console, mod))
	if err != nil {
		return Result{}, fmt.Errorf("failed to open log file %q: %w", cfg.FilePath, err)
	}

	res.Warnings = append(res.Warnings, rfWarnings...)
	res.Closer = rf
	res.HasFile = true

	// Each sink filters on its own level; the root level is the lower one so
	// records reach both filters. multiLevelWriter writes every sink even
	// when one fails, so a file failure never hides the console record.
	sink := zerolog.MultiLevelWriter(
		levelFilter{w: console, min: consoleLevel},
		levelFilter{w: rf, min: fileLevel},
	)
	res.Logger = zerolog.New(sink).Level(min(fileLevel, consoleLevel)).With().Timestamp().Logger()

	return res, nil
}

// fileRotateOptions maps the config onto RotatingFile options and wires the
// failure reporter: a dedicated logger over the unfiltered console writer, so
// the transition WARN/INFO bypasses console_level and never re-enters the
// RotatingFile it reports on.
func fileRotateOptions(cfg config.LoggingConfig, console io.Writer, mod func(*rotateOptions)) rotateOptions {
	reporter := zerolog.New(console).With().Timestamp().Str("component", "logging").
		Str("file_path", cfg.FilePath).Logger()

	opts := rotateOptions{
		Path:           cfg.FilePath,
		MaxSegmentSize: cfg.FileRotation.EffectiveMaxSegmentSize(),
		MaxTotalSize:   cfg.FileRotation.EffectiveMaxTotalSize(),
		RetentionDays:  cfg.FileRotation.EffectiveRetentionDays(),
		Report: func(healthy bool, err error) {
			if healthy {
				reporter.Info().Msg("log file recovered; records are written to the log file again")

				return
			}

			reporter.Warn().Err(err).Msg("log file is failing; records are dropped from the log file until it recovers (console log unaffected)")
		},
		ReportRetention: func(err error) {
			reporter.Warn().Err(err).Msg("log retention could not delete or list a segment; will retry on the next rotation")
		},
	}

	if mod != nil {
		mod(&opts)
	}

	return opts
}

// levelFilter forwards records at or above min to w. A dropped record reports
// len(p) so zerolog's multiLevelWriter does not turn it into ErrShortWrite.
type levelFilter struct {
	w   io.Writer
	min zerolog.Level
}

// Guarantees at compile time that levelFilter implements zerolog.LevelWriter.
var _ zerolog.LevelWriter = levelFilter{}

func (f levelFilter) Write(p []byte) (int, error) {
	return f.w.Write(p)
}

func (f levelFilter) WriteLevel(l zerolog.Level, p []byte) (int, error) {
	if l < f.min {
		return len(p), nil
	}

	return f.w.Write(p)
}

// resolveLevels derives the file and console levels from cfg and the
// environment override (READY_LOG_LEVEL), which forces both sinks — including
// a console set to "off", so the env var stays a debugging escape hatch.
// Unrecognised YAML levels fall back silently (config.ValidateLogging already
// warned about them).
func resolveLevels(cfg config.LoggingConfig, getenv func(string) string) (file, console zerolog.Level, warnings []string) {
	file, _ = parseLevel(cfg.Level)
	console = file

	switch {
	case cfg.ConsoleLevel == config.ConsoleLevelOff:
		// "off" keeps fatal/panic records: a daemon that exits via
		// logger.Fatal must never die silently on the console.
		console = zerolog.FatalLevel
	case cfg.ConsoleLevel != "":
		if parsed, ok := parseLevel(cfg.ConsoleLevel); ok {
			console = parsed
		}
	}

	value := getenv(envLevel)
	if value == "" {
		return file, console, warnings
	}

	parsed, ok := parseLevel(value)
	if !ok {
		warnings = append(warnings, fmt.Sprintf("%s=%q is not a recognised level; using the YAML-configured levels", envLevel, value))

		return file, console, warnings
	}

	return parsed, parsed, warnings
}

// buildStderrWriter returns a ConsoleWriter for "console" format (colours
// only when color is true), or the raw writer for "json" format.
func buildStderrWriter(format string, stderr io.Writer, color bool) io.Writer {
	if format == "json" {
		return stderr
	}

	return zerolog.ConsoleWriter{Out: stderr, NoColor: !color}
}

// parseLevel maps a level name string to a zerolog.Level. The second return
// value is false when the name is not recognised.
func parseLevel(name string) (zerolog.Level, bool) {
	switch name {
	case traceLabel:
		return zerolog.TraceLevel, true
	case debugLabel:
		return zerolog.DebugLevel, true
	case infoLabel:
		return zerolog.InfoLevel, true
	case warnLabel:
		return zerolog.WarnLevel, true
	case errorLabel:
		return zerolog.ErrorLevel, true
	default:
		return zerolog.InfoLevel, false
	}
}
