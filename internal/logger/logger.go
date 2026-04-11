// Package logger provides a centralized zerolog logger for the application.
package logger

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// L is the global logger instance. It is initialized to a sensible default
// (console writer on stderr, InfoLevel) and should be configured once in main
// via Init.
var L = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).With().Timestamp().Logger()

// Init configures the global logger.  level may be one of "debug", "info",
// "warn", "error", "fatal", "panic" (case-insensitive).  An empty string
// defaults to "info".  If json is true the output is structured JSON instead
// of the human-friendly console format.
func Init(level string, json bool) {
	var output io.Writer = os.Stderr

	if json {
		output = os.Stderr
	} else {
		output = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
	}

	lvl := parseLevel(level)
	L = zerolog.New(output).Level(lvl).With().Timestamp().Logger()
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
