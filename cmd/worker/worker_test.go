package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestMain runs the package tests under goleak so a goroutine leaked by run()
// — the payload-writer and output-drain goroutines it spawns must both be
// joined before run() returns — fails the suite instead of leaking silently.
//
// It also clears WORKER_MODE for the whole package so the ambient environment
// can never change a test's outcome (Q269). The AGC provisioner sets
// WORKER_MODE=scaleset on ScaleSet-protocol runner pods; when GAG's own CI runs
// on such a pod the job's `go test` process inherits it, which would flip every
// run()-based classic-path test into the scale-set branch and fail its
// assertions. The scale-set tests exercise runScaleSet directly rather than via
// run(), so none of them relies on an ambient WORKER_MODE; a test that intends a
// specific mode sets it with t.Setenv itself.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(workerModeEnv)
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

// writeUnreadable writes content to path, strips its mode bits, and restores
// them so t.TempDir cleanup can remove the file. It reads the file back and
// skips when that read succeeds. uid is a proxy for the capability and is
// wrong both ways: a uid-0 process without CAP_DAC_OVERRIDE cannot read mode
// 000, and a non-root process holding it can. The skip names the uid observed,
// so a skipped run is distinguishable from a passing one.
func writeUnreadable(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if _, err := os.ReadFile(path); err == nil {
		t.Skipf("uid %d reads a mode-000 file, so %s cannot be made unreadable", os.Geteuid(), path)
	}
}

func TestReadJITBlob_Valid(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, jitConfigFile), []byte("  base64blob==\n"), 0o600))
	blob, err := readJITBlob(dir)
	require.NoError(t, err)
	assert.Equal(t, "base64blob==", blob, "the blob is trimmed of surrounding whitespace")
}

func TestReadJITBlob_MissingIsError(t *testing.T) {
	_, err := readJITBlob(t.TempDir())
	require.Error(t, err, "a scale-set worker cannot register without a JIT config")
}

func TestReadJITBlob_EmptyIsError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, jitConfigFile), []byte("   \n"), 0o600))
	_, err := readJITBlob(dir)
	require.Error(t, err)
}

// TestRunScaleSet_ExecsRunShWithJITConfig verifies the scale-set worker mode execs
// the runner image's run.sh with --jitconfig and the staged blob, from the runner
// home directory — the probed interface (§2b-4). A fake run.sh records its argv.
func TestRunScaleSet_ExecsRunShWithJITConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("run.sh exec test is POSIX-only")
	}
	runnerHome := t.TempDir()
	payloadDir := t.TempDir()

	// A fake run.sh that records its arguments (one per line) into args.txt in cwd.
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > args.txt\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(runnerHome, runnerRunScript), []byte(script), 0o700)) //nolint:gosec // test fixture must be executable

	const blob = "eyJydW5uZXIiOnt9fQ==" // arbitrary base64 blob
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, jitConfigFile), []byte(blob), 0o600))

	// No PROXY_CA_CERT_PATH set → proxy-CA trust install is a tolerated no-op.
	t.Setenv("PROXY_CA_CERT_PATH", "")

	require.NoError(t, runScaleSet(payloadDir, runnerHome))

	got, err := os.ReadFile(filepath.Join(runnerHome, "args.txt"))
	require.NoError(t, err, "run.sh must have executed in the runner home directory")
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	require.Equal(t, []string{jitConfigFlag, blob}, lines,
		"run.sh must receive exactly --jitconfig <blob>")
}

// TestRunScaleSet_MissingBlobFailsFast verifies runScaleSet errors before exec when no
// JIT blob is staged (rather than launching a runner that cannot register).
func TestRunScaleSet_MissingBlobFailsFast(t *testing.T) {
	runnerHome := t.TempDir()
	require.Error(t, runScaleSet(t.TempDir(), runnerHome))
}

// TestRunScaleSet_InstallsProxyCATrust verifies the wrapper's one retained duty in
// scale-set mode: it installs the per-tenant egress-proxy CA trust and passes
// SSL_CERT_FILE into the run.sh environment so the runner's own TLS to GitHub through
// the proxy is trusted.
func TestRunScaleSet_InstallsProxyCATrust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("run.sh exec test is POSIX-only")
	}
	staging := t.TempDir()
	systemBundle := filepath.Join(staging, "ca-certificates.crt")
	require.NoError(t, os.WriteFile(systemBundle, []byte("-----BEGIN CERTIFICATE-----\nSYS\n-----END CERTIFICATE-----\n"), 0o644)) //nolint:gosec // G306: test fixture public CA bundle
	withSystemCABundleCandidates(t, []string{systemBundle})

	caPath := filepath.Join(staging, "tls.crt")
	require.NoError(t, os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----\nPROXY\n-----END CERTIFICATE-----\n"), 0o600))
	t.Setenv("PROXY_CA_CERT_PATH", caPath)

	runnerHome := t.TempDir()
	payloadDir := t.TempDir()
	// A fake run.sh that records its SSL_CERT_FILE env into env.txt in cwd.
	script := "#!/bin/sh\nprintf 'SSL_CERT_FILE=%s\\n' \"$SSL_CERT_FILE\" > env.txt\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(runnerHome, runnerRunScript), []byte(script), 0o700)) //nolint:gosec // test fixture must be executable
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, jitConfigFile), []byte("blob=="), 0o600))

	require.NoError(t, runScaleSet(payloadDir, runnerHome))

	got, err := os.ReadFile(filepath.Join(runnerHome, "env.txt"))
	require.NoError(t, err)
	want := "SSL_CERT_FILE=" + filepath.Join(runnerHome, caBundleFile)
	assert.Equal(t, want, strings.TrimSpace(string(got)),
		"run.sh must inherit the combined proxy-CA bundle via SSL_CERT_FILE")
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
	credsCfg := `{"scheme":"OAuth","data":{"clientId":"abc","authorizationUrl":"https://auth"}}` //nolint:gosec // G101: synthetic test fixture, not a real credential
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

// TestInstallCATrust_EmptyPathIsNoOp guards the common "no per-tenant
// proxy configured" case: the AGC provisioner leaves PROXY_CA_CERT_PATH empty
// and the wrapper must skip the trust-store install without error and without
// touching the runner home.
func TestInstallCATrust_EmptyPathIsNoOp(t *testing.T) {
	runnerHome := t.TempDir()
	env, err := installCATrust(runnerHome, "")
	require.NoError(t, err)
	assert.Nil(t, env)

	entries, err := os.ReadDir(runnerHome)
	require.NoError(t, err)
	assert.Empty(t, entries, "no files must be written when no proxy CA is configured")
}

// TestInstallCATrust_MissingFileIsNoOp covers the race where the env var
// names a path but the Secret was deleted underneath us (or the mount is
// stale). Tolerated as no-op so the wrapper at least lets the runner reach
// GitHub via whatever the base image already trusts.
func TestInstallCATrust_MissingFileIsNoOp(t *testing.T) {
	runnerHome := t.TempDir()
	env, err := installCATrust(runnerHome, filepath.Join(t.TempDir(), "nonexistent.crt"))
	require.NoError(t, err)
	assert.Nil(t, env)
}

// TestInstallCATrust_EmptyFileIsNoOp covers the case where the Secret
// was created but never populated. Treated identically to missing.
func TestInstallCATrust_EmptyFileIsNoOp(t *testing.T) {
	runnerHome := t.TempDir()
	caPath := filepath.Join(t.TempDir(), "tls.crt")
	require.NoError(t, os.WriteFile(caPath, []byte("   \n"), 0o600))

	env, err := installCATrust(runnerHome, caPath)
	require.NoError(t, err)
	assert.Nil(t, env)
}

// TestInstallCATrust_AppendsToSystemBundle verifies the happy path:
// the wrapper concatenates the system trust bundle with the mounted proxy CA,
// writes the combined PEM into the runner home, and returns the SSL_CERT_FILE
// env var pointing at the combined file. Regression guard for Queue item 5h.
func TestInstallCATrust_AppendsToSystemBundle(t *testing.T) {
	stagingDir := t.TempDir()
	systemBundle := filepath.Join(stagingDir, "ca-certificates.crt")
	systemContent := []byte("-----BEGIN CERTIFICATE-----\nFAKE-SYSTEM-CA\n-----END CERTIFICATE-----\n")
	require.NoError(t, os.WriteFile(systemBundle, systemContent, 0o644)) //nolint:gosec // G306: test fixture writing a fake public CA bundle
	withSystemCABundleCandidates(t, []string{systemBundle})

	caPath := filepath.Join(stagingDir, "tls.crt")
	caContent := []byte("-----BEGIN CERTIFICATE-----\nFAKE-PROXY-CA\n-----END CERTIFICATE-----\n")
	require.NoError(t, os.WriteFile(caPath, caContent, 0o600))

	runnerHome := t.TempDir()
	env, err := installCATrust(runnerHome, caPath)
	require.NoError(t, err)

	bundlePath := filepath.Join(runnerHome, caBundleFile)
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

// TestInstallCATrust_WorksWithoutSystemBundle covers minimal base images
// (e.g. distroless variants) that ship no OS trust store. The wrapper writes
// a bundle containing only the proxy CA — sufficient for the proxy handshake
// itself, though the runner won't be able to validate any non-proxied
// endpoints. That trade-off is acceptable because Runner.Worker's only
// network egress in this deployment IS through the proxy.
func TestInstallCATrust_WorksWithoutSystemBundle(t *testing.T) {
	withSystemCABundleCandidates(t, []string{filepath.Join(t.TempDir(), "does-not-exist.crt")})

	caPath := filepath.Join(t.TempDir(), "tls.crt")
	caContent := []byte("-----BEGIN CERTIFICATE-----\nONLY-PROXY-CA\n-----END CERTIFICATE-----\n")
	require.NoError(t, os.WriteFile(caPath, caContent, 0o600))

	runnerHome := t.TempDir()
	env, err := installCATrust(runnerHome, caPath)
	require.NoError(t, err)
	require.Len(t, env, 1)

	got, err := os.ReadFile(filepath.Join(runnerHome, caBundleFile))
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
	require.NoError(t, os.WriteFile(stubPath, []byte(script), 0o755)) //nolint:gosec // G306: test writes an executable stub script onto PATH

	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PAYLOAD_SECRET_PATH", payloadDir)
	t.Setenv("RUNNER_HOME_DIR", runnerHome)
	t.Setenv("PROXY_CA_CERT_PATH", caPath)

	require.NoError(t, run())

	data, err := os.ReadFile(envFile)
	require.NoError(t, err)

	wantBundle := filepath.Join(runnerHome, caBundleFile)
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
	require.NoError(t, os.WriteFile(stubPath, []byte(script), 0o755)) //nolint:gosec // G306: test writes an executable stub script onto PATH

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

func TestInstallSelf_CopiesExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := installSelf(dir); err != nil {
		t.Fatalf("installSelf: %v", err)
	}
	dst := filepath.Join(dir, "wrapper")
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat installed wrapper: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("installed wrapper is empty")
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed wrapper is not executable: mode %v", fi.Mode())
	}
}

func TestResolveWorkerBin_FoundInRunnerHome(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o750); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(bin, "Runner.Worker")
	if err := os.WriteFile(want, []byte("x"), 0o600); err != nil { // only Stat'd by resolveWorkerBin
		t.Fatal(err)
	}
	got, err := resolveWorkerBin(home)
	if err != nil {
		t.Fatalf("resolveWorkerBin: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveWorkerBin_FallbackToPath(t *testing.T) {
	pathDir := t.TempDir()
	onPath := filepath.Join(pathDir, "Runner.Worker")
	// Must be executable: exec.LookPath only resolves files with an exec bit.
	if err := os.WriteFile(onPath, []byte("x"), 0o700); err != nil { //nolint:gosec // G306: a PATH fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	// runnerHome has no bin/Runner.Worker → must fall back to PATH.
	got, err := resolveWorkerBin(t.TempDir())
	if err != nil {
		t.Fatalf("resolveWorkerBin: %v", err)
	}
	if got != onPath {
		t.Fatalf("got %q, want %q", got, onPath)
	}
}

func TestResolveWorkerBin_NotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir → Runner.Worker not on PATH
	if _, err := resolveWorkerBin(t.TempDir()); err == nil {
		t.Fatal("expected error when Runner.Worker is absent from RUNNER_HOME_DIR/bin and PATH")
	}
}

func TestInstallSelf_BadDir(t *testing.T) {
	// A path whose parent is a regular file → MkdirAll fails.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installSelf(filepath.Join(f, "sub")); err == nil {
		t.Fatal("expected error installing under a file path")
	}
}

func TestEnvOr(t *testing.T) {
	if got := envOr("WK_UNSET_VAR_XYZ", "fallback"); got != "fallback" {
		t.Fatalf("unset: got %q, want fallback", got)
	}
	t.Setenv("WK_SET_VAR_XYZ", "value")
	if got := envOr("WK_SET_VAR_XYZ", "fallback"); got != "value" {
		t.Fatalf("set: got %q, want value", got)
	}
}

func TestLogLevelFromEnv(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")
	if got := logLevelFromEnv(); got != slog.LevelDebug {
		t.Fatalf("debug: got %v, want %v", got, slog.LevelDebug)
	}
	t.Setenv("LOG_LEVEL", "info")
	if got := logLevelFromEnv(); got != slog.LevelInfo {
		t.Fatalf("info: got %v, want %v", got, slog.LevelInfo)
	}
	t.Setenv("LOG_LEVEL", "")
	if got := logLevelFromEnv(); got != slog.LevelInfo {
		t.Fatalf("default: got %v, want %v", got, slog.LevelInfo)
	}
}

// TestInstallSelf_DestinationIsDirectory covers the os.OpenFile branch of
// installSelf: dir is creatable (MkdirAll succeeds because dir already
// exists), but the "wrapper" destination path is itself a directory, so
// OpenFile(O_CREATE|O_WRONLY|O_TRUNC) fails with EISDIR.
func TestInstallSelf_DestinationIsDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "wrapper"), 0o750))

	err := installSelf(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create")
}

// TestInstallSelf_HappyPath_CopiesRunningBinaryExecutable verifies the full
// success path into a fresh temp dir: the copied "wrapper" file exists,
// is non-empty, and carries the 0o755 executable mode installSelf requests
// regardless of the source binary's own mode.
func TestInstallSelf_HappyPath_CopiesRunningBinaryExecutable(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, installSelf(dir))

	dst := filepath.Join(dir, "wrapper")
	fi, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Positive(t, fi.Size(), "copied wrapper must not be empty")
	assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm(),
		"installed wrapper must be mode 0o755 regardless of the running test binary's own mode")
}

// TestResolveWorkerBin_NeitherRunnerHomeNorPathHasIt is a second flavor of the
// not-found case: runnerHome/bin does not even exist (as opposed to existing
// but lacking the binary), and PATH is empty. Both lookup strategies must
// fail and the error must name both places searched.
func TestResolveWorkerBin_NeitherRunnerHomeNorPathHasIt(t *testing.T) {
	t.Setenv("PATH", "")
	runnerHome := t.TempDir() // no bin/ subdirectory at all
	_, err := resolveWorkerBin(runnerHome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Runner.Worker")
	assert.Contains(t, err.Error(), runnerHome)
}

// TestMaterializeJITConfig_ReadErrorOtherThanNotExist covers the case where a jitconfig file that exists but can't be read for a reason
// other than absence (here, permission denied) must surface as a wrapped
// error, not be silently skipped like the missing-file case.
func TestMaterializeJITConfig_ReadErrorOtherThanNotExist(t *testing.T) {
	payloadDir := t.TempDir()
	runnerHome := t.TempDir()
	writeUnreadable(t, filepath.Join(payloadDir, jitConfigFile), "irrelevant")

	err := materializeJITConfig(payloadDir, runnerHome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read jitconfig")
}

// TestMaterializeJITConfig_MkdirAllFails covers the case where runnerHome
// cannot be created because its parent path component is a regular file, not
// a directory.
func TestMaterializeJITConfig_MkdirAllFails(t *testing.T) {
	payloadDir := t.TempDir()
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0o600))
	runnerHome := filepath.Join(parent, "runner-home")

	blob := encodeFixtureBlob(t, map[string]string{".runner": `{}`})
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, jitConfigFile), []byte(blob), 0o600))

	err := materializeJITConfig(payloadDir, runnerHome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create runner home")
}

// TestMaterializeJITConfig_PerFileBase64DecodeError covers the case where
// the outer blob is valid base64/JSON, but one entry's value is not valid
// base64, so decoding that individual file's content fails.
func TestMaterializeJITConfig_PerFileBase64DecodeError(t *testing.T) {
	payloadDir := t.TempDir()
	runnerHome := t.TempDir()

	raw, err := json.Marshal(map[string]string{".runner": "not-valid-base64!!"})
	require.NoError(t, err)
	blob := base64.StdEncoding.EncodeToString(raw)
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, jitConfigFile), []byte(blob), 0o600))

	err = materializeJITConfig(payloadDir, runnerHome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode .runner")
}

// TestMaterializeJITConfig_WriteFileFails covers the case where the target
// file path for a runner-config entry is occupied by a directory, so
// os.WriteFile fails.
func TestMaterializeJITConfig_WriteFileFails(t *testing.T) {
	payloadDir := t.TempDir()
	runnerHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(runnerHome, ".runner"), 0o750))

	blob := encodeFixtureBlob(t, map[string]string{".runner": `{"agentId":1}`})
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, jitConfigFile), []byte(blob), 0o600))

	err := materializeJITConfig(payloadDir, runnerHome)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write")
}

// TestInstallCATrust_ReadErrorOtherThanNotExist covers the case where the
// CA cert path exists but can't be read (permission denied), which must
// error rather than be treated as the tolerated "no proxy configured" no-op.
func TestInstallCATrust_ReadErrorOtherThanNotExist(t *testing.T) {
	runnerHome := t.TempDir()
	caPath := filepath.Join(t.TempDir(), "tls.crt")
	writeUnreadable(t, caPath, "cert")

	env, err := installCATrust(runnerHome, caPath)
	require.Error(t, err)
	assert.Nil(t, env)
	assert.Contains(t, err.Error(), "read CA cert")
}

// TestInstallCATrust_ReadSystemCABundleErrorPropagates covers the case where readSystemCABundle returning a non-NotExist error must
// abort installCATrust before anything is written.
func TestInstallCATrust_ReadSystemCABundleErrorPropagates(t *testing.T) {
	unreadable := filepath.Join(t.TempDir(), "ca-certificates.crt")
	writeUnreadable(t, unreadable, "sys")
	withSystemCABundleCandidates(t, []string{unreadable})

	runnerHome := t.TempDir()
	caPath := filepath.Join(t.TempDir(), "tls.crt")
	require.NoError(t, os.WriteFile(caPath, []byte("proxy-cert"), 0o600))

	env, err := installCATrust(runnerHome, caPath)
	require.Error(t, err)
	assert.Nil(t, env)
	assert.Contains(t, err.Error(), "read system CA bundle")

	entries, rerr := os.ReadDir(runnerHome)
	require.NoError(t, rerr)
	assert.Empty(t, entries, "no bundle must be written when the system CA read fails")
}

// TestInstallCATrust_MkdirAllFails covers the case where runnerHome's
// parent path component is a regular file, so MkdirAll fails after the
// combined bundle bytes were already built in memory.
func TestInstallCATrust_MkdirAllFails(t *testing.T) {
	withSystemCABundleCandidates(t, []string{filepath.Join(t.TempDir(), "missing")})

	parent := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0o600))
	runnerHome := filepath.Join(parent, "runner-home")

	caPath := filepath.Join(t.TempDir(), "tls.crt")
	require.NoError(t, os.WriteFile(caPath, []byte("proxy-cert"), 0o600))

	env, err := installCATrust(runnerHome, caPath)
	require.Error(t, err)
	assert.Nil(t, env)
	assert.Contains(t, err.Error(), "create runner home")
}

// TestInstallCATrust_WriteFileFails covers the case where the
// destination bundle path is occupied by a directory, so the final
// os.WriteFile fails.
func TestInstallCATrust_WriteFileFails(t *testing.T) {
	withSystemCABundleCandidates(t, []string{filepath.Join(t.TempDir(), "missing")})

	runnerHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(runnerHome, caBundleFile), 0o750))

	caPath := filepath.Join(t.TempDir(), "tls.crt")
	require.NoError(t, os.WriteFile(caPath, []byte("proxy-cert"), 0o600))

	env, err := installCATrust(runnerHome, caPath)
	require.Error(t, err)
	assert.Nil(t, env)
	assert.Contains(t, err.Error(), "write combined CA bundle")
}

// TestReadSystemCABundle_ErrorOtherThanNotExist covers the case where a candidate path exists but is unreadable, which must surface as
// an error rather than be treated like a missing candidate.
func TestReadSystemCABundle_ErrorOtherThanNotExist(t *testing.T) {
	unreadable := filepath.Join(t.TempDir(), "ca-bundle.crt")
	writeUnreadable(t, unreadable, "x")
	withSystemCABundleCandidates(t, []string{unreadable})

	b, err := readSystemCABundle()
	require.Error(t, err)
	assert.Nil(t, b)
	assert.Contains(t, err.Error(), "read")
}

// TestRun_ReadPayloadErrorIsWrapped covers the case where run() wraps
// readPayload's error with "read payload: " context rather than propagating
// it bare. No subprocess is ever reached because the payload read happens
// first.
func TestRun_ReadPayloadErrorIsWrapped(t *testing.T) {
	t.Setenv("PAYLOAD_SECRET_PATH", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("RUNNER_HOME_DIR", t.TempDir())

	err := run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read payload")
}

// TestRun_MaterializeJITConfigErrorIsWrapped covers the case where a
// jitconfig blob that fails to decode must short-circuit run() with a
// "materialize JIT config: " wrapped error before any pipes or subprocess
// are created.
func TestRun_MaterializeJITConfigErrorIsWrapped(t *testing.T) {
	payloadDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, payloadFile), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, jitConfigFile), []byte("not-base64!!"), 0o600))

	t.Setenv("PAYLOAD_SECRET_PATH", payloadDir)
	t.Setenv("RUNNER_HOME_DIR", t.TempDir())

	err := run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "materialize JIT config")
}

// TestRun_InstallProxyCATrustErrorIsWrapped covers the case where a
// PROXY_CA_CERT_PATH that exists but is unreadable must short-circuit run()
// with an "install proxy CA trust: " wrapped error before any pipes or
// subprocess are created.
func TestRun_InstallCATrustErrorIsWrapped(t *testing.T) {
	payloadDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, payloadFile), []byte(`{}`), 0o600))

	caPath := filepath.Join(t.TempDir(), "tls.crt")
	writeUnreadable(t, caPath, "cert")

	t.Setenv("PAYLOAD_SECRET_PATH", payloadDir)
	t.Setenv("RUNNER_HOME_DIR", t.TempDir())
	t.Setenv("PROXY_CA_CERT_PATH", caPath)

	err := run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install CA trust")
}

func TestTranslateWorkerExitCode(t *testing.T) {
	// Runner.Worker exits 100 + (int)TaskResult. The two results GitHub still
	// concludes as `success` map to 0 so the worker pod ends Succeeded (Q240);
	// every other code (failed/canceled/skipped job, or a crashed worker) passes
	// through verbatim so the pod stays Failed and visible.
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"succeeded (100) -> 0", 100, 0},
		{"succeeded-with-issues (101) -> 0", 101, 0},
		{"failed (102) passes through", 102, 102},
		{"canceled (103) passes through", 103, 103},
		{"skipped (104) passes through", 104, 104},
		{"plain success 0 passes through", 0, 0},
		{"generic error 1 passes through", 1, 1},
		{"SIGKILL/OOM 137 passes through", 137, 137},
		{"SIGSEGV 139 passes through", 139, 139},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := translateWorkerExitCode(tc.in); got != tc.want {
				t.Fatalf("translateWorkerExitCode(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// --- Termination-signal relay (Q385) ---------------------------------------
//
// The wrapper is PID 1 of the worker container, so Kubernetes delivers SIGTERM
// only to it; the child must be signalled explicitly or it runs to the cgroup
// SIGKILL without reporting its job result. These tests assert the child
// actually receives the signal (not merely that the relay runs) and that the
// resulting exit code still reaches the wrapper's caller.

// startIdleChild starts a POSIX shell child in dir that installs trapBody as its
// TERM handler, creates ready.txt to announce the trap is armed, and then idles.
// It returns once the child has confirmed readiness, so a signal sent afterwards
// cannot race the trap installation.
func startIdleChild(t *testing.T, dir, trapBody string) *exec.Cmd {
	t.Helper()
	script := fmt.Sprintf("trap '%s' TERM\n: > ready.txt\nwhile true; do sleep 0.1; done\n", trapBody)
	cmd := exec.Command("/bin/sh", "-c", script) //nolint:gosec // G204: script is a test-local literal, not user input
	cmd.Dir = dir
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "ready.txt"))
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "child never armed its TERM trap")
	return cmd
}

// TestTerminationRelay_ForwardsToChild is the core Q385 assertion: a
// SIGTERM delivered to the wrapper reaches the child, which gets to run its
// shutdown work before exiting with a code of its own choosing.
func TestTerminationRelay_ForwardsToChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worker wrapper targets Linux; shell-stub strategy is POSIX-only")
	}
	dir := t.TempDir()
	// The trap stands in for Runner.Worker reporting job cancellation to GitHub:
	// observable side effect first, then a deliberate exit code.
	cmd := startIdleChild(t, dir, "printf reported > cancelled.txt; exit 7")

	stop, done := armTerminationRelay(30 * time.Second).forwardTo(cmd.Process)
	// Retire the relay even if an assertion below fails: a live relay left
	// registered would swallow the next test's signal.
	t.Cleanup(stop)
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM),
		"the wrapper process must receive the signal Kubernetes would send it")

	err := cmd.Wait()
	stop()
	<-done

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 7, childExitCode(exitErr.ProcessState),
		"the child's own exit code must survive the relay")

	got, readErr := os.ReadFile(filepath.Join(dir, "cancelled.txt"))
	require.NoError(t, readErr, "the child must have run its TERM handler")
	assert.Equal(t, "reported", string(got))
}

// TestTerminationRelay_KillsChildOutlivingGrace pins the bounded-drain
// decision (kubernetes-conventions.md rule 7): a child that ignores SIGTERM is
// killed at the wrapper's own deadline, inside the pod grace period, rather than
// the whole cgroup being SIGKILLed with no record of why.
func TestTerminationRelay_KillsChildOutlivingGrace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worker wrapper targets Linux; shell-stub strategy is POSIX-only")
	}
	dir := t.TempDir()
	cmd := startIdleChild(t, dir, "") // empty trap body: SIGTERM is ignored

	stop, done := armTerminationRelay(200 * time.Millisecond).forwardTo(cmd.Process)
	t.Cleanup(stop)
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))

	err := cmd.Wait()
	stop()
	<-done

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, exitSignalOffset+int(syscall.SIGKILL), childExitCode(exitErr.ProcessState),
		"a child that outlives the grace period is SIGKILLed and reported as 137")
}

// recordingProc is a terminable that records what the relay did to it.
type recordingProc struct {
	mu      sync.Mutex
	signals []os.Signal
	killed  bool
}

func (p *recordingProc) Signal(s os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signals = append(p.signals, s)
	return nil
}

func (p *recordingProc) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killed = true
	return nil
}

func (p *recordingProc) snapshot() ([]os.Signal, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...), p.killed
}

// TestTerminationRelay_QuietExitLeavesChildAlone verifies the normal case
// — the overwhelming majority of worker pods — is untouched: a job that finishes
// on its own is never signalled, and the relay goroutine exits on stop().
func TestTerminationRelay_QuietExitLeavesChildAlone(t *testing.T) {
	proc := &recordingProc{}
	stop, done := armTerminationRelay(time.Millisecond).forwardTo(proc)
	stop()
	stop() // idempotent
	<-done

	signals, killed := proc.snapshot()
	assert.Empty(t, signals, "an unsignalled wrapper must not disturb its child")
	assert.False(t, killed, "the grace timer must not be armed until a signal arrives")
}

// TestTerminationRelay_ForwardsSIGINT covers the interactive/local path:
// SIGINT is relayed as SIGINT, not silently converted to SIGTERM.
func TestTerminationRelay_ForwardsSIGINT(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal delivery to self is POSIX-only in this test")
	}
	proc := &recordingProc{}
	stop, done := armTerminationRelay(30 * time.Second).forwardTo(proc)
	t.Cleanup(stop)
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGINT))
	require.Eventually(t, func() bool {
		signals, _ := proc.snapshot()
		return len(signals) == 1
	}, 10*time.Second, 5*time.Millisecond, "SIGINT was never forwarded")
	stop()
	<-done

	signals, killed := proc.snapshot()
	require.Equal(t, []os.Signal{syscall.SIGINT}, signals)
	assert.False(t, killed, "the child exited within the grace period")
}

// guardSignals registers a test-owned SIGTERM/SIGINT channel so a signal the
// code under test is *not* expected to catch cannot fall through to Go's default
// disposition and kill the test binary. signal.Notify fans out to every
// registered channel, so this observes without stealing.
func guardSignals(t *testing.T) <-chan os.Signal {
	t.Helper()
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGTERM, syscall.SIGINT)
	t.Cleanup(func() { signal.Stop(guard) })
	return guard
}

// TestArmTerminationRelay_ForwardsSignalArrivingBeforeChildStarts is the Q445
// regression test. The wrapper used to call signal.Notify only after starting
// its child, leaving a window in which the pod's one SIGTERM hit the default
// disposition: dropped on the floor for a container's PID 1 (so the child ran to
// the cgroup SIGKILL — the Q385 failure), fatal anywhere else. Arming first must
// mean a signal that beats the child is held and delivered to it, not lost.
func TestArmTerminationRelay_ForwardsSignalArrivingBeforeChildStarts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal delivery to self is POSIX-only in this test")
	}
	relay := armTerminationRelay(30 * time.Second)
	t.Cleanup(relay.disarm)

	// The signal arrives while the relay is armed but has no process to forward
	// to yet — exactly the window the child start used to sit in.
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))
	require.Eventually(t, func() bool { return len(relay.sigCh) == 1 }, 10*time.Second, 5*time.Millisecond,
		"an armed relay must capture a signal that arrives before the child starts")

	proc := &recordingProc{}
	stop, done := relay.forwardTo(proc)
	t.Cleanup(stop)
	require.Eventually(t, func() bool {
		signals, _ := proc.snapshot()
		return len(signals) == 1
	}, 10*time.Second, 5*time.Millisecond, "the held signal was never forwarded to the child")
	stop()
	<-done

	signals, killed := proc.snapshot()
	assert.Equal(t, []os.Signal{syscall.SIGTERM}, signals,
		"a pre-start SIGTERM must reach the child that raced it")
	assert.False(t, killed, "the child exited within the grace period")
}

// TestArmTerminationRelay_DisarmReleasesTheRegistration covers the start-failed
// path: the relay must not keep a registration alive for a child that never
// existed, or the wrapper's own exit path would swallow signals it no longer
// forwards anywhere.
func TestArmTerminationRelay_DisarmReleasesTheRegistration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal delivery to self is POSIX-only in this test")
	}
	guard := guardSignals(t) // keeps the disarmed signal from killing the binary
	relay := armTerminationRelay(30 * time.Second)
	relay.disarm()

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))
	select {
	case <-guard:
	case <-time.After(10 * time.Second):
		t.Fatal("the test's own guard never saw the signal")
	}
	assert.Empty(t, relay.sigCh, "a disarmed relay must no longer receive signals")
}

// TestRunScaleSet_ForwardsSIGTERMToRunSh exercises the scale-set path end to end:
// run.sh is what reports the job in that mode, so the signal must reach it too.
//
// It leans on the Q445 ordering: run.sh's ready.txt is written after
// runScaleSet started it, which is after the relay was armed, so the SIGTERM
// below can only land on an armed handler. With the old arm-after-start order
// that implication did not hold and this test could kill the test binary
// outright instead of failing.
func TestRunScaleSet_ForwardsSIGTERMToRunSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("run.sh exec test is POSIX-only")
	}
	runnerHome := t.TempDir()
	payloadDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(payloadDir, jitConfigFile), []byte("blob=="), 0o600))
	t.Setenv("PROXY_CA_CERT_PATH", "")

	// A run.sh that reports on SIGTERM and exits 0, so runScaleSet returns
	// normally instead of calling os.Exit and taking the test binary with it.
	script := "#!/bin/sh\ntrap 'printf stopped > stopped.txt; exit 0' TERM\n: > ready.txt\nwhile true; do sleep 0.1; done\n"
	require.NoError(t, os.WriteFile(filepath.Join(runnerHome, runnerRunScript), []byte(script), 0o700)) //nolint:gosec // test fixture must be executable

	errCh := make(chan error, 1)
	go func() { errCh <- runScaleSet(payloadDir, runnerHome) }()

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(runnerHome, "ready.txt"))
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "run.sh never started")
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("runScaleSet did not return after SIGTERM")
	}

	got, err := os.ReadFile(filepath.Join(runnerHome, "stopped.txt"))
	require.NoError(t, err, "run.sh must have received the forwarded SIGTERM")
	assert.Equal(t, "stopped", string(got))
}

func TestChildExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal exit codes are POSIX-only")
	}
	t.Run("nil state", func(t *testing.T) {
		assert.Equal(t, 1, childExitCode(nil))
	})
	t.Run("normal exit code passes through", func(t *testing.T) {
		cmd := exec.Command("/bin/sh", "-c", "exit 3")
		err := cmd.Run()
		require.Error(t, err)
		assert.Equal(t, 3, childExitCode(cmd.ProcessState))
	})
	t.Run("signalled child reports 128+signal", func(t *testing.T) {
		cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; while true; do sleep 0.1; done")
		require.NoError(t, cmd.Start())
		require.NoError(t, cmd.Process.Kill())
		_ = cmd.Wait()
		assert.Equal(t, exitSignalOffset+int(syscall.SIGKILL), childExitCode(cmd.ProcessState),
			"os.ProcessState reports -1 for a signalled child; os.Exit(-1) would become a meaningless 255")
	})
}

func TestShutdownGrace(t *testing.T) {
	t.Run("unset falls back to the default", func(t *testing.T) {
		t.Setenv(shutdownGraceEnv, "")
		assert.Equal(t, defaultShutdownGrace, shutdownGrace())
	})
	t.Run("valid duration is honored", func(t *testing.T) {
		t.Setenv(shutdownGraceEnv, "45s")
		assert.Equal(t, 45*time.Second, shutdownGrace())
	})
	t.Run("unparseable value falls back rather than disabling the drain", func(t *testing.T) {
		t.Setenv(shutdownGraceEnv, "soon")
		assert.Equal(t, defaultShutdownGrace, shutdownGrace())
	})
	t.Run("non-positive value falls back", func(t *testing.T) {
		t.Setenv(shutdownGraceEnv, "0s")
		assert.Equal(t, defaultShutdownGrace, shutdownGrace())
	})
}

// TestInstallCATrust_CombinesBothCAs is the worker half of Q536: a GHES tenant
// behind a private CA runs with both mounts, and the bundle the runner reads must
// carry the system roots, the proxy CA, and the appliance CA — losing any one of
// them breaks a different hop.
func TestInstallCATrust_CombinesBothCAs(t *testing.T) {
	stagingDir := t.TempDir()
	systemBundle := filepath.Join(stagingDir, "ca-certificates.crt")
	systemContent := []byte("-----BEGIN CERTIFICATE-----\nFAKE-SYSTEM-CA\n-----END CERTIFICATE-----\n")
	require.NoError(t, os.WriteFile(systemBundle, systemContent, 0o644)) //nolint:gosec // G306: test fixture writing a fake public CA bundle
	withSystemCABundleCandidates(t, []string{systemBundle})

	proxyPath := filepath.Join(stagingDir, "tls.crt")
	require.NoError(t, os.WriteFile(proxyPath,
		[]byte("-----BEGIN CERTIFICATE-----\nFAKE-PROXY-CA\n-----END CERTIFICATE-----\n"), 0o600))
	githubPath := filepath.Join(stagingDir, "ca.crt")
	require.NoError(t, os.WriteFile(githubPath,
		[]byte("-----BEGIN CERTIFICATE-----\nFAKE-GHES-CA\n-----END CERTIFICATE-----\n"), 0o600))

	runnerHome := t.TempDir()
	env, err := installCATrust(runnerHome, proxyPath, githubPath)
	require.NoError(t, err)
	require.Equal(t, []string{"SSL_CERT_FILE=" + filepath.Join(runnerHome, caBundleFile)}, env)

	got, err := os.ReadFile(filepath.Join(runnerHome, caBundleFile))
	require.NoError(t, err)
	for _, want := range []string{"FAKE-SYSTEM-CA", "FAKE-PROXY-CA", "FAKE-GHES-CA"} {
		assert.Contains(t, string(got), want, "combined bundle must retain %s", want)
	}
}

// TestInstallCATrust_GitHubCAOnly covers the direct-egress GHES tenant: no proxy is
// attached, so the appliance CA is the only supplied source and must still land in
// the bundle rather than be skipped along with the absent proxy path.
func TestInstallCATrust_GitHubCAOnly(t *testing.T) {
	withSystemCABundleCandidates(t, []string{filepath.Join(t.TempDir(), "does-not-exist.crt")})

	githubPath := filepath.Join(t.TempDir(), "ca.crt")
	caContent := []byte("-----BEGIN CERTIFICATE-----\nONLY-GHES-CA\n-----END CERTIFICATE-----\n")
	require.NoError(t, os.WriteFile(githubPath, caContent, 0o600))

	runnerHome := t.TempDir()
	env, err := installCATrust(runnerHome, "", githubPath)
	require.NoError(t, err)
	require.Len(t, env, 1)

	got, err := os.ReadFile(filepath.Join(runnerHome, caBundleFile))
	require.NoError(t, err)
	assert.Equal(t, caContent, got)
}
