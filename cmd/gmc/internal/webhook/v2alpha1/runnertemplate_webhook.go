// Package v2alpha1 holds the GMC's validating admission webhooks for the v2alpha1
// (actions-gateway.com) data kinds. The GMC is the cluster-singleton operator, so
// it — not the per-tenant AGC — hosts cluster-wide admission for the whole v2 API
// surface, importing the RunnerTemplate types from the AGC api module.
package v2alpha1

import (
	"context"
	"fmt"
	"strings"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/validation"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// reservedProxyEnvNames are the worker-container environment variables the AGC
// injects as controller-enforced invariants when it provisions a worker pod
// (cmd/agc/internal/provisioner/provisioner.go: HTTP_PROXY/HTTPS_PROXY/NO_PROXY and
// the proxy-CA path). In v1 a template that set them was silently overwritten at
// pod-build time; v2 makes that an author-time rejection so the template fails
// closed instead of being rewritten behind the author's back (§H.4, §H.7). Matched
// case-insensitively: the standard proxy variables are honoured in both cases by
// Go's net/http proxy resolution, so a lowercase entry is the same footgun.
var reservedProxyEnvNames = map[string]struct{}{
	"http_proxy":         {},
	"https_proxy":        {},
	"no_proxy":           {},
	"proxy_ca_cert_path": {},
}

// validateReservedPodFields rejects the per-container reserved-pod-field violations
// that exceed the CRD CEL cost budget (an unbounded containers-array walk). The
// scalar pod-level reserved fields (serviceAccountName, host{PID,Network,IPC},
// automountServiceAccountToken) are enforced by the M1 CEL rules on the CRD and are
// not re-checked here.
//
//   - Reserved proxy env vars are rejected on every container and init container,
//     for both RunnerTemplate and ClusterRunnerTemplate.
//   - Privileged containers are rejected only when rejectPrivileged is true. The
//     namespaced RunnerTemplate sets it (a tenant must not self-author a privileged
//     worker shape); the cluster-scoped ClusterRunnerTemplate does not (it is
//     platform-authored — its purpose is golden privileged templates such as
//     DinD/sysbox, §H.4/§H.6). Pod Security Admission, stamped per the gateway's
//     securityProfile, remains the runtime enforcement backstop for both kinds — so
//     allowing privileged on the cluster-scoped kind is no weaker than v1.
func validateReservedPodFields(spec *agcv2alpha1.RunnerTemplateSpec, rejectPrivileged bool) error {
	check := func(containers []corev1.Container, isInit bool) error {
		label := "containers"
		if isInit {
			label = "initContainers"
		}
		for _, c := range containers {
			for _, e := range c.Env {
				if _, reserved := reservedProxyEnvNames[strings.ToLower(e.Name)]; reserved {
					return fmt.Errorf(
						"podTemplate.spec.%s[%q]: env %q is reserved: the AGC injects the egress-proxy variables (HTTP_PROXY/HTTPS_PROXY/NO_PROXY/PROXY_CA_CERT_PATH) into worker containers; setting it in a template is overridden and not permitted",
						label, c.Name, e.Name)
				}
			}
			if rejectPrivileged && isPrivileged(c.SecurityContext) {
				return fmt.Errorf(
					"podTemplate.spec.%s[%q]: privileged containers are not permitted in a namespaced RunnerTemplate; use a platform-owned ClusterRunnerTemplate for privileged (DinD/sysbox) worker shapes",
					label, c.Name)
			}
		}
		return nil
	}

	if err := check(spec.PodTemplate.Spec.Containers, false); err != nil {
		return err
	}
	return check(spec.PodTemplate.Spec.InitContainers, true)
}

// isPrivileged reports whether a container SecurityContext explicitly sets
// privileged: true. Mirrors the v1 webhook helper of the same name.
func isPrivileged(sc *corev1.SecurityContext) bool {
	return sc != nil && sc.Privileged != nil && *sc.Privileged
}

// validatePodTemplatePriorityClass rejects a namespaced RunnerTemplate whose
// podTemplate.spec.priorityClassName is not on the platform allowlist (Q289).
//
// podTemplate is a full corev1.PodTemplateSpec and the AGC copies it verbatim into
// the worker pod (provisioner.buildPod), overriding priorityClassName only when a
// priorityTiers tier matches. So this field is a SECOND, previously ungated route to
// the very escalation the Q132 allowlist exists to stop: a tenant naming a
// high-priority, preempting cluster-scoped PriorityClass has the scheduler evict
// OTHER tenants' running worker pods — and their egress proxies — to place its own.
//
// The escalation needs no setup on the tenant's part. Kubernetes ships
// system-cluster-critical (value 2000000000, preemptionPolicy PreemptLowerPriority)
// in every cluster, and — verified against a real apiserver, not assumed — nothing
// restricts it to kube-system: a pod naming it in a tenant namespace is admitted and
// resolves to that value. Gating the field here is what makes the Q132 guarantee
// ("a tenant cannot preempt another tenant") actually hold.
//
// The empty string means the pod names no PriorityClass and is always permitted, so
// the secure default (an unset --allowed-priority-classes flag) forbids every named
// class without forbidding ordinary unprioritized workers.
//
// Only the namespaced kind is gated. ClusterRunnerTemplate is cluster-scoped and
// therefore platform-authored — a tenant cannot create one — so the platform may name
// any class there, exactly as it may declare privileged containers there (§H.4/§H.6).
//
// This is a webhook check, not a CRD CEL rule, because the allowlist is dynamic
// platform config a spec-scoped CEL XValidation cannot read.
func validatePodTemplatePriorityClass(spec *agcv2alpha1.RunnerTemplateSpec, list *allowlist.PriorityClassAllowlist) error {
	name := spec.PodTemplate.Spec.PriorityClassName
	if list.AllowedPodPriorityClass(name) {
		return nil
	}
	return fmt.Errorf(
		"podTemplate.spec.priorityClassName: %q is not in the platform allowlist %v; "+
			"a PriorityClass sets the scheduler's preemption order across the whole cluster, so the platform admin must "+
			"pre-create it and add it to the GMC --allowed-priority-classes flag or the watched PriorityClass allowlist ConfigMap",
		name, list.Names())
}

// reapBlockingSidecarWarnings returns a NON-BLOCKING admission warning when a
// template carries a regular (non-native) sidecar container that may keep the worker
// pod alive after the runner container exits, stranding the runner slot (Q249, the
// Q247 stranding class). It is deliberately a warning, never a rejection: the
// heuristic is undecidable (nothing in a pod spec says a container "runs forever"), so
// a hard block would punish legitimate self-exiting sidecars. The
// SelfExitingSidecarsAnnotation opt-out silences it per named sidecar. Returns nil
// when there is nothing to warn about, so admission proceeds cleanly.
func reapBlockingSidecarWarnings(spec *agcv2alpha1.RunnerTemplateSpec, annotations map[string]string) admission.Warnings {
	names := agcv2alpha1.ReapBlockingSidecars(spec, annotations)
	if len(names) == 0 {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(
		"worker pod sidecar container(s) %s are regular containers, not native sidecars: a regular container that runs past the job (e.g. a DinD dockerd) keeps the worker pod from reaping, so the runner slot counts against maxWorkers and the pool can strand. Declare them as native sidecars (restartPolicy: Always init containers, Kubernetes >= 1.29) so the pod terminates when the runner exits, or — if they exit cleanly on their own — acknowledge them in the %s annotation to silence this warning.",
		strings.Join(names, ", "), agcv2alpha1.SelfExitingSidecarsAnnotation)}
}

// logRejection records a server-side audit line whenever an admission request is
// denied, mirroring the v1 ActionsGateway webhook. Denials are rare and
// security-relevant, so the trail is logged at Info. The error text is a validation
// message (container/env names) and never carries Secret contents.
func logRejection(ctx context.Context, kind, op, namespace, name string, err error) error {
	logf.FromContext(ctx).Info(kind+" admission denied",
		"operation", op,
		"namespace", namespace,
		"name", name,
		"reason", err.Error())
	return err
}

// +kubebuilder:webhook:path=/validate-actions-gateway-com-v2alpha1-runnertemplate,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.com,resources=runnertemplates,verbs=create;update,versions=v2alpha1,name=vrunnertemplate-v2alpha1.kb.io,admissionReviewVersions=v1

// RunnerTemplateCustomValidator validates the namespaced RunnerTemplate data kind.
// It rejects the reserved per-container pod fields, including privileged containers,
// and gates podTemplate.spec.priorityClassName against the platform allowlist (Q289).
//
// +kubebuilder:object:generate=false
type RunnerTemplateCustomValidator struct {
	// PriorityClasses is the platform allowlist of cluster-scoped PriorityClass names
	// a tenant may name on a worker pod. A nil allowlist forbids every named class
	// (the secure default), matching the v1 ActionsGateway validator.
	PriorityClasses *allowlist.PriorityClassAllowlist
}

// ValidateCreate rejects a RunnerTemplate carrying reserved pod fields or an
// off-allowlist priorityClassName, and emits a non-blocking reap-blocking-sidecar
// warning (Q249).
func (v *RunnerTemplateCustomValidator) ValidateCreate(ctx context.Context, obj *agcv2alpha1.RunnerTemplate) (admission.Warnings, error) {
	if err := v.validate(&obj.Spec); err != nil {
		return nil, logRejection(ctx, "RunnerTemplate", "create", obj.Namespace, obj.Name, err)
	}
	return reapBlockingSidecarWarnings(&obj.Spec, obj.Annotations), nil
}

// ValidateUpdate applies the same checks on update, so an existing RunnerTemplate
// cannot be edited to smuggle in a reserved field or an off-allowlist PriorityClass.
// Deletion-only updates — deletionTimestamp set, spec unchanged — are admitted
// without re-validation (Q518; see validation.DeletionOnlyUpdate).
func (v *RunnerTemplateCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *agcv2alpha1.RunnerTemplate) (admission.Warnings, error) {
	if validation.DeletionOnlyUpdate(newObj, oldObj.Spec, newObj.Spec) {
		return nil, nil
	}
	if err := v.validate(&newObj.Spec); err != nil {
		return nil, logRejection(ctx, "RunnerTemplate", "update", newObj.Namespace, newObj.Name, err)
	}
	return reapBlockingSidecarWarnings(&newObj.Spec, newObj.Annotations), nil
}

// validate runs the shared create/update gates for the namespaced kind.
func (v *RunnerTemplateCustomValidator) validate(spec *agcv2alpha1.RunnerTemplateSpec) error {
	if err := validateReservedPodFields(spec, true); err != nil {
		return err
	}
	return validatePodTemplatePriorityClass(spec, v.PriorityClasses)
}

// ValidateDelete is a no-op.
func (v *RunnerTemplateCustomValidator) ValidateDelete(_ context.Context, _ *agcv2alpha1.RunnerTemplate) (admission.Warnings, error) {
	return nil, nil
}

// +kubebuilder:webhook:path=/validate-actions-gateway-com-v2alpha1-clusterrunnertemplate,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.com,resources=clusterrunnertemplates,verbs=create;update,versions=v2alpha1,name=vclusterrunnertemplate-v2alpha1.kb.io,admissionReviewVersions=v1

// ClusterRunnerTemplateCustomValidator validates the cluster-scoped
// ClusterRunnerTemplate. It rejects the reserved proxy env vars but ALLOWS
// privileged containers AND any podTemplate.spec.priorityClassName: the cluster-scoped
// kind is platform-authored (a tenant cannot create cluster-scoped objects), and its
// documented purpose is golden privileged templates. Gating the PriorityClass here
// would only constrain the platform against itself — the allowlist exists to stop a
// TENANT naming a preempting class (Q132/Q289). PSA remains the runtime backstop.
//
// +kubebuilder:object:generate=false
type ClusterRunnerTemplateCustomValidator struct{}

// ValidateCreate rejects a ClusterRunnerTemplate carrying reserved proxy env vars and
// emits a non-blocking reap-blocking-sidecar warning (Q249).
func (v *ClusterRunnerTemplateCustomValidator) ValidateCreate(ctx context.Context, obj *agcv2alpha1.ClusterRunnerTemplate) (admission.Warnings, error) {
	if err := validateReservedPodFields(&obj.Spec, false); err != nil {
		return nil, logRejection(ctx, "ClusterRunnerTemplate", "create", obj.Namespace, obj.Name, err)
	}
	return reapBlockingSidecarWarnings(&obj.Spec, obj.Annotations), nil
}

// ValidateUpdate applies the same checks and reap-blocking-sidecar warning on update.
func (v *ClusterRunnerTemplateCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *agcv2alpha1.ClusterRunnerTemplate) (admission.Warnings, error) {
	if err := validateReservedPodFields(&newObj.Spec, false); err != nil {
		return nil, logRejection(ctx, "ClusterRunnerTemplate", "update", newObj.Namespace, newObj.Name, err)
	}
	return reapBlockingSidecarWarnings(&newObj.Spec, newObj.Annotations), nil
}

// ValidateDelete is a no-op.
func (v *ClusterRunnerTemplateCustomValidator) ValidateDelete(_ context.Context, _ *agcv2alpha1.ClusterRunnerTemplate) (admission.Warnings, error) {
	return nil, nil
}

// SetupRunnerTemplateWebhooksWithManager registers the validating webhooks for both
// the namespaced RunnerTemplate and the cluster-scoped ClusterRunnerTemplate. The
// manager's scheme must already include agcv2alpha1 (the GMC registers it at
// startup). priorityClasses is the shared platform PriorityClass allowlist the
// namespaced kind's podTemplate is gated against (Q289); nil forbids every named
// class, the secure default.
func SetupRunnerTemplateWebhooksWithManager(mgr ctrl.Manager, priorityClasses *allowlist.PriorityClassAllowlist) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &agcv2alpha1.RunnerTemplate{}).
		WithValidator(&RunnerTemplateCustomValidator{PriorityClasses: priorityClasses}).
		Complete(); err != nil {
		return fmt.Errorf("register RunnerTemplate webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &agcv2alpha1.ClusterRunnerTemplate{}).
		WithValidator(&ClusterRunnerTemplateCustomValidator{}).
		Complete(); err != nil {
		return fmt.Errorf("register ClusterRunnerTemplate webhook: %w", err)
	}
	return nil
}
