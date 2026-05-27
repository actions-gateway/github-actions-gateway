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
)

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
	if err := run(); err != nil {
		slog.Error("worker wrapper failed", "error", err)
		os.Exit(1)
	}
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

	// 3. Create anonymous pipes.
	// r1/w1: wrapper writes job → worker reads (workerReadFD in child)
	// r2/w2: worker writes back → wrapper drains  (workerWriteFD in child)
	r1, w1, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create worker-input pipe: %w", err)
	}
	r2, w2, err := os.Pipe()
	if err != nil {
		r1.Close()
		w1.Close()
		return fmt.Errorf("create worker-output pipe: %w", err)
	}

	// 4. Start Runner.Worker.
	// ExtraFiles[0] = r1 → fd 3 in child (worker reads job message)
	// ExtraFiles[1] = w2 → fd 4 in child (worker writes back)
	workerPath, err := exec.LookPath(workerBin)
	if err != nil {
		r1.Close()
		w1.Close()
		r2.Close()
		w2.Close()
		return fmt.Errorf("find Runner.Worker: %w", err)
	}
	cmd := exec.Command(workerPath,
		"spawnclient",
		strconv.Itoa(workerReadFD), strconv.Itoa(workerWriteFD),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{r1, w2}
	if err := cmd.Start(); err != nil {
		r1.Close()
		w1.Close()
		r2.Close()
		w2.Close()
		return fmt.Errorf("start Runner.Worker: %w", err)
	}

	// Child inherited r1 and w2; close our copies so EOF propagates correctly.
	r1.Close()
	w2.Close()

	// 5. Write payload to worker-input pipe concurrently.
	// The write blocks until Runner.Worker opens the read end.
	writeErr := make(chan error, 1)
	go func() {
		defer w1.Close()
		writeErr <- writeJobMessage(w1, payload)
	}()

	// 6. Drain worker-output pipe to prevent the worker blocking on writes.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		defer r2.Close()
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
