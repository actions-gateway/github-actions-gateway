// Package v2beta1 wires the GMC-hosted conversion webhook for the actions-gateway.com
// v2 hub kinds (Q74). The conversion webhook is GMC-hosted for the same reason the v2
// validating webhooks are: all v2 admission runs off the single GMC webhook server,
// even for the AGC-reconciled RunnerSet / RunnerTemplate kinds.
package v2beta1

import (
	"fmt"

	apiv2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupConversionWebhooksWithManager registers the conversion webhook for the five v2
// hub kinds (ActionsGateway, EgressProxy, RunnerSet, RunnerTemplate,
// ClusterRunnerTemplate). controller-runtime serves one shared /convert endpoint for
// every Convertible/Hub type in the manager's scheme, so each
// NewWebhookManagedBy(...).Complete() call registers that same handler idempotently
// (the builder's isAlreadyHandled guard). The five explicit calls assert, at wiring
// time, that each hub kind is genuinely convertible in the scheme — i.e. both its
// v2beta1 hub and its v2alpha1 spoke are registered — rather than silently serving a
// half-wired /convert. The manager's scheme must already include both api/v2alpha1
// and api/v2beta1 (the GMC registers both at startup).
func SetupConversionWebhooksWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.ActionsGateway{}).Complete(); err != nil {
		return fmt.Errorf("register ActionsGateway conversion webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.EgressProxy{}).Complete(); err != nil {
		return fmt.Errorf("register EgressProxy conversion webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.RunnerSet{}).Complete(); err != nil {
		return fmt.Errorf("register RunnerSet conversion webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.RunnerTemplate{}).Complete(); err != nil {
		return fmt.Errorf("register RunnerTemplate conversion webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.ClusterRunnerTemplate{}).Complete(); err != nil {
		return fmt.Errorf("register ClusterRunnerTemplate conversion webhook: %w", err)
	}
	return nil
}
