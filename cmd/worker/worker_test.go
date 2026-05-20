package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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

	// L1: use a deadline so a blocked FIFO open fails the test clearly instead
	// of hanging indefinitely in CI.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

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
	select {
	case err := <-readDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for FIFO reader to finish")
	}

	b := buf.Bytes()
	require.Len(t, b, 4+len(payload), "expected 4-byte length prefix + payload")

	gotLen := binary.BigEndian.Uint32(b[:4])
	assert.Equal(t, uint32(len(payload)), gotLen, "length prefix must equal payload length")
	assert.Equal(t, payload, b[4:], "payload bytes must follow the length prefix")
}

// TestWrapper_EmptyPayload verifies that writePayloadToPipe sends a [0,0,0,0]
// wire message (4-byte prefix encoding 0) when the payload is empty.
func TestWrapper_EmptyPayload(t *testing.T) {
	dir := t.TempDir()
	pipePath := filepath.Join(dir, "job-in-empty")
	require.NoError(t, syscall.Mkfifo(pipePath, 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

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

	require.NoError(t, writePayloadToPipe(pipePath, []byte{}))
	select {
	case err := <-readDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for FIFO reader to finish")
	}

	// M2: empty payload → exactly 4 bytes encoding value 0.
	b := buf.Bytes()
	require.Len(t, b, 4, "empty payload must produce exactly the 4-byte length prefix")
	assert.Equal(t, uint32(0), binary.BigEndian.Uint32(b[:4]), "length prefix must be 0 for empty payload")
}

// TestWrapper_LargePayload verifies that the length prefix round-trips correctly
// for a payload larger than a single TCP segment (65536 bytes).
func TestWrapper_LargePayload(t *testing.T) {
	dir := t.TempDir()
	pipePath := filepath.Join(dir, "job-in-large")
	require.NoError(t, syscall.Mkfifo(pipePath, 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload := bytes.Repeat([]byte("x"), 65536)

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
	select {
	case err := <-readDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for FIFO reader to finish")
	}

	// M2: length prefix must round-trip correctly via binary.BigEndian.
	b := buf.Bytes()
	require.Len(t, b, 4+len(payload), "expected 4-byte length prefix + 65536 payload bytes")
	gotLen := binary.BigEndian.Uint32(b[:4])
	assert.Equal(t, uint32(len(payload)), gotLen, "length prefix must equal payload length for large payload")
	assert.Equal(t, payload, b[4:], "payload bytes must be intact after large write")
}
