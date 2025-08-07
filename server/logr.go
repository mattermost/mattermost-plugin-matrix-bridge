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

	// Extract path from filespec
	filepath := filepath.Dir(filespec)
	if filepath != "" && filepath != "." {
		// Ensure log directory exists
		if err := os.MkdirAll(filepath, 0755); err != nil {
			return logr.Logger{}, err
		}
	}

	// Configure JSON formatter for structured logging
	jsonFormatter := &formatters.JSON{
		EnableCaller: true,
	}

	// Create file target with rotation options
	fileOptions := targets.FileOptions{
		Filename:   filespec,
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
		return logr.Logger{}, err
	}

	// Return a Logger instance from the Logr
	return logger.NewLogger(), nil
}
