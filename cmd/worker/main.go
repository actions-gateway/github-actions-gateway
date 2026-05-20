// Command worker is the entrypoint wrapper for an ephemeral GitHub Actions
// runner pod. It bridges the Kubernetes Secret world into the Named Pipe
// world that Runner.Worker expects:
//
//  1. Read the job payload from the mounted Secret directory
//     (PAYLOAD_SECRET_PATH, default /run/secrets/job-payload).
//  2. Create two Named Pipes (FIFOs) under RUNNER_PIPE_DIR
//     (default /tmp/runner-pipes): job-in (wrapper→worker) and job-out (worker→wrapper).
//  3. Start Runner.Worker as a child process with --startuptype workerprocess
//     and the pipe paths as positional arguments.
//  4. Write the payload bytes to job-in concurrently to avoid a blocking write
//     deadlocking before Runner.Worker opens the read end.
//  5. Relay Runner.Worker stdout/stderr to our own stdout/stderr.
//  6. Exit with the same exit code as Runner.Worker.
//
// TODO(validation): Confirm the exact wire format Runner.Worker expects on the
// input FIFO against a live binary using testdata/job_payload.json. The
// 4-byte-length-prefix + JSON protocol described here is inferred from the
// open-source runner source (src/Runner.Worker/Worker.cs) and must be validated
// before end-to-end testing.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const (
	defaultPayloadPath = "/run/secrets/job-payload"
	defaultPipeDir     = "/tmp/runner-pipes"
	workerBin          = "Runner.Worker"
	payloadFile        = "payload"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker wrapper failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	payloadDir := envOr("PAYLOAD_SECRET_PATH", defaultPayloadPath)
	pipeDir := envOr("RUNNER_PIPE_DIR", defaultPipeDir)

	// 1. Read payload from Secret mount.
	payload, err := readPayload(payloadDir)
	if err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	slog.Info("payload loaded", "bytes", len(payload))

	// 2. Create named pipes.
	if err := os.MkdirAll(pipeDir, 0o700); err != nil {
		return fmt.Errorf("create pipe dir: %w", err)
	}
	pipeIn := filepath.Join(pipeDir, "job-in")
	pipeOut := filepath.Join(pipeDir, "job-out")

	for _, p := range []string{pipeIn, pipeOut} {
		_ = os.Remove(p) // remove stale pipe from a previous run
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			return fmt.Errorf("mkfifo %s: %w", p, err)
		}
	}

	// 3. Start Runner.Worker with pipe paths.
	workerPath, err := exec.LookPath(workerBin)
	if err != nil {
		return fmt.Errorf("find Runner.Worker: %w", err)
	}
	cmd := exec.Command(workerPath, "--startuptype", "workerprocess", pipeIn, pipeOut)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Runner.Worker: %w", err)
	}

	// 4. Write payload to job-in concurrently (blocks until worker opens read end).
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- writePayloadToPipe(pipeIn, payload)
	}()

	// 5. Wait for Runner.Worker.
	waitErr := cmd.Wait()

	// Drain the write goroutine.
	if werr := <-writeErr; werr != nil {
		slog.Warn("payload write error", "error", werr)
	}

	// 6. Propagate Runner.Worker exit code.
	if waitErr != nil {
		var exitErr *exec.ExitError
		if ok := isExitError(waitErr, &exitErr); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("Runner.Worker: %w", waitErr)
	}
	return nil
}

// writePayloadToPipe writes the 4-byte big-endian length prefix followed by
// the payload bytes into the FIFO at path. Opens the FIFO for writing, which
// blocks until Runner.Worker opens the read end.
//
// TODO(validation): Confirm length-prefix protocol against live Runner.Worker.
func writePayloadToPipe(path string, payload []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, os.ModeNamedPipe)
	if err != nil {
		return fmt.Errorf("open pipe for write: %w", err)
	}
	defer f.Close()

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := f.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("write length prefix: %w", err)
	}
	if _, err := io.WriteString(f, string(payload)); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

func readPayload(dir string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, payloadFile))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func isExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}
