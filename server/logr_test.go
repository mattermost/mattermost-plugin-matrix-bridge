package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateTransactionLogger_NoFilespec(t *testing.T) {
	t.Setenv("MM_MATRIX_LOG_FILESPEC", "")

	logger, err := CreateTransactionLogger()

	require.NoError(t, err)
	assert.NotNil(t, logger.Logr())
}

func TestCreateTransactionLogger_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "sub", "matrix.log")
	t.Setenv("MM_MATRIX_LOG_FILESPEC", logFile)

	logger, err := CreateTransactionLogger()

	require.NoError(t, err)
	assert.NotNil(t, logger.Logr())
	_, statErr := os.Stat(filepath.Join(dir, "sub"))
	assert.NoError(t, statErr, "subdirectory should have been created")
}

func TestCreateTransactionLogger_RelativePath(t *testing.T) {
	dir := t.TempDir()
	prev, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(prev) })

	t.Setenv("MM_MATRIX_LOG_FILESPEC", "logs/matrix.log")

	logger, err := CreateTransactionLogger()

	require.NoError(t, err)
	assert.NotNil(t, logger.Logr())
	_, statErr := os.Stat(filepath.Join(dir, "logs"))
	assert.NoError(t, statErr, "relative subdirectory should have been created")
}

func TestCreateTransactionLogger_BareFilename(t *testing.T) {
	// filespec with no directory component — filepath.Dir returns ".", so no mkdir needed
	dir := t.TempDir()
	prev, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(prev) })

	t.Setenv("MM_MATRIX_LOG_FILESPEC", "matrix.log")

	logger, err := CreateTransactionLogger()

	require.NoError(t, err)
	assert.NotNil(t, logger.Logr())
}

func TestCreateTransactionLogger_RelativeTraversalRejected(t *testing.T) {
	// A relative filespec whose dir component contains ".." must be rejected.
	// filepath.Clean does not resolve ".." for relative paths against the filesystem,
	// so "../outside/evil.log" stays as-is and relDir becomes "../outside".
	// os.Root.MkdirAll rejects paths with ".." components at the OS level.
	dir := t.TempDir()
	prev, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(prev) })

	outside := filepath.Join(filepath.Dir(dir), "outside")
	t.Setenv("MM_MATRIX_LOG_FILESPEC", "../outside/evil.log")

	_, createErr := CreateTransactionLogger()

	assert.Error(t, createErr, "os.Root.MkdirAll should reject '..' traversal")
	_, statErr := os.Stat(outside)
	assert.True(t, os.IsNotExist(statErr), "traversal must not create directories outside the working directory")
}
