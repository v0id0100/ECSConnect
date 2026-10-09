package logger

import (
	"log/slog"
	"os"
	"path/filepath"
)

var l *slog.Logger
var logPath string

// Init initialises the file logger at ~/.ecsconnect/ecsconnect.log.
// Silently falls back to a no-op logger if the file cannot be opened.
func Init() {
	home, err := os.UserHomeDir()
	if err != nil {
		l = slog.New(slog.NewTextHandler(os.Stderr, nil))
		return
	}

	dir := filepath.Join(home, ".ecsconnect")
	_ = os.MkdirAll(dir, 0o755)
	logPath = filepath.Join(dir, "ecsconnect.log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		l = slog.New(slog.NewTextHandler(os.Stderr, nil))
		return
	}

	l = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	l.Info("ECSConnect started")
}

// Path returns the path to the log file.
func Path() string { return logPath }

func Info(msg string, args ...any)  { l.Info(msg, args...) }
func Debug(msg string, args ...any) { l.Debug(msg, args...) }
func Warn(msg string, args ...any)  { l.Warn(msg, args...) }
func Error(msg string, args ...any) { l.Error(msg, args...) }
