package v1alpha1

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
)

// reservedNamespaces lists namespaces where ActionsGateway CRs are forbidden.
var reservedNamespaces = map[string]bool{
	"kube-system":              true,
	"kube-public":              true,
	"actions-gateway-system":   true,
}

// SetupActionsGatewayWebhookWithManager registers the webhook for ActionsGateway in the manager.
func SetupActionsGatewayWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &gmcv1alpha1.ActionsGateway{}).
		WithValidator(&ActionsGatewayCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-actions-gateway-github-com-v1alpha1-actionsgateway,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.github.com,resources=actionsgateways,verbs=create;update,versions=v1alpha1,name=vactionsgateway-v1alpha1.kb.io,admissionReviewVersions=v1

// ActionsGatewayCustomValidator validates ActionsGateway resources.
//
// +kubebuilder:object:generate=false
type ActionsGatewayCustomValidator struct{}

// ValidateCreate rejects CRs created in reserved namespaces and with privileged containers.
func (v *ActionsGatewayCustomValidator) ValidateCreate(_ context.Context, obj *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	if reservedNamespaces[obj.Namespace] {
		return nil, fmt.Errorf("ActionsGateway may not be created in reserved namespace %q", obj.Namespace)
	}
	if err := validateRunnerGroups(obj); err != nil {
		return nil, err
	}
	return nil, nil
}

// ValidateUpdate rejects updates that introduce privileged containers.
func (v *ActionsGatewayCustomValidator) ValidateUpdate(_ context.Context, _, newObj *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	if err := validateRunnerGroups(newObj); err != nil {
		return nil, err
	}
	return nil, nil
}

// ValidateDelete is a no-op.
func (v *ActionsGatewayCustomValidator) ValidateDelete(_ context.Context, _ *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	return nil, nil
}

// validateRunnerGroups rejects privileged containers in any RunnerGroup's PodTemplate.
// This check was previously expressed as a CEL x-kubernetes-validations rule on the CRD, but
// iterating over an unbounded corev1.PodTemplateSpec.containers array exceeds the k8s 1.35
// CEL cost budget. The admission webhook is the correct place for this validation.
func validateRunnerGroups(ag *gmcv1alpha1.ActionsGateway) error {
	for i, rg := range ag.Spec.RunnerGroups {
		for _, c := range rg.PodTemplate.Spec.Containers {
			if isPrivileged(c.SecurityContext) {
				return fmt.Errorf("runnerGroups[%d]: privileged containers are not permitted in worker pods (container %q)", i, c.Name)
			}
		}
		for _, c := range rg.PodTemplate.Spec.InitContainers {
			if isPrivileged(c.SecurityContext) {
				return fmt.Errorf("runnerGroups[%d]: privileged init containers are not permitted in worker pods (container %q)", i, c.Name)
			}
		}
	}
	return nil
}

// isPrivileged returns true when the SecurityContext explicitly sets privileged: true.
func isPrivileged(sc *corev1.SecurityContext) bool {
	return sc != nil && sc.Privileged != nil && *sc.Privileged
}
