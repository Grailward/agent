package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// deadHandle simulates the stderr of a Windows GUI binary (-H windowsgui) with no
// attached console: every write fails with an "invalid handle" error.
type deadHandle struct{ writes int }

func (d *deadHandle) Write(p []byte) (int, error) {
	d.writes++
	return 0, errors.New("write /dev/stderr: the handle is invalid")
}

// TestSwallowingWriterNeverStarvesDestination pins the fix for the blank Windows
// log. A failing output handle placed FIRST in an io.MultiWriter must not stop
// the fan-out: io.MultiWriter aborts at the first writer that errors, so without
// the swallowing wrapper the destination behind a dead stderr would receive
// nothing. Wrapping stderr makes the guarantee hold regardless of writer order,
// so no future reordering can reintroduce the bug.
func TestSwallowingWriterNeverStarvesDestination(t *testing.T) {
	stderr := &deadHandle{}
	var file bytes.Buffer

	// The dead handle sits ahead of the destination on purpose.
	mw := io.MultiWriter(swallowingWriter{stderr}, &file)

	payload := []byte("Configuration error: token cannot be empty\n")
	n, err := mw.Write(payload)
	if err != nil {
		t.Fatalf("MultiWriter.Write returned error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("short write: got %d, want %d", n, len(payload))
	}
	if got := file.String(); got != string(payload) {
		t.Fatalf("file destination got %q, want %q", got, string(payload))
	}
	if stderr.writes != 1 {
		t.Fatalf("stderr should still be attempted best-effort, got %d writes", stderr.writes)
	}
}
