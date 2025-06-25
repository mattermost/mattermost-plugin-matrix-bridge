package main

import "github.com/mattermost/mattermost/server/public/plugin"

// Logger interface for logging operations
type Logger interface {
	LogDebug(message string, keyValuePairs ...any)
	LogInfo(message string, keyValuePairs ...any)
	LogWarn(message string, keyValuePairs ...any)
	LogError(message string, keyValuePairs ...any)
}

// PluginAPILogger adapts the plugin.API to implement the Logger interface
type PluginAPILogger struct {
	api plugin.API
}

// NewPluginAPILogger creates a new PluginAPILogger
func NewPluginAPILogger(api plugin.API) Logger {
	return &PluginAPILogger{api: api}
}

// LogDebug logs a debug message
func (l *PluginAPILogger) LogDebug(message string, keyValuePairs ...any) {
	l.api.LogDebug(message, keyValuePairs...)
}

// LogInfo logs an info message
func (l *PluginAPILogger) LogInfo(message string, keyValuePairs ...any) {
	l.api.LogInfo(message, keyValuePairs...)
}

// LogWarn logs a warning message
func (l *PluginAPILogger) LogWarn(message string, keyValuePairs ...any) {
	l.api.LogWarn(message, keyValuePairs...)
}

// LogError logs an error message
func (l *PluginAPILogger) LogError(message string, keyValuePairs ...any) {
	l.api.LogError(message, keyValuePairs...)
}
