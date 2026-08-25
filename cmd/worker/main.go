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
//  7. Relay Runner.Worker stdout/stderr to our own stdout/stderr, and relay
//     termination signals the other way: the wrapper is PID 1 of the worker
//     container, so a pod SIGTERM (eviction, node drain, `gh run cancel`)
//     reaches only the wrapper — the child must be signalled explicitly or it
//     runs to the cgroup SIGKILL with no chance to report its outcome. See
//     terminationRelay.
//  8. Translate Runner.Worker's exit code: a successful job exits 100 (not 0),
//     because Runner.Worker returns TaskResultUtil.TranslateToReturnCode(result)
//     == 100 + (int)TaskResult and TaskResult.Succeeded == 0. We map the success
//     codes back to 0 so Kubernetes marks the worker pod Succeeded; non-success
//     and fault codes pass through verbatim. See translateWorkerExitCode.
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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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

	// returnCodeOffset mirrors TaskResultUtil._returnCodeOffset in the runner
	// source (src/Runner.Common/Util/TaskResultUtil.cs). Runner.Worker encodes
	// its job result as an exit code of returnCodeOffset + (int)TaskResult, so a
	// successful job (TaskResult.Succeeded == 0) exits exactly returnCodeOffset.
	returnCodeOffset = 100

	// exitSucceeded and exitSucceededWithIssues are the Runner.Worker exit codes
	// for the two job results that GitHub still concludes as `success`:
	// TaskResult.Succeeded (0) and TaskResult.SucceededWithIssues (1). The
	// wrapper maps these to a 0 process exit so the worker pod ends Succeeded.
	exitSucceeded           = returnCodeOffset
	exitSucceededWithIssues = returnCodeOffset + 1

	// caBundleFile is the file name (under RUNNER_HOME_DIR) where the wrapper writes
	// the combined system + supplied CA bundle — the per-tenant egress proxy's CA,
	// and on GHES the CA fronting the appliance (Q536). SSL_CERT_FILE points the
	// .NET HttpClient at this file so its TLS handshakes succeed.
	caBundleFile = "ca-bundle.crt"

	// workerModeEnv selects the wrapper's execution mode. Empty (the default) is
	// the classic M3 mode: the AGC already acquired the job and hands its payload
	// to Runner.Worker over anonymous pipes. workerModeScaleSet is the Q264 Option E
	// mode: there is no payload — the pod runs the full runner (run.sh --jitconfig),
	// which opens its own broker session, pulls its one job, and reports its own
	// completion (§2.4). The AGC provisioner sets this env for ScaleSet-protocol sets.
	workerModeEnv      = "WORKER_MODE"
	workerModeScaleSet = "scaleset"

	// runnerRunScript is the full-runner entrypoint the actions-runner image ships
	// at RUNNER_HOME_DIR/run.sh. The scale-set mode execs it with --jitconfig.
	runnerRunScript = "run.sh"

	// jitConfigFlag is the run.sh flag that consumes the base64 JIT config blob a
	// scale-set worker registers with (the same blob generatejitconfig returns).
	jitConfigFlag = "--jitconfig"

	// shutdownGraceEnv overrides how long the wrapper waits for its child to exit
	// after forwarding a termination signal, as a Go duration (e.g. "20s").
	shutdownGraceEnv = "WORKER_SHUTDOWN_GRACE"

	// defaultShutdownGrace is the wrapper's own drain budget: how long the child
	// gets, after being signalled, to abort its job and report the result to
	// GitHub before the wrapper kills it.
	//
	// It is deliberately shorter than the pod's terminationGracePeriodSeconds
	// (Kubernetes' 30s default; the AGC provisioner does not override it, and a
	// tenant PodTemplate may). Staying inside the pod budget means the wrapper
	// gets to log *why* the job ended and exit with a meaningful code, instead of
	// the whole cgroup being SIGKILLed with no record. Operators who raise
	// terminationGracePeriodSeconds should raise WORKER_SHUTDOWN_GRACE with it.
	defaultShutdownGrace = 25 * time.Second

	// exitSignalOffset is the shell convention for reporting a process killed by
	// a signal: 128 + signal number (so SIGTERM → 143, SIGKILL → 137). os.Process
	// reports such an exit as -1, which os.Exit would turn into a meaningless 255.
	exitSignalOffset = 128
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
	// controllers (k8s audit F1). LOG_LEVEL (info|debug, default info) is the
	// single level source the GMC can crank per tenant without a code change
	// (logging-audit Theme G).
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

// resolveWorkerBin locates the Runner.Worker binary. It prefers
// $RUNNER_HOME_DIR/bin (the actions-runner layout) so resolution does not depend
// on PATH — the wrapper is injected into an unmodified upstream image whose PATH
// may not include the runner bin dir — and falls back to PATH for images that
// place the binary elsewhere.
func resolveWorkerBin(runnerHome string) (string, error) {
	p := filepath.Join(runnerHome, "bin", workerBin)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	p, err := exec.LookPath(workerBin)
	if err != nil {
		return "", fmt.Errorf("find %s (looked in %s/bin and PATH): %w", workerBin, runnerHome, err)
	}
	return p, nil
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

	// Report the runner version this image ships before either mode branches, so a
	// custom workerImage answers the question the AGC's image-reference check cannot
	// (Q715).
	reportRunnerVersion(runnerHome)

	// Scale-set mode (Q264 Option E): no payload to hand off — the pod runs the full
	// runner, which pulls its own job through its own session. The wrapper keeps only
	// its proxy-CA trust duty; the pipes handoff and Runner.Worker spawn below do not
	// run for a ScaleSet worker.
	if strings.EqualFold(os.Getenv(workerModeEnv), workerModeScaleSet) {
		return runScaleSet(payloadDir, runnerHome)
	}

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

	// 2a. Install the mounted CA certs into a combined trust bundle and prepare the
	// env var the child Runner.Worker (and any of its own children — job steps, shell
	// scripts, etc.) needs to find it. The AGC provisioner mounts the egress proxy's
	// CA at PROXY_CA_CERT_PATH and a GHES appliance's CA at GITHUB_CA_CERT_PATH;
	// without trust install, Runner.Worker's .NET HttpClient rejects the corresponding
	// TLS cert with UntrustedRoot before any traffic reaches GitHub. A missing path
	// (e.g. tests, deployments with no per-tenant proxy, public GitHub) is a tolerated
	// no-op.
	caTrustEnv, err := installCATrust(runnerHome,
		os.Getenv("PROXY_CA_CERT_PATH"), os.Getenv("GITHUB_CA_CERT_PATH"))
	if err != nil {
		return fmt.Errorf("install CA trust: %w", err)
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
	workerPath, err := resolveWorkerBin(runnerHome)
	if err != nil {
		_ = r1.Close()
		_ = w1.Close()
		_ = r2.Close()
		_ = w2.Close()
		return err
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
	if len(caTrustEnv) > 0 {
		cmd.Env = append(os.Environ(), caTrustEnv...)
	}
	// Relay pod termination to Runner.Worker: it is not PID 1, so it never sees
	// the pod's SIGTERM on its own (Q385). Arm before starting it — a signal that
	// lands between Start and the registration has no handler to reach (Q445).
	relay := armTerminationRelay(shutdownGrace())
	if err := cmd.Start(); err != nil {
		relay.disarm()
		_ = r1.Close()
		_ = w1.Close()
		_ = r2.Close()
		_ = w2.Close()
		return fmt.Errorf("start Runner.Worker: %w", err)
	}
	stopRelay, relayDone := relay.forwardTo(cmd.Process)

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

	// 7. Wait for Runner.Worker, then retire the signal relay it was driving.
	waitErr := cmd.Wait()
	stopRelay()
	<-relayDone

	// After the process exits its fds close, so drainDone fires promptly.
	<-drainDone

	if werr := <-writeErr; werr != nil {
		slog.Warn("payload write error", "error", werr)
	}

	// 8. Translate and propagate Runner.Worker's exit code.
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			os.Exit(translateWorkerExitCode(childExitCode(exitErr.ProcessState)))
		}
		return fmt.Errorf("Runner.Worker: %w", waitErr)
	}
	return nil
}

// runScaleSet is the Q264 Option E worker path. Unlike the classic mode there is no
// acquired payload to hand off: the pod runs the full actions-runner via
// `run.sh --jitconfig <blob>`, which opens its own broker session, pulls its single
// assigned job, renews its own lock, and reports its own completion (§2.4). The
// wrapper's only remaining duty is installing the per-tenant egress-proxy CA trust so
// the runner's own TLS to GitHub through the proxy succeeds — the same isolation the
// classic path preserves; the App token still never reaches the pod (only the one-shot
// JIT config does).
//
// The JIT blob is the base64 value generatejitconfig returned, staged by the AGC
// provisioner under the Secret's jitconfig key. It is passed to run.sh as the probed
// interface exposes it (§2b-4). It appears in the runner process's argv, but grants
// nothing the job it configures does not already run with (it is that runner's own
// single-job credential in that runner's own pod), so it is not a privilege boundary.
func runScaleSet(payloadDir, runnerHome string) error {
	blob, err := readJITBlob(payloadDir)
	if err != nil {
		return err
	}

	caTrustEnv, err := installCATrust(runnerHome,
		os.Getenv("PROXY_CA_CERT_PATH"), os.Getenv("GITHUB_CA_CERT_PATH"))
	if err != nil {
		return fmt.Errorf("install CA trust: %w", err)
	}

	runScript := filepath.Join(runnerHome, runnerRunScript)
	cmd := exec.Command(runScript, jitConfigFlag, blob) //nolint:gosec // G204: runScript is the runner image's fixed run.sh; blob is the AGC-minted JIT config
	cmd.Dir = runnerHome
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if len(caTrustEnv) > 0 {
		cmd.Env = append(cmd.Env, caTrustEnv...)
	}

	slog.Info("starting scale-set runner", "script", runScript)

	// Same PID 1 problem as the classic path: run.sh has its own SIGTERM handling
	// (it tells its runner to stop after the current job and report it), but it
	// only ever gets the chance if the wrapper forwards the signal (Q385). Arm
	// before Start so no signal can land while the default disposition still
	// applies (Q445).
	relay := armTerminationRelay(shutdownGrace())
	if err := cmd.Start(); err != nil {
		relay.disarm()
		return fmt.Errorf("start run.sh: %w", err)
	}
	stopRelay, relayDone := relay.forwardTo(cmd.Process)
	err = cmd.Wait()
	stopRelay()
	<-relayDone

	if err != nil {
		// run.sh is the full runner, which already uses the conventional 0-is-success
		// exit convention (it translates Runner.Worker's 100-offset itself), so the
		// exit code passes through unmodified — no translateWorkerExitCode.
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(childExitCode(exitErr.ProcessState))
		}
		return fmt.Errorf("run.sh: %w", err)
	}
	return nil
}

// readJITBlob reads and validates the base64 JIT config blob a scale-set worker
// registers with, from <payloadDir>/jitconfig. Unlike the classic materializeJITConfig
// (which decodes the blob into runner config files), the scale-set path hands the blob
// verbatim to run.sh --jitconfig, so a missing or empty blob is a hard error: without
// it the runner cannot register.
func readJITBlob(payloadDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(payloadDir, jitConfigFile))
	if err != nil {
		return "", fmt.Errorf("read jitconfig: %w", err)
	}
	blob := strings.TrimSpace(string(raw))
	if blob == "" {
		return "", fmt.Errorf("empty jitconfig blob at %s/%s: a scale-set worker cannot register without a JIT config", payloadDir, jitConfigFile)
	}
	return blob, nil
}

// terminable is the subset of *os.Process that terminationRelay.forwardTo needs.
// Narrowing it keeps the relay unit-testable against a recording fake without
// spawning a process.
type terminable interface {
	Signal(os.Signal) error
	Kill() error
}

// shutdownGrace returns the wrapper's drain budget from WORKER_SHUTDOWN_GRACE,
// falling back to defaultShutdownGrace when unset, unparseable, or non-positive
// (a misconfigured value must not silently disable the child's chance to report
// its job result).
func shutdownGrace() time.Duration {
	raw := os.Getenv(shutdownGraceEnv)
	if raw == "" {
		return defaultShutdownGrace
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("ignoring invalid "+shutdownGraceEnv,
			"value", raw, "using", defaultShutdownGrace)
		return defaultShutdownGrace
	}
	return d
}

// terminationRelay is a SIGTERM/SIGINT registration held on behalf of a child
// process that does not exist yet. It exists because the wrapper is PID 1 of the
// worker container: Kubernetes delivers SIGTERM to PID 1 only, so without this
// relay the child (Runner.Worker in classic mode, run.sh in scale-set mode) is
// never told the pod is going away and runs until the cgroup SIGKILL at grace
// expiry — with no chance to abort its job and report the cancellation, leaving
// GitHub to wait out the job lock instead (Q385). See
// docs/development/kubernetes-conventions.md § Graceful shutdown, rule 5.
//
// Arming and forwarding are two steps rather than one so the registration can be
// made *before* the child is started: see armTerminationRelay.
type terminationRelay struct {
	sigCh chan os.Signal
	grace time.Duration
}

// armTerminationRelay registers for SIGTERM/SIGINT and returns a relay ready to
// forward them. Call it before starting the child, then hand the started process
// to forwardTo (or, if the start failed, call disarm).
//
// The ordering is the whole point. Between process start and the first
// signal.Notify, Go leaves the signal at its default disposition, and there the
// wrapper has no good outcome: as PID 1 of the worker container the kernel drops
// a default-disposition SIGTERM on the floor (SIGNAL_UNKILLABLE), so the pod's
// one termination notice is lost and the child runs to the cgroup SIGKILL — the
// exact Q385 failure this relay exists to prevent; run anywhere that is *not*
// PID 1 (a test binary, a local run) the default disposition kills the process
// outright instead (Q445). Registering first closes the window in both roles,
// the same way cmd/proxy registers a tunnel before hijacking it (rule 4).
//
// A signal that arrives while the relay is armed but the child is not yet
// started is held in the buffered channel and forwarded as soon as forwardTo
// runs, so an early SIGTERM stops the job it raced instead of being swallowed.
func armTerminationRelay(grace time.Duration) *terminationRelay {
	// Buffered: signal.Notify never blocks, so an unbuffered channel would drop
	// the very signal we exist to forward if the goroutine is mid-iteration —
	// or, before forwardTo, if no goroutine is receiving yet.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	return &terminationRelay{sigCh: sigCh, grace: grace}
}

// disarm releases an armed relay whose child never started, restoring the
// default disposition. It is the error-path counterpart of forwardTo.
func (r *terminationRelay) disarm() { signal.Stop(r.sigCh) }

// forwardTo starts forwarding the armed signals to proc until stop is called.
//
// grace bounds the wait, per rule 7 — a drain whose worst case we can name. It
// starts at the first forwarded signal; if the child is still alive when it
// expires, the relay SIGKILLs it and logs the overrun, so the wrapper exits
// deliberately with a reported reason rather than being killed mid-write by the
// kubelet. Signals arriving after the first are forwarded too (a second SIGTERM
// does not shorten the deadline) so an operator's repeated Ctrl-C reaches the
// child.
//
// It returns immediately. stop is idempotent and ends the relay; done is closed
// once the relay goroutine has exited, so the caller controls whether and how to
// wait (repo convention: async work hands back a done channel rather than hiding
// it in a closure). Call stop only after waiting on the child — stopping earlier
// would restore the default disposition and let a late SIGTERM kill the wrapper
// out from under a child that is still reporting.
func (r *terminationRelay) forwardTo(proc terminable) (stop func(), done <-chan struct{}) {
	sigCh, grace := r.sigCh, r.grace

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	var once sync.Once

	go func() {
		defer close(doneCh)
		defer signal.Stop(sigCh)

		var deadline <-chan time.Time
		timer := time.NewTimer(grace)
		// Not started yet: drain the timer so it only fires once armed below.
		if !timer.Stop() {
			<-timer.C
		}
		defer timer.Stop()

		for {
			select {
			case <-stopCh:
				return
			case sig := <-sigCh:
				slog.Info("forwarding termination signal to child",
					"signal", sig.String(), "grace", grace)
				if err := proc.Signal(sig); err != nil {
					slog.Warn("could not forward signal to child",
						"signal", sig.String(), "error", err)
				}
				if deadline == nil {
					timer.Reset(grace)
					deadline = timer.C
				}
			case <-deadline:
				slog.Error("child outlived the shutdown grace period; killing it",
					"grace", grace, "hint", "raise "+shutdownGraceEnv+" (and the pod's terminationGracePeriodSeconds) if jobs need longer to report cancellation")
				if err := proc.Kill(); err != nil {
					slog.Warn("could not kill child", "error", err)
				}
				// A nil channel blocks forever: the deadline is one-shot, and the
				// relay now just waits for the caller's stop after cmd.Wait returns.
				deadline = nil
			}
		}
	}()

	return func() { once.Do(func() { close(stopCh) }) }, doneCh
}

// childExitCode maps a finished child's ProcessState onto the exit code the
// wrapper should return. A child killed by a signal has no exit status —
// ProcessState.ExitCode reports -1, which os.Exit would truncate to 255 — so it
// is reported using the shell's 128+signal convention (SIGTERM → 143). That
// keeps a cancelled or killed job distinguishable from a job that exited 255 on
// its own.
func childExitCode(state *os.ProcessState) int {
	if state == nil {
		return 1
	}
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return exitSignalOffset + int(ws.Signal())
	}
	return 1
}

// translateWorkerExitCode maps a Runner.Worker process exit code onto the exit
// code the wrapper itself should return.
//
// Runner.Worker does not use the conventional 0-is-success exit convention: it
// returns TaskResultUtil.TranslateToReturnCode(result) == 100 + (int)TaskResult
// (actions/runner src/Runner.Common/Util/TaskResultUtil.cs), so a job that
// concludes `success` exits 100, not 0. In the upstream architecture
// Runner.Listener consumes this code, translates it back, and exits 0 itself —
// the 100-offset never escapes the process tree. Because the wrapper spawns
// Runner.Worker directly it must do the same, otherwise every successful job
// leaves the container at exit 100 and Kubernetes marks the worker pod Failed —
// cosmetic (the AGC provisioner never inspects the exit code), but it makes
// successful jobs look like failures to operators and their dashboards (Q240).
//
// Only the two results GitHub still concludes as `success` — Succeeded (100) and
// SucceededWithIssues (101) — are mapped to 0. Every other code passes through
// verbatim: a genuinely failed, canceled, or skipped job keeps its 100-offset
// code, and a crashed worker keeps its signal/fault code, so those pods still
// end Failed and remain visible for investigation.
func translateWorkerExitCode(code int) int {
	switch code {
	case exitSucceeded, exitSucceededWithIssues:
		return 0
	default:
		return code
	}
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

// installCATrust reads each supplied CA cert path, concatenates them with the
// host's existing OS trust bundle, writes the combined PEM into runnerHome under
// caBundleFile, and returns the env vars the child Runner.Worker (and any of its own
// subprocesses) needs to use that bundle. The returned slice is `KEY=VALUE` strings
// ready for append-onto-os.Environ().
//
// The paths are the per-tenant egress proxy's CA and, on a GHES appliance behind a
// private CA, that appliance's CA (Q536). Both are additive to the system bundle,
// never a replacement.
//
// Behaviour, per path:
//
//   - "" → not configured, contributes nothing.
//   - a missing file → tolerated as a no-op so the wrapper keeps working in unit
//     tests or when the AGC provisioner ran without the corresponding mount. The
//     wrapper logs and continues.
//   - a read failure for any other reason → error (the AGC mounted it but we can't
//     read it; failing fast surfaces a misconfiguration before the runner times out
//     chasing an UntrustedRoot).
//
// With no path contributing a cert the bundle is not written at all and nil env is
// returned, leaving the runner on its image's own trust store.
//
// The combined bundle is written world-readable (0o644) because the runner
// user (UID 1001 in the actions-runner image) is also the only consumer; the
// cert is public and adding restrictive permissions would just risk locking
// out a future supplemental container running as a different UID.
func installCATrust(runnerHome string, caPaths ...string) ([]string, error) {
	var extra [][]byte
	var installed []string
	for _, caPath := range caPaths {
		if caPath == "" {
			continue
		}
		caPEM, err := os.ReadFile(caPath)
		if err != nil {
			if os.IsNotExist(err) {
				slog.Info("no CA cert mounted at this path; skipping it", "path", caPath)
				continue
			}
			return nil, fmt.Errorf("read CA cert %s: %w", caPath, err)
		}
		if len(bytes.TrimSpace(caPEM)) == 0 {
			slog.Warn("CA cert file is empty; skipping it", "path", caPath)
			continue
		}
		extra = append(extra, caPEM)
		installed = append(installed, caPath)
	}
	if len(extra) == 0 {
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
	for _, caPEM := range extra {
		combined.Write(caPEM)
		if !bytes.HasSuffix(caPEM, []byte("\n")) {
			combined.WriteByte('\n')
		}
	}

	if err := os.MkdirAll(runnerHome, 0o700); err != nil {
		return nil, fmt.Errorf("create runner home %s: %w", runnerHome, err)
	}
	target := filepath.Join(runnerHome, caBundleFile)
	if err := os.WriteFile(target, combined.Bytes(), 0o644); err != nil { //nolint:gosec // G306: a CA trust bundle holds public certs and must be world-readable for the runner
		return nil, fmt.Errorf("write combined CA bundle %s: %w", target, err)
	}
	slog.Info("CA trust installed",
		"bundle", target, "extra_certs", installed, "system_bytes", len(systemPEM))

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
// (some minimal base images ship without one — the supplied CAs alone still work
// for their own endpoints, just not for any other TLS endpoint).
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
