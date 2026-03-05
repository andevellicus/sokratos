package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"sokratos/timefmt"
)

var (
	Log     *zap.SugaredLogger
	logFile *os.File
)

// Init creates the log directory, opens a timestamped log file, and configures
// a zap logger that writes to both stdout and the log file.
func Init(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	filename := time.Now().Format(timefmt.LogFile) + ".log"
	path := filepath.Join(dir, filename)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	logFile = f

	encoderCfg := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		MessageKey:     "msg",
		CallerKey:      "caller",
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeLevel:    zapcore.CapitalColorLevelEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
	}

	// File encoder without color codes.
	fileEncoderCfg := encoderCfg
	fileEncoderCfg.EncodeLevel = zapcore.CapitalLevelEncoder

	consoleCore := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderCfg),
		zapcore.AddSync(os.Stdout),
		zapcore.DebugLevel,
	)

	fileCore := zapcore.NewCore(
		zapcore.NewConsoleEncoder(fileEncoderCfg),
		zapcore.AddSync(f),
		zapcore.DebugLevel,
	)

	core := zapcore.NewTee(consoleCore, fileCore)
	Log = zap.New(core, zap.AddCaller()).Sugar()

	return nil
}

// Close flushes the logger and closes the log file.
func Close() {
	if Log != nil {
		_ = Log.Sync()
	}
	if logFile != nil {
		logFile.Close()
	}
}
