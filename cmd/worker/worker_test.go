package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapper_ReadPayloadFromMount(t *testing.T) {
	dir := t.TempDir()
	want := []byte(`{"run_id":42,"variables":{}}`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "payload"), want, 0o600))

	got, err := readPayload(dir)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestWrapper_MissingPayload(t *testing.T) {
	dir := t.TempDir()
	// No payload file written.
	_, err := readPayload(dir)
	require.Error(t, err)
}

// TestWrapper_WritesToNamedPipes verifies that writePayloadToPipe sends a
// 4-byte big-endian length prefix followed by the raw payload bytes.
func TestWrapper_WritesToNamedPipes(t *testing.T) {
	dir := t.TempDir()
	pipePath := filepath.Join(dir, "job-in")
	require.NoError(t, syscall.Mkfifo(pipePath, 0o600))

	payload := []byte(`{"run_id":99}`)

	// Read end must be opened concurrently — open(RDONLY) on a FIFO blocks
	// until a writer opens the write end.
	var buf bytes.Buffer
	readDone := make(chan error, 1)
	go func() {
		f, err := os.Open(pipePath)
		if err != nil {
			readDone <- err
			return
		}
		defer f.Close()
		_, err = io.Copy(&buf, f)
		readDone <- err
	}()

	require.NoError(t, writePayloadToPipe(pipePath, payload))
	require.NoError(t, <-readDone)

	b := buf.Bytes()
	require.Len(t, b, 4+len(payload), "expected 4-byte length prefix + payload")

	gotLen := binary.BigEndian.Uint32(b[:4])
	assert.Equal(t, uint32(len(payload)), gotLen, "length prefix must equal payload length")
	assert.Equal(t, payload, b[4:], "payload bytes must follow the length prefix")
}
