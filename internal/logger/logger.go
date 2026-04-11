// Package logger provides a centralized zerolog logger for the application.
package logger

import (
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	// Set a reasonable default before Init() is called.
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).With().Timestamp().Logger()
	zerolog.TimeFieldFormat = time.RFC3339
}

// Init configures the global logger.  level may be one of "debug", "info",
// "warn", "error", "fatal", "panic" (case-insensitive).  An empty string
// defaults to "info".  If json is true the output is structured JSON instead
// of the human-friendly console format.
func Init(level string, json bool) {
	var output zerolog.ConsoleWriter = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}

	if json {
		log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
	} else {
		log.Logger = zerolog.New(output).With().Timestamp().Logger()
	}

	zerolog.SetGlobalLevel(parseLevel(level))
}

func parseLevel(s string) zerolog.Level {
	switch s {
	case "debug":
		return zerolog.DebugLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "fatal":
		return zerolog.FatalLevel
	case "panic":
		return zerolog.PanicLevel
	case "trace":
		return zerolog.TraceLevel
	default:
		return zerolog.InfoLevel
	}
}
