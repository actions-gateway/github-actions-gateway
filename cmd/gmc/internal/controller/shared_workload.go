package controller

import (
	corev1 "k8s.io/api/core/v1"
)

// Pod-level facts every GMC-managed workload shares regardless of the API version
// that asked for it: the listener ports, the hardened security baseline, the proxy's
// shutdown budget, and the log-level default. The v1 ActionsGateway, the v2
// ActionsGateway, and the standalone EgressProxy all build on these.

const (
	// agcContainerName is the AGC control-plane container's name inside the AGC pod.
	// It is a distinct, shorter identity from agcAppName (the workload/app name), and
	// anything that addresses the container by name — notably the managed
	// VerticalPodAutoscaler's containerPolicy (Q360), which silently applies to
	// nothing if the name does not match — must use this constant.
	agcContainerName = "agc"

	// proxyPort is the egress proxy's CONNECT listener port.
	proxyPort = int32(8080)

	// healthMetricsPort is the plaintext health listener port (/healthz,
	// /readyz) probed by the kubelet on both the proxy and the AGC pods. It
	// carries no Prometheus metrics — those moved to the mTLS metricsPort below
	// so blanket client-cert enforcement on the metrics listener does not break
	// the certless kubelet probes. The AGC manager pins its health bind address
	// to this port (healthProbeBindAddress in cmd/agc/main.go).
	healthMetricsPort = int32(8081)

	// metricsPort is the dedicated mTLS Prometheus-metrics listener port for
	// both the proxy and the AGC. Each serves /metrics over HTTPS and requires a
	// client certificate signed by the per-tenant metrics CA (see
	// metrics_cert.go); the metrics-scrape NetworkPolicy ingress rule targets
	// this port. The GMC pins the AGC manager's metrics bind address here
	// (cmd/agc/main.go) so the ingress rule provably matches the live listener.
	metricsPort = int32(8443)

	// defaultLogLevel is the log verbosity threaded to the AGC and proxy when a
	// gateway or proxy omits spec.logLevel. Mirrors the CRD's +kubebuilder:default;
	// kept here so a hand-applied CR without the field still runs at info rather
	// than emitting an empty LOG_LEVEL the workloads would have to interpret.
	defaultLogLevel = "info"
)

// logLevelOrDefault returns the configured log level, falling back to
// defaultLogLevel when unset. Threaded to the AGC and proxy as LOG_LEVEL.
func logLevelOrDefault(level string) string {
	if level == "" {
		return defaultLogLevel
	}
	return level
}

// hardenedContainerSecurityContext returns the restricted container
// SecurityContext applied to every GMC-managed container (AGC and proxy):
// non-root, read-only root filesystem, no privilege escalation, all Linux
// capabilities dropped, and the RuntimeDefault seccomp profile. Defining it once
// keeps the security baseline from drifting between Deployments — hardening (or
// accidentally relaxing) one container must not silently leave the other behind.
func hardenedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr(true),
		ReadOnlyRootFilesystem:   ptr(true),
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// nonrootPodSecurityContext returns the pod-level SecurityContext shared by the
// AGC and proxy Deployments: fsGroup 65532 so the distroless nonroot UID can read
// group-owned mounted Secrets (cert/key files projected with mode 0o440). See the
// TLS-mode comment in buildProxyDeployment for the 0o440-vs-0o400 rationale.
func nonrootPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{FSGroup: ptr(int64(65532))}
}

// The proxy pod's shutdown budget (Q384 + Q386).
//
// There is deliberately no `preStop` hook here, even though endpoint removal
// races SIGTERM and something must absorb that race. A preStop sleep is the
// canonical remedy, but it is *serial* with the drain and its cost is
// unconditional: the kubelet grants a terminating pod min(grace period,
// remaining node-shutdown window), so on a truncated window — spot preemption,
// graceful node shutdown — a preStop sleep spends scarce budget idling and
// leaves less for draining than if it were absent, partially undoing Q384
// exactly where disruption is most frequent.
//
// cmd/proxy absorbs the race in-process instead, by holding the CONNECT listener
// open after SIGTERM until arrivals go quiet (lingerForEndpointRemoval). That
// linger is spent INSIDE the drain budget rather than ahead of it, so it costs
// nothing here: the two waits overlap, and the worst case stays what Q384 sized.
//
// The arithmetic — every term is a real wait the kubelet must accommodate:
//
//	drain budget (linger + tunnel drain, cmd/proxy)  45s
//	+ force-close unwind + health listener shutdown   7s
//	+ headroom for process exit and kubelet jitter    8s
//	= terminationGracePeriodSeconds                  60s
//
// Raising PROXY_SHUTDOWN_DRAIN_TIMEOUT on a pool without raising the grace
// period here re-breaks Q384: the drain would still be running when SIGKILL
// lands. The two are documented together in docs/operations/troubleshooting.md.
const (
	// proxyDrainBudgetSeconds mirrors defaultShutdownDrainTimeout in
	// cmd/proxy/proxy.go. cmd/proxy is a separate Go module, so this cannot be
	// an import — change both together.
	proxyDrainBudgetSeconds = 45

	// proxyDrainTailSeconds covers what cmd/proxy does after the drain deadline
	// expires: tunnelCloseGrace (2s) waiting for force-closed relays to unwind,
	// then healthShutdownTimeout (5s) for the health/metrics listener.
	proxyDrainTailSeconds = 7

	// proxyExitHeadroomSeconds absorbs process exit and kubelet scheduling
	// jitter so the budget above is a bound, not a coincidence.
	proxyExitHeadroomSeconds = 8

	// proxyTerminationGracePeriodSeconds is the sum of the terms above. Stated
	// as the arithmetic rather than a literal so the claim the manifest makes
	// stays checkable against the code that has to fit inside it.
	proxyTerminationGracePeriodSeconds = proxyDrainBudgetSeconds +
		proxyDrainTailSeconds + proxyExitHeadroomSeconds
)
