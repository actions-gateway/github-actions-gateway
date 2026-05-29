package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// encodeFixtureBlob renders files as the base64-encoded JIT config blob format
// produced by GitHub's generate-jitconfig endpoint.
func encodeFixtureBlob(t *testing.T, files map[string]string) string {
	t.Helper()
	enc := make(map[string]string, len(files))
	for k, v := range files {
		enc[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	raw, err := json.Marshal(enc)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestMaterializeJITConfig_WritesAllThreeFiles(t *testing.T) {
	payloadDir := t.TempDir()
	runnerHome := t.TempDir()

	runnerCfg := `{"agentId":1234,"serverUrl":"https://broker"}`
	credsCfg := `{"scheme":"OAuth","data":{"clientId":"abc","authorizationUrl":"https://auth"}}`
	rsaParams := `{"modulus":"AA","exponent":"AQAB","d":"BB","p":"CC","q":"DD","dp":"EE","dq":"FF","inverseQ":"GG"}`

	blob := encodeFixtureBlob(t, map[string]string{
		".runner":                runnerCfg,
		".credentials":           credsCfg,
		".credentials_rsaparams": rsaParams,
	})
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, "jitconfig"), []byte(blob), 0o600))

	require.NoError(t, materializeJITConfig(payloadDir, runnerHome))

	for name, want := range map[string]string{
		".runner":                runnerCfg,
		".credentials":           credsCfg,
		".credentials_rsaparams": rsaParams,
	} {
		got, err := os.ReadFile(filepath.Join(runnerHome, name))
		require.NoError(t, err, "expected %s to exist", name)
		assert.Equal(t, want, string(got), "content of %s must round-trip", name)
		info, err := os.Stat(filepath.Join(runnerHome, name))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
			"credentials files must be 0600 to protect the RSA private key")
	}
}

// TestMaterializeJITConfig_MissingFileIsNoOp covers stub-registrar agents
// whose Secrets carry no jitconfig key. The wrapper must not error so that
// pre-M3 integration tests continue to work.
func TestMaterializeJITConfig_MissingFileIsNoOp(t *testing.T) {
	payloadDir := t.TempDir()
	runnerHome := t.TempDir()

	require.NoError(t, materializeJITConfig(payloadDir, runnerHome))

	entries, err := os.ReadDir(runnerHome)
	require.NoError(t, err)
	assert.Empty(t, entries, "runner home must remain empty when jitconfig is absent")
}

// TestMaterializeJITConfig_EmptyFileIsNoOp covers the case where the AGC wrote
// the key with empty content (e.g. agent created before the field was
// populated). Treated identically to missing.
func TestMaterializeJITConfig_EmptyFileIsNoOp(t *testing.T) {
	payloadDir := t.TempDir()
	runnerHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, "jitconfig"), []byte("   \n"), 0o600))

	require.NoError(t, materializeJITConfig(payloadDir, runnerHome))

	entries, err := os.ReadDir(runnerHome)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestMaterializeJITConfig_RejectsBadBase64(t *testing.T) {
	payloadDir := t.TempDir()
	runnerHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, "jitconfig"), []byte("not-base64!!"), 0o600))

	err := materializeJITConfig(payloadDir, runnerHome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode base64 blob")
}

func TestMaterializeJITConfig_RejectsMalformedJSON(t *testing.T) {
	payloadDir := t.TempDir()
	runnerHome := t.TempDir()
	bad := base64.StdEncoding.EncodeToString([]byte("[not a map]"))
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, "jitconfig"), []byte(bad), 0o600))

	err := materializeJITConfig(payloadDir, runnerHome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse JIT config JSON")
}

// TestMaterializeJITConfig_IgnoresUnknownEntries hardens the wrapper against a
// future or malicious JIT blob that includes arbitrary file names. Only the
// three documented runner-config files are written; other keys are dropped
// without raising an error (the AGC is trusted but defense-in-depth is cheap).
func TestMaterializeJITConfig_IgnoresUnknownEntries(t *testing.T) {
	payloadDir := t.TempDir()
	runnerHome := t.TempDir()

	blob := encodeFixtureBlob(t, map[string]string{
		".runner":                `{"agentId":1}`,
		".credentials":           `{}`,
		".credentials_rsaparams": `{}`,
		"../../etc/passwd":       "evil",
		"unrelated":              "ignored",
	})
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, "jitconfig"), []byte(blob), 0o600))

	require.NoError(t, materializeJITConfig(payloadDir, runnerHome))

	entries, err := os.ReadDir(runnerHome)
	require.NoError(t, err)
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name()] = true
	}
	assert.Equal(t, map[string]bool{
		".runner":                true,
		".credentials":           true,
		".credentials_rsaparams": true,
	}, got)
}

// TestWrapper_InvokesRunnerWorker_WithSpawnclientArgs end-to-end exercises run()
// against a stub Runner.Worker binary and asserts the subprocess receives exactly
// the three positional arguments documented by the .NET runner
// (src/Runner.Worker/Program.cs): "spawnclient", the inherited read FD (3), and
// the inherited write FD (4). Guards against the regression fixed in PR #59,
// where the wrapper passed "--startuptype workerprocess" instead.
func TestWrapper_InvokesRunnerWorker_WithSpawnclientArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worker wrapper targets Linux; shell-stub strategy is POSIX-only")
	}

	payloadDir := t.TempDir()
	runnerHome := t.TempDir()
	stubDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args.txt")

	// A minimal payload is enough — the wrapper writes it to the worker-input
	// pipe via a goroutine, and the kernel pipe buffer absorbs it even though
	// the stub never reads fd 3.
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, payloadFile), []byte(`{}`), 0o600))

	// Stub Runner.Worker: dump argc + argv to argsFile and exit 0. exit 0 is
	// required because run() calls os.Exit on any non-zero ExitError, which
	// would terminate the test process.
	stubPath := filepath.Join(stubDir, workerBin)
	script := fmt.Sprintf(`#!/bin/sh
{
  printf '%%s\n' "$#"
  for a in "$@"; do
    printf '%%s\n' "$a"
  done
} > %q
exit 0
`, argsFile)
	require.NoError(t, os.WriteFile(stubPath, []byte(script), 0o755))

	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PAYLOAD_SECRET_PATH", payloadDir)
	t.Setenv("RUNNER_HOME_DIR", runnerHome)

	require.NoError(t, run())

	data, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := []string{
		"3",
		"spawnclient",
		fmt.Sprintf("%d", workerReadFD),
		fmt.Sprintf("%d", workerWriteFD),
	}
	require.Equal(t, want, got,
		"Runner.Worker must be invoked with exactly [spawnclient, %d, %d]",
		workerReadFD, workerWriteFD)
}
