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
	"go.uber.org/goleak"
)

// TestMain runs the package tests under goleak so a goroutine leaked by run()
// — the payload-writer and output-drain goroutines it spawns must both be
// joined before run() returns — fails the suite instead of leaking silently.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// withSystemCABundleCandidates temporarily overrides systemCABundleCandidates
// so tests don't have to depend on whatever real bundle exists on the dev
// machine.
func withSystemCABundleCandidates(t *testing.T, paths []string) {
	t.Helper()
	orig := systemCABundleCandidates
	systemCABundleCandidates = paths
	t.Cleanup(func() { systemCABundleCandidates = orig })
}

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

// TestInstallProxyCATrust_EmptyPathIsNoOp guards the common "no per-tenant
// proxy configured" case: the AGC provisioner leaves PROXY_CA_CERT_PATH empty
// and the wrapper must skip the trust-store install without error and without
// touching the runner home.
func TestInstallProxyCATrust_EmptyPathIsNoOp(t *testing.T) {
	runnerHome := t.TempDir()
	env, err := installProxyCATrust("", runnerHome)
	require.NoError(t, err)
	assert.Nil(t, env)

	entries, err := os.ReadDir(runnerHome)
	require.NoError(t, err)
	assert.Empty(t, entries, "no files must be written when no proxy CA is configured")
}

// TestInstallProxyCATrust_MissingFileIsNoOp covers the race where the env var
// names a path but the Secret was deleted underneath us (or the mount is
// stale). Tolerated as no-op so the wrapper at least lets the runner reach
// GitHub via whatever the base image already trusts.
func TestInstallProxyCATrust_MissingFileIsNoOp(t *testing.T) {
	runnerHome := t.TempDir()
	env, err := installProxyCATrust(filepath.Join(t.TempDir(), "nonexistent.crt"), runnerHome)
	require.NoError(t, err)
	assert.Nil(t, env)
}

// TestInstallProxyCATrust_EmptyFileIsNoOp covers the case where the Secret
// was created but never populated. Treated identically to missing.
func TestInstallProxyCATrust_EmptyFileIsNoOp(t *testing.T) {
	runnerHome := t.TempDir()
	caPath := filepath.Join(t.TempDir(), "tls.crt")
	require.NoError(t, os.WriteFile(caPath, []byte("   \n"), 0o600))

	env, err := installProxyCATrust(caPath, runnerHome)
	require.NoError(t, err)
	assert.Nil(t, env)
}

// TestInstallProxyCATrust_AppendsToSystemBundle verifies the happy path:
// the wrapper concatenates the system trust bundle with the mounted proxy CA,
// writes the combined PEM into the runner home, and returns the SSL_CERT_FILE
// env var pointing at the combined file. Regression guard for Queue item 5h.
func TestInstallProxyCATrust_AppendsToSystemBundle(t *testing.T) {
	stagingDir := t.TempDir()
	systemBundle := filepath.Join(stagingDir, "ca-certificates.crt")
	systemContent := []byte("-----BEGIN CERTIFICATE-----\nFAKE-SYSTEM-CA\n-----END CERTIFICATE-----\n")
	require.NoError(t, os.WriteFile(systemBundle, systemContent, 0o644))
	withSystemCABundleCandidates(t, []string{systemBundle})

	caPath := filepath.Join(stagingDir, "tls.crt")
	caContent := []byte("-----BEGIN CERTIFICATE-----\nFAKE-PROXY-CA\n-----END CERTIFICATE-----\n")
	require.NoError(t, os.WriteFile(caPath, caContent, 0o600))

	runnerHome := t.TempDir()
	env, err := installProxyCATrust(caPath, runnerHome)
	require.NoError(t, err)

	bundlePath := filepath.Join(runnerHome, proxyCABundleFile)
	require.Equal(t, []string{"SSL_CERT_FILE=" + bundlePath}, env)

	got, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	assert.Contains(t, string(got), "FAKE-SYSTEM-CA",
		"combined bundle must preserve the system trust roots")
	assert.Contains(t, string(got), "FAKE-PROXY-CA",
		"combined bundle must include the per-tenant proxy CA")
	// Order matters for some validators that short-circuit on first match;
	// our wrapper writes system bundle first, proxy CA last.
	sysIdx := bytes.Index(got, []byte("FAKE-SYSTEM-CA"))
	proxyIdx := bytes.Index(got, []byte("FAKE-PROXY-CA"))
	assert.True(t, sysIdx < proxyIdx, "system roots must precede the proxy CA")
}

// TestInstallProxyCATrust_WorksWithoutSystemBundle covers minimal base images
// (e.g. distroless variants) that ship no OS trust store. The wrapper writes
// a bundle containing only the proxy CA — sufficient for the proxy handshake
// itself, though the runner won't be able to validate any non-proxied
// endpoints. That trade-off is acceptable because Runner.Worker's only
// network egress in this deployment IS through the proxy.
func TestInstallProxyCATrust_WorksWithoutSystemBundle(t *testing.T) {
	withSystemCABundleCandidates(t, []string{filepath.Join(t.TempDir(), "does-not-exist.crt")})

	caPath := filepath.Join(t.TempDir(), "tls.crt")
	caContent := []byte("-----BEGIN CERTIFICATE-----\nONLY-PROXY-CA\n-----END CERTIFICATE-----\n")
	require.NoError(t, os.WriteFile(caPath, caContent, 0o600))

	runnerHome := t.TempDir()
	env, err := installProxyCATrust(caPath, runnerHome)
	require.NoError(t, err)
	require.Len(t, env, 1)

	got, err := os.ReadFile(filepath.Join(runnerHome, proxyCABundleFile))
	require.NoError(t, err)
	assert.Equal(t, caContent, got,
		"bundle must contain just the proxy CA when no system bundle exists")
}

// TestWrapper_PropagatesProxyTrustEnvToChild verifies that when
// PROXY_CA_CERT_PATH is set, run() builds the combined trust bundle and the
// child process sees SSL_CERT_FILE in its environment. The stub
// Runner.Worker dumps its env to a file so we can assert on it directly.
func TestWrapper_PropagatesProxyTrustEnvToChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worker wrapper targets Linux; shell-stub strategy is POSIX-only")
	}

	staging := t.TempDir()
	withSystemCABundleCandidates(t, []string{filepath.Join(staging, "missing")})
	caPath := filepath.Join(staging, "tls.crt")
	require.NoError(t, os.WriteFile(caPath,
		[]byte("-----BEGIN CERTIFICATE-----\nTEST-PROXY-CA\n-----END CERTIFICATE-----\n"),
		0o600))

	payloadDir := t.TempDir()
	runnerHome := t.TempDir()
	stubDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "env.txt")

	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, payloadFile), []byte(`{}`), 0o600))

	stubPath := filepath.Join(stubDir, workerBin)
	script := fmt.Sprintf(`#!/bin/sh
printenv > %q
exit 0
`, envFile)
	require.NoError(t, os.WriteFile(stubPath, []byte(script), 0o755))

	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PAYLOAD_SECRET_PATH", payloadDir)
	t.Setenv("RUNNER_HOME_DIR", runnerHome)
	t.Setenv("PROXY_CA_CERT_PATH", caPath)

	require.NoError(t, run())

	data, err := os.ReadFile(envFile)
	require.NoError(t, err)

	wantBundle := filepath.Join(runnerHome, proxyCABundleFile)
	assert.Contains(t, string(data), "SSL_CERT_FILE="+wantBundle,
		"child Runner.Worker must see SSL_CERT_FILE pointing at the combined trust bundle")

	got, err := os.ReadFile(wantBundle)
	require.NoError(t, err)
	assert.Contains(t, string(got), "TEST-PROXY-CA",
		"combined bundle must contain the mounted proxy CA")
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
