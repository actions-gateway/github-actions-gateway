/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package v2alpha1 holds the GMC's validating admission webhooks for the v2alpha1
// (actions-gateway.com) data kinds. The GMC is the cluster-singleton operator, so
// it — not the per-tenant AGC — hosts cluster-wide admission for the whole v2 API
// surface, importing the RunnerTemplate types from the AGC api module.
package v2alpha1

import (
	"context"
	"fmt"
	"strings"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v2alpha1"
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
	check := func(kind string, containers []corev1.Container, isInit bool) error {
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
					"podTemplate.spec.%s[%q]: privileged containers are not permitted in a namespaced %s; use a platform-owned ClusterRunnerTemplate for privileged (DinD/sysbox) worker shapes",
					label, c.Name, kind)
			}
		}
		return nil
	}

	if err := check("RunnerTemplate", spec.PodTemplate.Spec.Containers, false); err != nil {
		return err
	}
	return check("RunnerTemplate", spec.PodTemplate.Spec.InitContainers, true)
}

// isPrivileged reports whether a container SecurityContext explicitly sets
// privileged: true. Mirrors the v1 webhook helper of the same name.
func isPrivileged(sc *corev1.SecurityContext) bool {
	return sc != nil && sc.Privileged != nil && *sc.Privileged
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
// It rejects the reserved per-container pod fields, including privileged containers.
//
// +kubebuilder:object:generate=false
type RunnerTemplateCustomValidator struct{}

// ValidateCreate rejects a RunnerTemplate carrying reserved pod fields.
func (v *RunnerTemplateCustomValidator) ValidateCreate(ctx context.Context, obj *agcv2alpha1.RunnerTemplate) (admission.Warnings, error) {
	if err := validateReservedPodFields(&obj.Spec, true); err != nil {
		return nil, logRejection(ctx, "RunnerTemplate", "create", obj.Namespace, obj.Name, err)
	}
	return nil, nil
}

// ValidateUpdate applies the same reserved-pod-field checks on update.
func (v *RunnerTemplateCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *agcv2alpha1.RunnerTemplate) (admission.Warnings, error) {
	if err := validateReservedPodFields(&newObj.Spec, true); err != nil {
		return nil, logRejection(ctx, "RunnerTemplate", "update", newObj.Namespace, newObj.Name, err)
	}
	return nil, nil
}

// ValidateDelete is a no-op.
func (v *RunnerTemplateCustomValidator) ValidateDelete(_ context.Context, _ *agcv2alpha1.RunnerTemplate) (admission.Warnings, error) {
	return nil, nil
}

// +kubebuilder:webhook:path=/validate-actions-gateway-com-v2alpha1-clusterrunnertemplate,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.com,resources=clusterrunnertemplates,verbs=create;update,versions=v2alpha1,name=vclusterrunnertemplate-v2alpha1.kb.io,admissionReviewVersions=v1

// ClusterRunnerTemplateCustomValidator validates the cluster-scoped
// ClusterRunnerTemplate. It rejects the reserved proxy env vars but ALLOWS
// privileged containers: the cluster-scoped kind is platform-authored (a tenant
// cannot create cluster-scoped objects), and its documented purpose is golden
// privileged templates. PSA remains the runtime backstop.
//
// +kubebuilder:object:generate=false
type ClusterRunnerTemplateCustomValidator struct{}

// ValidateCreate rejects a ClusterRunnerTemplate carrying reserved proxy env vars.
func (v *ClusterRunnerTemplateCustomValidator) ValidateCreate(ctx context.Context, obj *agcv2alpha1.ClusterRunnerTemplate) (admission.Warnings, error) {
	if err := validateReservedPodFields(&obj.Spec, false); err != nil {
		return nil, logRejection(ctx, "ClusterRunnerTemplate", "create", obj.Namespace, obj.Name, err)
	}
	return nil, nil
}

// ValidateUpdate applies the same checks on update.
func (v *ClusterRunnerTemplateCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *agcv2alpha1.ClusterRunnerTemplate) (admission.Warnings, error) {
	if err := validateReservedPodFields(&newObj.Spec, false); err != nil {
		return nil, logRejection(ctx, "ClusterRunnerTemplate", "update", newObj.Namespace, newObj.Name, err)
	}
	return nil, nil
}

// ValidateDelete is a no-op.
func (v *ClusterRunnerTemplateCustomValidator) ValidateDelete(_ context.Context, _ *agcv2alpha1.ClusterRunnerTemplate) (admission.Warnings, error) {
	return nil, nil
}

// SetupRunnerTemplateWebhooksWithManager registers the validating webhooks for both
// the namespaced RunnerTemplate and the cluster-scoped ClusterRunnerTemplate. The
// manager's scheme must already include agcv2alpha1 (the GMC registers it at
// startup).
func SetupRunnerTemplateWebhooksWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &agcv2alpha1.RunnerTemplate{}).
		WithValidator(&RunnerTemplateCustomValidator{}).
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
