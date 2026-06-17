package main

import (
	"os"
	"path/filepath"

	"github.com/mattermost/logr/v2"
	"github.com/mattermost/logr/v2/formatters"
	"github.com/mattermost/logr/v2/targets"
)

// CreateTransactionLogger creates and configures a Logr instance for Matrix transaction debugging.
// It creates a dedicated JSON file logger for Matrix events and transactions.
func CreateTransactionLogger() (logr.Logger, error) {
	// Create logger instance with options
	logger, err := logr.New(
		logr.MaxQueueSize(1000),
	)
	if err != nil {
		return logr.Logger{}, err
	}

	filespec := os.Getenv("MM_MATRIX_LOG_FILESPEC")
	if filespec == "" {
		return logger.NewLogger(), nil
	}

	cleanedFilespec := filepath.Clean(filespec)
	logDir := filepath.Dir(cleanedFilespec)

	if logDir != "." {
		// Use os.OpenRoot so Root.MkdirAll rejects any ".." components at the
		// OS level. Absolute paths are anchored to the filesystem root; relative
		// paths are anchored to the current working directory.
		var fsRootPath, relDir string
		if filepath.IsAbs(logDir) {
			fsRootPath = filepath.VolumeName(logDir) + string(filepath.Separator)
			relDir = logDir[len(fsRootPath):]
		} else {
			fsRootPath = "."
			relDir = logDir
		}

		if relDir != "" {
			fsRoot, openErr := os.OpenRoot(fsRootPath)
			if openErr != nil {
				_ = logger.Shutdown()
				return logr.Logger{}, openErr
			}
			mkdirErr := fsRoot.MkdirAll(relDir, 0o750)
			_ = fsRoot.Close()
			if mkdirErr != nil {
				_ = logger.Shutdown()
				return logr.Logger{}, mkdirErr
			}
		}
	}

	// Configure JSON formatter for structured logging
	jsonFormatter := &formatters.JSON{
		EnableCaller: true,
	}

	// Create file target with rotation options
	fileOptions := targets.FileOptions{
		Filename:   cleanedFilespec,
		MaxSize:    100, // 100MB
		MaxBackups: 5,
		MaxAge:     5, // 5 days
		Compress:   true,
	}
	fileTarget := targets.NewFileTarget(fileOptions)

	// Create a custom filter for debug level and above
	filter := logr.NewCustomFilter(logr.Debug, logr.Info, logr.Warn, logr.Error, logr.Fatal, logr.Panic)

	// Add target with JSON formatter
	if err := logger.AddTarget(fileTarget, "matrix-transactions", filter, jsonFormatter, 100); err != nil {
		_ = logger.Shutdown()
		return logr.Logger{}, err
	}

	// Return a Logger instance from the Logr
	return logger.NewLogger(), nil
}
