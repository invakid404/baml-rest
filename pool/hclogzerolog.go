// Package pool provides hclogzerolog, a wrapper for zerolog.Logger to be used as hclog.Logger
package pool

import (
	"io"
	"log"

	"github.com/hashicorp/go-hclog"
	"github.com/rs/zerolog"
)

// hclogNameField is the field hclog.Logger name will be written to.
//
// hclog has a concept of logger name and, moreover, name inheritance
// (when you create named logger on top of named logger).
// Logger name acts like a prefix for the log message.
// On the other hand, zerolog operates key/value pairs to add context to messages.
// So, we convert the hclog logger name to key/value context pair for zerolog.
const hclogNameField = "hclog_name"

type hclogZerolog struct {
	logger    zerolog.Logger
	nameField string
	name      string
}

// newHclogZerolog creates an instance of hclogZerolog wrapping provided zerolog.Logger.
func newHclogZerolog(logger zerolog.Logger) *hclogZerolog {
	return &hclogZerolog{
		logger:    logger,
		nameField: hclogNameField,
		name:      "",
	}
}

func (l *hclogZerolog) Log(level hclog.Level, msg string, args ...any) {
	switch level {
	case hclog.Trace:
		l.logger.Trace().Fields(args).Msg(msg)
	case hclog.Debug:
		l.logger.Debug().Fields(args).Msg(msg)
	case hclog.Info:
		l.logger.Info().Fields(args).Msg(msg)
	case hclog.Warn:
		l.logger.Warn().Fields(args).Msg(msg)
	case hclog.Error:
		l.logger.Error().Fields(args).Msg(msg)
	case hclog.NoLevel:
		l.logger.Log().Fields(args).Msg(msg)
	default:
		l.logger.Error().Msgf("Unknown log level: %s", level)
	}
}

func (l *hclogZerolog) Trace(format string, args ...any) {
	l.logger.Trace().Fields(args).Msg(format)
}

func (l *hclogZerolog) Debug(format string, args ...any) {
	l.logger.Debug().Fields(args).Msg(format)
}

func (l *hclogZerolog) Info(format string, args ...any) {
	l.logger.Info().Fields(args).Msg(format)
}

func (l *hclogZerolog) Warn(format string, args ...any) {
	l.logger.Warn().Fields(args).Msg(format)
}

func (l *hclogZerolog) Error(format string, args ...any) {
	l.logger.Error().Fields(args).Msg(format)
}

func (l *hclogZerolog) IsTrace() bool {
	return l.logger.GetLevel() == zerolog.TraceLevel
}

func (l *hclogZerolog) IsDebug() bool {
	return l.logger.GetLevel() == zerolog.DebugLevel
}

func (l *hclogZerolog) IsInfo() bool {
	return l.logger.GetLevel() == zerolog.InfoLevel
}

func (l *hclogZerolog) IsWarn() bool {
	return l.logger.GetLevel() == zerolog.WarnLevel
}

func (l *hclogZerolog) IsError() bool {
	return l.logger.GetLevel() == zerolog.ErrorLevel
}

func (l *hclogZerolog) ImpliedArgs() []any {
	return nil
}

func (l *hclogZerolog) With(args ...any) hclog.Logger {
	return &hclogZerolog{l.logger.With().Fields(args).Logger(), l.nameField, l.name}
}

func (l *hclogZerolog) Name() string {
	return l.name
}

func (l *hclogZerolog) Named(name string) hclog.Logger {
	var newName string
	if l.name == "" {
		newName = name
	} else {
		newName = l.name + "." + name
	}

	return &hclogZerolog{l.logger.With().Str(l.nameField, newName).Logger(), l.nameField, newName}
}

func (l *hclogZerolog) ResetNamed(name string) hclog.Logger {
	return &hclogZerolog{l.logger.With().Str(l.nameField, name).Logger(), l.nameField, name}
}

func (l *hclogZerolog) SetLevel(level hclog.Level) {
	switch level {
	case hclog.Trace:
		l.logger = l.logger.Level(zerolog.TraceLevel)
	case hclog.Debug:
		l.logger = l.logger.Level(zerolog.DebugLevel)
	case hclog.Info:
		l.logger = l.logger.Level(zerolog.InfoLevel)
	case hclog.Warn:
		l.logger = l.logger.Level(zerolog.WarnLevel)
	case hclog.Error:
		l.logger = l.logger.Level(zerolog.ErrorLevel)
	case hclog.NoLevel:
		l.logger = l.logger.Level(zerolog.NoLevel)
	default:
		l.logger.Error().Msgf("Unknown log level: %s", level)
	}
}

func (l *hclogZerolog) GetLevel() hclog.Level {
	switch l.logger.GetLevel() {
	case zerolog.TraceLevel:
		return hclog.Trace
	case zerolog.DebugLevel:
		return hclog.Debug
	case zerolog.InfoLevel:
		return hclog.Info
	case zerolog.WarnLevel:
		return hclog.Warn
	case zerolog.ErrorLevel, zerolog.FatalLevel, zerolog.PanicLevel:
		return hclog.Error
	case zerolog.Disabled, zerolog.NoLevel:
		return hclog.NoLevel
	default:
		l.logger.Error().Msgf("Unknown log level: %s", l.logger.GetLevel())
		return hclog.NoLevel
	}
}

func (l *hclogZerolog) StandardLogger(_ *hclog.StandardLoggerOptions) *log.Logger {
	return log.New(l.logger, "", 0)
}

func (l *hclogZerolog) StandardWriter(_ *hclog.StandardLoggerOptions) io.Writer {
	return l.logger
}
