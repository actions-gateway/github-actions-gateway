package v1alpha1

import (
	"context"
	"fmt"

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

// ValidateCreate rejects CRs created in reserved namespaces.
func (v *ActionsGatewayCustomValidator) ValidateCreate(_ context.Context, obj *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	if reservedNamespaces[obj.Namespace] {
		return nil, fmt.Errorf("ActionsGateway may not be created in reserved namespace %q", obj.Namespace)
	}
	return nil, nil
}

// ValidateUpdate is a no-op (namespace cannot change on update).
func (v *ActionsGatewayCustomValidator) ValidateUpdate(_ context.Context, _, _ *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete is a no-op.
func (v *ActionsGatewayCustomValidator) ValidateDelete(_ context.Context, _ *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	return nil, nil
}
