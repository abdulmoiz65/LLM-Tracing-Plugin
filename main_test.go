package main

import (
	"fmt"
	"os"
	"testing"
)

// TestMain runs the tests inside a temporary working directory: tests that
// drive the process-wide plugin instance save history to the default
// data_dir (relative to the working directory), which must not land in the
// repository.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "llm-tracing-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = os.Chdir(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	plug.shutdown() // close the history file before removing the folder
	_ = os.Chdir(os.TempDir())
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
