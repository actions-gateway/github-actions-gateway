package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
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
	_, err := readPayload(dir)
	require.Error(t, err)
}

// TestWrapper_EncodeUTF16LE verifies that encodeUTF16LE matches the C# UnicodeEncoding
// behaviour used by StreamString: each UTF-16 code unit is two little-endian bytes.
func TestWrapper_EncodeUTF16LE(t *testing.T) {
	// ASCII: every character → [char, 0x00]
	assert.Equal(t, []byte{'A', 0x00, 'B', 0x00}, encodeUTF16LE("AB"))

	// BMP non-ASCII: U+00E9 LATIN SMALL LETTER E WITH ACUTE → [0xE9, 0x00]
	assert.Equal(t, []byte{0xE9, 0x00}, encodeUTF16LE("é"))

	// Supplementary plane character U+1F600 (😀) → surrogate pair
	// UTF-16LE: 0xD83D 0xDE00 → [0x3D, 0xD8, 0x00, 0xDE]
	assert.Equal(t, []byte{0x3D, 0xD8, 0x00, 0xDE}, encodeUTF16LE("😀"))

	assert.Empty(t, encodeUTF16LE(""))
}

// TestWrapper_WriteJobMessage verifies the full wire format:
// [4 bytes LE MessageType=1][4 bytes LE byteLen][UTF-16LE body]
func TestWrapper_WriteJobMessage(t *testing.T) {
	payload := []byte(`{"run_id":99}`)
	wantBody := encodeUTF16LE(string(payload))

	var buf bytes.Buffer
	require.NoError(t, writeJobMessage(&buf, payload))

	b := buf.Bytes()
	require.Len(t, b, 8+len(wantBody))

	assert.Equal(t, uint32(msgTypeNewJobRequest), binary.LittleEndian.Uint32(b[:4]),
		"message type must be 1 (NewJobRequest)")
	assert.Equal(t, uint32(len(wantBody)), binary.LittleEndian.Uint32(b[4:8]),
		"byte-length field must be UTF-16LE byte count")
	assert.Equal(t, wantBody, b[8:], "body must be UTF-16LE encoded")
}

// TestWrapper_WriteJobMessage_Empty verifies that an empty payload produces an
// 8-byte header with byteLen=0 and no body bytes.
func TestWrapper_WriteJobMessage_Empty(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeJobMessage(&buf, []byte{}))

	b := buf.Bytes()
	require.Len(t, b, 8, "empty payload must produce exactly the 8-byte header")
	assert.Equal(t, uint32(msgTypeNewJobRequest), binary.LittleEndian.Uint32(b[:4]))
	assert.Equal(t, uint32(0), binary.LittleEndian.Uint32(b[4:8]))
}

// TestWrapper_WriteJobMessage_Large verifies that the byte-length field
// round-trips for payloads larger than a single pipe buffer (65536 bytes).
func TestWrapper_WriteJobMessage_Large(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 65536)
	wantBody := encodeUTF16LE(string(payload))

	var buf bytes.Buffer
	require.NoError(t, writeJobMessage(&buf, payload))

	b := buf.Bytes()
	require.Len(t, b, 8+len(wantBody))
	assert.Equal(t, uint32(len(wantBody)), binary.LittleEndian.Uint32(b[4:8]))
	assert.Equal(t, wantBody, b[8:])
}
