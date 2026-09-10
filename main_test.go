package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteTerminationMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")
	cause := errors.New("restore failed: psql failed: exit status 3: psql:<stdin>:12: ERROR: syntax error")

	writeTerminationMessage(path, cause)

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the termination message: %v", err)
	}
	if string(written) != cause.Error() {
		t.Errorf("termination message is %q, want %q", written, cause.Error())
	}
}

func TestWriteTerminationMessageTolerantOfUnwritablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-directory", "termination-log")

	writeTerminationMessage(path, errors.New("restore failed"))

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to stay absent, stat returned %v", path, err)
	}
}
