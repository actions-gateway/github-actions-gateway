package controller

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Reconciler plumbing that carries no API version: the step-tagged provisioning
// error every reconciler wraps its failures in, the RoleBinding recreate sentinel,
// the EgressRulesStale vocabulary, the tenant-namespace marker the admission policy
// keys on, and the optional ServiceMonitor GVK. The v1 and v2 ActionsGateway
// reconcilers, the EgressProxy reconciler and the namespace PSA reconciler all use
// these.

// provisioningError wraps a reconcileResources failure with the named step that
// was in progress when it failed, so Reconcile can surface that step in the
// Degraded condition before returning early (Q156). It Unwraps to the underlying
// error so errors.Is/As and IsConflict-style checks still see the cause.
type provisioningError struct {
	step string
	err  error
}

func (e *provisioningError) Error() string { return fmt.Sprintf("%s: %v", e.step, e.err) }
func (e *provisioningError) Unwrap() error { return e.err }

// errRoleRefImmutable signals that an existing RoleBinding references a
// different (immutable) roleRef than desired, so it must be deleted and
// recreated rather than patched. It never escapes the applyRoleBinding helpers.
var errRoleRefImmutable = errors.New("rolebinding roleRef changed; recreate required")

// errDeploymentSelectorImmutable signals that an existing Deployment's
// spec.selector differs from the desired one, so it must be deleted and recreated
// rather than patched. It never escapes the applyDeployment helper.
var errDeploymentSelectorImmutable = errors.New("deployment selector changed; recreate required")

// DefaultEgressStaleThreshold is the EgressRulesStale age threshold when a
// reconciler's EgressStaleThreshold is unset. It is just over twice the 24h
// IP-range refresh interval, so a single entirely-missed refresh (age peaks at
// ~2 intervals before the next success) does not trip the condition; only a
// genuinely stalled loop does (Q157).
const DefaultEgressStaleThreshold = 49 * time.Hour

// egressStale carries the computed EgressRulesStale condition (Q157).
type egressStale struct {
	stale   bool
	reason  string
	message string
}

// TenantNamespaceMarkerLabel is the label a trusted administrator must apply to a
// tenant namespace to mark it as managed by the GMC. The namespace-psa-guard
// ValidatingAdmissionPolicy (config/admission-policy/namespace-psa-guard.yaml)
// denies the GMC ServiceAccount any namespace patch unless the existing namespace
// already carries this label set to "true" — confining the GMC's cluster-wide
// namespaces:patch grant to managed tenants so a compromised GMC cannot relabel
// kube-system PSA (k8s best-practices audit finding B2 / Queue Q56). The GMC never
// sets this label itself; doing so would defeat the control.
const TenantNamespaceMarkerLabel = "actions-gateway.github.com/tenant"

// serviceMonitorGVK is the Prometheus-Operator ServiceMonitor GroupVersionKind.
// Per-tenant ServiceMonitors are built as unstructured objects so the GMC does
// not take a compile-time dependency on the prometheus-operator API module;
// the monitoring.coreos.com CRD is an optional, operator-installed prerequisite
// (see applyOrPruneServiceMonitors for the graceful CRD-absent handling).
var serviceMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "ServiceMonitor",
}
