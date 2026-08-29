package provisioner

import (
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// WorkerAuditMode selects the worker-address audit record the AGC writes to its
// structured log stream.
//
// It exists to make the proxy's per-connection egress record attributable. That
// record names a destination and, under EgressProxy.spec.auditLogging:
// ConnectionsWithSource, the source address that reached it; this one says which
// tenant and which job held that address, and names no destination at all.
// Neither is a movement log alone — the join of the two is — so each half is
// opted into separately and both default off (Q986, docs/design/05-security.md).
//
// The join has to be recorded live: a worker pod is deleted when its job ends
// and its address returns to the CNI's pool, so nothing can resolve an address
// to a job after the fact.
type WorkerAuditMode string

const (
	// WorkerAuditOff writes no worker-address record. The default, and what an
	// unset, empty, or unrecognized AGC_AUDIT_LOGGING resolves to.
	WorkerAuditOff WorkerAuditMode = "Off"
	// WorkerAuditAddresses writes one record when a worker pod's address is first
	// observed and one when the pod goes away.
	WorkerAuditAddresses WorkerAuditMode = "WorkerAddresses"
)

const (
	// workerAuditMsg is the stable slog message on every worker-address record,
	// and how a collector selects the stream. It is deliberately distinct from
	// the proxy's "egress audit": the two records come from different processes
	// and answer different questions, and one selector matching both would make
	// an operator filter them apart again.
	workerAuditMsg = "worker address audit"
	// workerAuditEventBind marks a worker pod's address becoming live.
	workerAuditEventBind = "bind"
	// workerAuditEventRelease marks the pod going away, which is when the address
	// may be handed to another pod. It bounds the window a bind opened.
	workerAuditEventRelease = "release"
)

// ParseWorkerAuditMode maps the AGC_AUDIT_LOGGING value onto a mode. Anything it
// does not recognize resolves to WorkerAuditOff, so a GMC newer than the AGC
// image can only under-record.
func ParseWorkerAuditMode(s string) WorkerAuditMode {
	if strings.EqualFold(strings.TrimSpace(s), string(WorkerAuditAddresses)) {
		return WorkerAuditAddresses
	}
	return WorkerAuditOff
}

// workerAuditable reports whether pod gets a record under mode, and its owner.
//
// A pod with no address has nothing to bind. A pod with no owner label is not a
// worker: the tenant namespace also holds the AGC's own pod, whose egress shares
// the pool, and leaving it unbound is what lets an auditor tell control-plane
// traffic from a worker's.
func workerAuditable(mode WorkerAuditMode, pod *corev1.Pod) (owner string, ok bool) {
	if mode != WorkerAuditAddresses || pod == nil || pod.Status.PodIP == "" {
		return "", false
	}
	return workerOwner(pod)
}

// logWorkerAddress writes one worker-address record for pod.
//
// Every field is read from the pod object the informer delivered, never from
// anything the worker itself said, so a job cannot forge its own attribution.
// The run ID and repository are the annotations the provisioner stamps at
// creation; a pod carrying neither — a scale-set worker whose payload had no run
// identity — still gets a record, because the address-to-namespace half is what
// attributes a shared pool and it does not depend on them.
//
// Written at info: an audit record must never depend on raising a tenant to
// debug, and raising one to debug must never add a field to it.
func logWorkerAddress(log *slog.Logger, mode WorkerAuditMode, pod *corev1.Pod, event string) {
	owner, ok := workerAuditable(mode, pod)
	if !ok || log == nil {
		return
	}
	attrs := []any{
		"namespace", pod.Namespace,
		"event", event,
		"podIP", pod.Status.PodIP,
		"pod", pod.Name,
		"owner", owner,
	}
	if v := pod.Annotations[AnnotationRunID]; v != "" {
		attrs = append(attrs, "runID", v)
	}
	if v := pod.Annotations[AnnotationRepository]; v != "" {
		attrs = append(attrs, "repository", v)
	}
	log.Info(workerAuditMsg, attrs...)
}
