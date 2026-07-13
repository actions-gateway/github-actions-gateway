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

package v2alpha1

import (
	"context"
	"fmt"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-actions-gateway-com-v2alpha1-actionsgateway,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.com,resources=actionsgateways,verbs=create;update,versions=v2alpha1,name=vactionsgateway-v2alpha1.kb.io,admissionReviewVersions=v1

// ActionsGatewayCustomValidator validates the v2 (actions-gateway.com) ActionsGateway
// kind. It gates spec.scheduling.priorityClassName — the priority class the AGC
// control-plane pod runs at — against the infra-only PriorityClass allowlist (Q284).
//
// This is a NEW webhook, not an extension of the v1alpha1 ActionsGateway webhook: that
// one lives in a different API group (actions-gateway.github.com) and the v1 kind has no
// spec.scheduling block at all. The v2 kind is where spec.scheduling lives, so its
// priorityClassName needs its own validating webhook — the piece Q284 stood up from
// scratch (there was no v2 ActionsGateway validating webhook before).
//
// +kubebuilder:object:generate=false
type ActionsGatewayCustomValidator struct {
	// InfraPriorityClasses is the infra-only PriorityClass allowlist
	// (--allowed-infra-priority-classes, Q284), disjoint from the worker allowlist. A
	// nil allowlist forbids every named class (the secure default), so only an
	// empty/unset spec.scheduling.priorityClassName passes.
	InfraPriorityClasses *allowlist.PriorityClassAllowlist
}

// ValidateCreate rejects a v2 ActionsGateway whose spec.scheduling.priorityClassName is
// not on the infra allowlist.
func (v *ActionsGatewayCustomValidator) ValidateCreate(ctx context.Context, obj *agcv2alpha1.ActionsGateway) (admission.Warnings, error) {
	if err := validateSchedulingPriorityClass(obj.Spec.Scheduling, v.InfraPriorityClasses); err != nil {
		return nil, logRejection(ctx, "ActionsGateway", "create", obj.Namespace, obj.Name, err)
	}
	return nil, nil
}

// ValidateUpdate applies the same gate on update, so an existing gateway cannot be
// edited to name an off-allowlist infra PriorityClass.
func (v *ActionsGatewayCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *agcv2alpha1.ActionsGateway) (admission.Warnings, error) {
	if err := validateSchedulingPriorityClass(newObj.Spec.Scheduling, v.InfraPriorityClasses); err != nil {
		return nil, logRejection(ctx, "ActionsGateway", "update", newObj.Namespace, newObj.Name, err)
	}
	return nil, nil
}

// ValidateDelete is a no-op.
func (v *ActionsGatewayCustomValidator) ValidateDelete(_ context.Context, _ *agcv2alpha1.ActionsGateway) (admission.Warnings, error) {
	return nil, nil
}

// SetupActionsGatewayWebhookWithManager registers the validating webhook for the v2
// ActionsGateway kind, wired to the infra-only PriorityClass allowlist (Q284). The
// manager's scheme must already include agcv2alpha1 (the GMC registers it at startup).
func SetupActionsGatewayWebhookWithManager(mgr ctrl.Manager, infraPriorityClasses *allowlist.PriorityClassAllowlist) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &agcv2alpha1.ActionsGateway{}).
		WithValidator(&ActionsGatewayCustomValidator{InfraPriorityClasses: infraPriorityClasses}).
		Complete(); err != nil {
		return fmt.Errorf("register v2 ActionsGateway webhook: %w", err)
	}
	return nil
}
