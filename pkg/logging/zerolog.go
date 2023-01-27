package logging

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/journald"
	"github.com/rs/zerolog/log"
)

type Logger = zerolog.Logger

// Different types of logging
const (
	LogConsolePretty string = "consolepretty"
	LogJSON          string = "json"
	LogDiscard       string = "discard"
	LogJournald      string = "journald"
)

var LogFormats = []string{LogJSON, LogConsolePretty, LogJournald, LogDiscard}
var LogFormatsCommandLine = []string{LogJSON, LogConsolePretty, LogDiscard}

var LogLevels = []string{"trace", "debug", "info", "warn", "error", "fatal", "panic"}

// init sets the time zone to UTC.
func init() {
	zerolog.TimestampFunc = func() time.Time {
		return time.Now().UTC()
	}
}

func isValidLogFormat(logFormat string) bool {
	for _, lf := range LogFormats {
		if lf == logFormat {
			return true
		}
	}
	return false
}

// InitZerolog initializes the global zerolog logger.
//
// level and logLevel determine where the logs go and what format is used.
func InitZerolog(level string, logFormat string) (*Logger, error) {

	if !isValidLogFormat(logFormat) {
		msg := fmt.Sprintf("Unknown log format: %s", logFormat)
		err := errors.New(msg)
		return nil, err
	}

	switch logFormat {
	case LogJSON:
		log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
	case LogConsolePretty:
		log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).
			With().Timestamp().Logger()
	case LogJournald:
		log.Logger = zerolog.New(journald.NewJournalDWriter())
	case LogDiscard:
		log.Logger = zerolog.New(io.Discard)
	default:
		return nil, fmt.Errorf("logFormat %q not known", logFormat)
	}

	err := SetLogLevel(level)
	if err != nil {
		return nil, err
	}

	return &log.Logger, nil
}

// GetGlobalLogger returns the global logger instance.
func GetGlobalLogger() *Logger {
	return &log.Logger
}

// GetLogLevel returns the current global log level.
func GetLogLevel() string {
	return zerolog.GlobalLevel().String()
}

// SetLogLevel sets the global log level.
func SetLogLevel(level string) error {
	logLevel, err := zerolog.ParseLevel(level)
	if err != nil {
		return fmt.Errorf("could not parse log level %q", level)
	}
	zerolog.SetGlobalLevel(logLevel)
	return nil
}

// SetLogFile creates a new global logger that writes to a file.
func SetLogFile(filename string) (*os.File, error) {
	file, err := os.OpenFile(
		filename,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY,
		0664,
	)
	if err != nil {
		return nil, err
	}
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: file, NoColor: true, TimeFormat: time.RFC3339}).
		With().Timestamp().Logger()

	return file, nil
}

// ZerologMiddleware logs access and converts panic to stack traces.
func ZerologMiddleware(logger *zerolog.Logger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			startTime := time.Now()

			defer func() {
				endTime := time.Now()
				errorLog := SubLoggerWithRequestIDAndTopic(r, "error")

				// Recover and record stack traces in case of a panic
				if rec := recover(); rec != nil {
					errorLog.Panic().
						Timestamp().
						Interface("recover_info", rec).
						Bytes("debug_stack", debug.Stack()).
						Msg("Runtime error (panic)")
					http.Error(ww, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}

				accessLog := SubLoggerWithRequestIDAndTopic(r, "access")
				accessLog.Info().
					Timestamp().
					Fields(map[string]interface{}{
						"remote_ip":  r.RemoteAddr,
						"url":        r.URL.Path,
						"proto":      r.Proto,
						"method":     r.Method,
						"user_agent": r.Header.Get("User-Agent"),
						"status":     ww.Status(),
						"latency_ms": float64(endTime.Sub(startTime).Nanoseconds()) / 1000000.0,
						"bytes_in":   r.Header.Get("Content-Length"),
						"bytes_out":  ww.BytesWritten(),
					}).
					Msg("Incoming request")
			}()
			next.ServeHTTP(ww, r)
		}
		return http.HandlerFunc(fn)
	}
}

// GetRequestID returns the request ID.
func GetRequestID(r *http.Request) string {
	key := middleware.RequestIDKey
	requestID, ok := r.Context().Value(key).(string)
	if !ok {
		requestID = "-"
	}
	return requestID
}

// SubLoggerWithRequestID creates a new sub-logger with request_id field.
func SubLoggerWithRequestID(r *http.Request) *zerolog.Logger {
	logger := log.Logger.With().
		Str("request_id", GetRequestID(r)).
		Logger()
	return &logger
}

// SubLoggerWithRequestIDAndTopic creates a new sub-logger with request_id and topic fields.
func SubLoggerWithRequestIDAndTopic(r *http.Request, topic string) *zerolog.Logger {
	logger := log.Logger.With().
		Str("request_id", GetRequestID(r)).
		Str("topic", topic).
		Logger()
	return &logger
}

// SubLoggerWithTopic creates sub-logger with topic field.
func SubLoggerWithTopic(lg *zerolog.Logger, topic string) *zerolog.Logger {
	logger := lg.With().Str("topic", topic).Logger()
	return &logger
}

// LoggerWithTopic creates a top-level logger with topic field.
func LoggerWithTopic(topic string) *zerolog.Logger {
	logger := log.Logger.With().
		Str("topic", topic).
		Logger()
	return &logger
}

// SubLogger create a new sub-logger with a specific key, value field.
func SubLoggerWithString(key string, val string) *zerolog.Logger {
	logger := log.Logger.With().
		Str(key, val).
		Logger()
	return &logger
}

// SubLogger creates a new sub-logger with a specific log level.
func SubLoggerWithSpecificLevel(lg *zerolog.Logger, level string) *zerolog.Logger {
	logLevel, err := zerolog.ParseLevel(level)
	if err != nil {
		logLevel = zerolog.TraceLevel
	}

	logger := lg.Level(logLevel)
	return &logger
}