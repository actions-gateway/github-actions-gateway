// Command worker is the entrypoint wrapper for an ephemeral GitHub Actions
// runner pod. It bridges the Kubernetes Secret world into the anonymous-pipe
// world that Runner.Worker expects:
//
//  1. Read the job payload from the mounted Secret directory
//     (PAYLOAD_SECRET_PATH, default /run/secrets/job-payload).
//  2. Materialize the runner configuration files (.runner, .credentials,
//     .credentials_rsaparams) from the Secret's "jitconfig" key into the
//     runner's home directory (RUNNER_HOME_DIR, default /home/runner).
//     Runner.Worker reads these files at startup via ConfigurationStore.
//  3. Create two OS anonymous pipes (not FIFOs — inherited file descriptors):
//     pipe-in (fd 3 in child): wrapper → worker
//     pipe-out (fd 4 in child): worker → wrapper
//  4. Start Runner.Worker with three positional args: "spawnclient" and the
//     inherited FD numbers (3 and 4). Reference: actions/runner v2.327.1
//     src/Runner.Worker/Program.cs — args.Length must equal 3, args[0] must
//     be "spawnclient", args[1] is pipeIn (read fd), args[2] is pipeOut
//     (write fd).
//  5. Write the job payload as a NewJobRequest message to pipe-in
//     concurrently (the write blocks until Runner.Worker reads).
//  6. Drain pipe-out to prevent the worker from blocking on writes.
//  7. Relay Runner.Worker stdout/stderr to our own stdout/stderr.
//  8. Exit with the same exit code as Runner.Worker.
//
// Wire format (ProcessChannel / StreamString in the runner source,
// src/Runner.Common/ProcessChannel.cs and StreamString.cs):
//
//	[4 bytes LE] MessageType (1 = NewJobRequest)
//	[4 bytes LE] byte-length of body encoded as UTF-16LE
//	[N bytes]    job payload JSON encoded as UTF-16LE
//
// The pipe handles are OS anonymous pipes — not named pipes / FIFOs. On Linux,
// AnonymousPipeClientStream in .NET opens the pipe by its integer FD number
// (passed as a string argument). Go's ExtraFiles maps index 0 → fd 3 and
// index 1 → fd 4 in the child process, which is why those constants are fixed.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	defaultPayloadPath = "/run/secrets/job-payload"
	defaultRunnerHome  = "/home/runner"
	workerBin          = "Runner.Worker"
	payloadFile        = "payload"
	jitConfigFile      = "jitconfig"

	// msgTypeNewJobRequest is MessageType.NewJobRequest from the runner source.
	msgTypeNewJobRequest = 1

	// workerReadFD and workerWriteFD are the FD numbers Runner.Worker receives
	// as positional CLI arguments. Go's ExtraFiles[0] → fd 3, [1] → fd 4.
	workerReadFD  = 3
	workerWriteFD = 4

	// proxyCABundleFile is the file name (under RUNNER_HOME_DIR) where the
	// wrapper writes the combined system + per-tenant-proxy CA bundle.
	// SSL_CERT_FILE points the .NET HttpClient at this file so its TLS
	// handshake with the egress proxy succeeds.
	proxyCABundleFile = "proxy-ca-bundle.crt"
)

// systemCABundleCandidates lists the canonical OS trust-bundle paths we know
// how to extend. The wrapper concatenates whichever of these exists with the
// mounted proxy CA. The actions-runner base image is Ubuntu, so
// /etc/ssl/certs/ca-certificates.crt is the live path in production; the
// others are kept for portability in tests or alternate base images.
var systemCABundleCandidates = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian / Ubuntu (actions-runner base)
	"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL / Fedora
	"/etc/ssl/cert.pem",                  // BSD / macOS
}

// runnerConfigFiles is the allowlist of file names the wrapper will materialize
// from the JIT config blob. The runner generate-jitconfig endpoint always
// returns these three keys; anything else is ignored to keep the wrapper from
// writing attacker-controlled file names into the runner's home directory.
var runnerConfigFiles = map[string]bool{
	".runner":                true,
	".credentials":           true,
	".credentials_rsaparams": true,
}

func main() {
	// Emit structured JSON on stderr so the worker shares one log shape with the
	// controllers (k8s audit F1); previously the package-level slog functions used
	// the stdlib TEXT handler, which a JSON log pipeline cannot parse. LOG_LEVEL
	// (info|debug, default info) is the single level source the GMC can crank per
	// tenant without a code change (logging-audit Theme G).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevelFromEnv()})))
	// Install mode: copy this binary into a shared volume so a runner container —
	// an unmodified upstream actions-runner image with no wrapper of its own — can
	// exec it. Used by the initContainer wrapper-delivery path; the OCI
	// image-volume path mounts the binary read-only and skips this. Usage:
	//   wrapper install <dir>
	if len(os.Args) == 3 && os.Args[1] == "install" {
		if err := installSelf(os.Args[2]); err != nil {
			slog.Error("wrapper install failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("worker wrapper failed", "error", err)
		os.Exit(1)
	}
}

// installSelf copies the running wrapper executable into dir as "wrapper" (mode
// 0o755). The initContainer wrapper-delivery path runs `wrapper install <dir>`
// against a shared volume so the runner container can exec the binary from there.
func installSelf(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	src, err := os.Open(self)
	if err != nil {
		return fmt.Errorf("open self: %w", err)
	}
	defer func() { _ = src.Close() }()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	dst := filepath.Join(dir, "wrapper")
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // G302: an entrypoint binary must be executable
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy wrapper: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	slog.Info("wrapper installed", "path", dst)
	return nil
}

// logLevelFromEnv maps LOG_LEVEL (info|debug, default info) to a slog.Level.
func logLevelFromEnv() slog.Level {
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func run() error {
	payloadDir := envOr("PAYLOAD_SECRET_PATH", defaultPayloadPath)
	runnerHome := envOr("RUNNER_HOME_DIR", defaultRunnerHome)

	// 1. Read payload from Secret mount.
	payload, err := readPayload(payloadDir)
	if err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	slog.Info("payload loaded", "bytes", len(payload))

	// 2. Materialize the runner configuration files from the JIT blob.
	// Runner.Worker's ConfigurationStore.GetSettings() loads .runner /
	// .credentials / .credentials_rsaparams from $HOME at startup and fails
	// with ArgumentNullException: configuredSettings when they are absent.
	if err := materializeJITConfig(payloadDir, runnerHome); err != nil {
		return fmt.Errorf("materialize JIT config: %w", err)
	}

	// 2a. Install the per-tenant egress-proxy CA cert into a combined trust
	// bundle and prepare the env var the child Runner.Worker (and any of its
	// own children — job steps, shell scripts, etc.) needs to find it. The
	// AGC provisioner mounts the CA at PROXY_CA_CERT_PATH; without trust
	// install, Runner.Worker's .NET HttpClient rejects the proxy's TLS cert
	// with UntrustedRoot before any traffic reaches GitHub. A missing path
	// (e.g. tests, deployments with no per-tenant proxy) is a tolerated
	// no-op.
	proxyTrustEnv, err := installProxyCATrust(os.Getenv("PROXY_CA_CERT_PATH"), runnerHome)
	if err != nil {
		return fmt.Errorf("install proxy CA trust: %w", err)
	}

	// 3. Create anonymous pipes.
	// r1/w1: wrapper writes job → worker reads (workerReadFD in child)
	// r2/w2: worker writes back → wrapper drains  (workerWriteFD in child)
	r1, w1, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create worker-input pipe: %w", err)
	}
	r2, w2, err := os.Pipe()
	if err != nil {
		_ = r1.Close()
		_ = w1.Close()
		return fmt.Errorf("create worker-output pipe: %w", err)
	}

	// 4. Start Runner.Worker.
	// ExtraFiles[0] = r1 → fd 3 in child (worker reads job message)
	// ExtraFiles[1] = w2 → fd 4 in child (worker writes back)
	// Resolve Runner.Worker. Prefer $RUNNER_HOME_DIR/bin (the actions-runner
	// layout) so resolution does not depend on PATH: the wrapper is injected into
	// an unmodified upstream image whose PATH may not include the runner bin dir.
	// Fall back to PATH for images that place the binary elsewhere.
	workerPath := filepath.Join(runnerHome, "bin", workerBin)
	if _, statErr := os.Stat(workerPath); statErr != nil {
		var lookErr error
		workerPath, lookErr = exec.LookPath(workerBin)
		if lookErr != nil {
			_ = r1.Close()
			_ = w1.Close()
			_ = r2.Close()
			_ = w2.Close()
			return fmt.Errorf("find %s (looked in %s/bin and PATH): %w", workerBin, runnerHome, lookErr)
		}
	}
	cmd := exec.Command(workerPath, //nolint:gosec // G204: workerPath is the discovered Runner.Worker binary, not user input
		"spawnclient",
		strconv.Itoa(workerReadFD), strconv.Itoa(workerWriteFD),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{r1, w2}
	// Pass the proxy-trust env on top of the inherited environment so .NET's
	// OpenSSL store picks up our combined bundle. Empty slice means no proxy
	// CA was configured; in that case we leave cmd.Env nil and the child
	// inherits the wrapper's env unchanged.
	if len(proxyTrustEnv) > 0 {
		cmd.Env = append(os.Environ(), proxyTrustEnv...)
	}
	if err := cmd.Start(); err != nil {
		_ = r1.Close()
		_ = w1.Close()
		_ = r2.Close()
		_ = w2.Close()
		return fmt.Errorf("start Runner.Worker: %w", err)
	}

	// Child inherited r1 and w2; close our copies so EOF propagates correctly.
	_ = r1.Close()
	_ = w2.Close()

	// 5. Write payload to worker-input pipe concurrently.
	// The write blocks until Runner.Worker opens the read end.
	writeErr := make(chan error, 1)
	go func() {
		defer func() { _ = w1.Close() }()
		writeErr <- writeJobMessage(w1, payload)
	}()

	// 6. Drain worker-output pipe to prevent the worker blocking on writes.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		defer func() { _ = r2.Close() }()
		_, _ = io.Copy(io.Discard, r2)
	}()

	// 7. Wait for Runner.Worker.
	waitErr := cmd.Wait()

	// After the process exits its fds close, so drainDone fires promptly.
	<-drainDone

	if werr := <-writeErr; werr != nil {
		slog.Warn("payload write error", "error", werr)
	}

	// 8. Propagate Runner.Worker exit code.
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("Runner.Worker: %w", waitErr)
	}
	return nil
}

// writeJobMessage writes a NewJobRequest message to w using the wire format
// defined by ProcessChannel/StreamString in the runner source.
func writeJobMessage(w io.Writer, payload []byte) error {
	body := encodeUTF16LE(string(payload))

	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[:4], msgTypeNewJobRequest)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

// encodeUTF16LE encodes s as UTF-16LE bytes, matching UnicodeEncoding in C#.
func encodeUTF16LE(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	b := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return b
}

func readPayload(dir string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, payloadFile))
}

// materializeJITConfig reads the base64-encoded JIT config blob from
// <payloadDir>/jitconfig and writes the runner configuration files
// (.runner, .credentials, .credentials_rsaparams) under runnerHome.
//
// The blob is a base64-encoded JSON object mapping file names to the
// base64-encoded contents of each file (the format returned verbatim by
// GitHub's POST /actions/runners/generate-jitconfig endpoint and stored in
// the agent Secret by the AGC).
//
// A missing jitconfig file is tolerated and is a no-op: this preserves the
// behavior of agents created by registrars that do not produce a JIT blob
// (e.g. stub agents in pre-M3 integration tests). Runner.Worker will fail
// at startup with ArgumentNullException: configuredSettings when the files
// are absent, so callers who care must ensure the AGC populated the key.
func materializeJITConfig(payloadDir, runnerHome string) error {
	blob, err := os.ReadFile(filepath.Join(payloadDir, jitConfigFile))
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("no JIT config blob in payload Secret; skipping runner config materialization")
			return nil
		}
		return fmt.Errorf("read jitconfig: %w", err)
	}
	trimmed := strings.TrimSpace(string(blob))
	if trimmed == "" {
		slog.Info("empty JIT config blob; skipping runner config materialization")
		return nil
	}

	decodedBlob, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return fmt.Errorf("decode base64 blob: %w", err)
	}

	var files map[string]string
	if err := json.Unmarshal(decodedBlob, &files); err != nil {
		return fmt.Errorf("parse JIT config JSON: %w", err)
	}

	if err := os.MkdirAll(runnerHome, 0o700); err != nil {
		return fmt.Errorf("create runner home %s: %w", runnerHome, err)
	}

	for name, encoded := range files {
		if !runnerConfigFiles[name] {
			slog.Warn("ignoring unexpected JIT config entry", "name", name)
			continue
		}
		content, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("decode %s: %w", name, err)
		}
		target := filepath.Join(runnerHome, name)
		// 0o600 — runner credentials include an RSA private key (in .credentials_rsaparams).
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		slog.Info("runner config file written", "path", target, "bytes", len(content))
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// installProxyCATrust reads the per-tenant egress-proxy CA cert from caPath,
// concatenates it with the host's existing OS trust bundle, writes the
// combined PEM into runnerHome under proxyCABundleFile, and returns the env
// vars the child Runner.Worker (and any of its own subprocesses) needs to use
// that bundle. The returned slice is `KEY=VALUE` strings ready for
// append-onto-os.Environ().
//
// Behaviour:
//
//   - caPath == "" → no proxy configured, returns nil env (no-op).
//   - caPath points at a missing file → tolerated as a no-op so the wrapper
//     keeps working in unit tests or when the AGC provisioner ran without a
//     proxy TLS Secret. The wrapper logs and continues.
//   - caPath read fails for any other reason → error (the AGC mounted a
//     Secret but we can't read it; failing fast surfaces a misconfiguration
//     before the runner times out chasing an UntrustedRoot).
//
// The combined bundle is written world-readable (0o644) because the runner
// user (UID 1001 in the actions-runner image) is also the only consumer; the
// cert is public and adding restrictive permissions would just risk locking
// out a future supplemental container running as a different UID.
func installProxyCATrust(caPath, runnerHome string) ([]string, error) {
	if caPath == "" {
		return nil, nil
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("no proxy CA cert mounted; skipping trust-store install",
				"path", caPath)
			return nil, nil
		}
		return nil, fmt.Errorf("read proxy CA cert %s: %w", caPath, err)
	}
	if len(bytes.TrimSpace(caPEM)) == 0 {
		slog.Warn("proxy CA cert file is empty; skipping trust-store install",
			"path", caPath)
		return nil, nil
	}

	systemPEM, err := readSystemCABundle()
	if err != nil {
		return nil, fmt.Errorf("read system CA bundle: %w", err)
	}

	var combined bytes.Buffer
	combined.Write(systemPEM)
	if len(systemPEM) > 0 && !bytes.HasSuffix(systemPEM, []byte("\n")) {
		combined.WriteByte('\n')
	}
	combined.Write(caPEM)
	if !bytes.HasSuffix(caPEM, []byte("\n")) {
		combined.WriteByte('\n')
	}

	if err := os.MkdirAll(runnerHome, 0o700); err != nil {
		return nil, fmt.Errorf("create runner home %s: %w", runnerHome, err)
	}
	target := filepath.Join(runnerHome, proxyCABundleFile)
	if err := os.WriteFile(target, combined.Bytes(), 0o644); err != nil { //nolint:gosec // G306: a CA trust bundle holds public certs and must be world-readable for the runner
		return nil, fmt.Errorf("write combined CA bundle %s: %w", target, err)
	}
	slog.Info("proxy CA trust installed",
		"bundle", target, "extra_cert", caPath, "system_bytes", len(systemPEM))

	// SSL_CERT_FILE is honored by OpenSSL's default verify-paths logic; .NET 6+
	// on Linux delegates X509Chain validation to OpenSSL via X509_STORE so it
	// picks this up without any .NET-specific configuration. SSL_CERT_DIR is
	// intentionally left untouched — pointing it at a non-hashed directory
	// would BREAK OpenSSL (it expects c_rehash output), and our single-file
	// bundle is sufficient.
	return []string{"SSL_CERT_FILE=" + target}, nil
}

// readSystemCABundle returns the contents of the first existing OS trust
// bundle from systemCABundleCandidates. Empty result with no error is valid
// (some minimal base images ship without one — the proxy CA alone still works
// for the proxy handshake, just not for any other TLS endpoint).
func readSystemCABundle() ([]byte, error) {
	for _, p := range systemCABundleCandidates {
		b, err := os.ReadFile(p)
		if err == nil {
			return b, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
	}
	return nil, nil
}
