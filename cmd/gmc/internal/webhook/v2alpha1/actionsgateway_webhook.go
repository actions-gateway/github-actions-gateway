package v2alpha1

import (
	"context"
	"fmt"
	"os"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-actions-gateway-com-v2alpha1-actionsgateway,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.com,resources=actionsgateways,verbs=create;update,versions=v2alpha1,name=vactionsgateway-v2alpha1.kb.io,admissionReviewVersions=v1

// ActionsGatewayCustomValidator validates the v2 (actions-gateway.com) ActionsGateway
// kind. It gates spec.scheduling.priorityClassName — the priority class the AGC
// control-plane pod runs at — against the infra-only PriorityClass allowlist (Q284),
// rejects a gitHubURL whose host falls in a bound EgressProxy's noProxyCIDRs
// (Q322 — the gateway is the object that binds the GitHub host, so a gateway write
// can assemble the GitHub-bypass pair from its side), rejects a structurally
// malformed gitHubURL, and rejects creation in a reserved namespace (both Q323 —
// the same guards the v1 webhook enforces, via the shared validation package).
//
// +kubebuilder:object:generate=false
type ActionsGatewayCustomValidator struct {
	// InfraPriorityClasses is the infra-only PriorityClass allowlist
	// (--allowed-infra-priority-classes, Q284), disjoint from the worker allowlist. A
	// nil allowlist forbids every named class (the secure default), so only an
	// empty/unset spec.scheduling.priorityClassName passes.
	InfraPriorityClasses *allowlist.PriorityClassAllowlist

	// reader resolves the EgressProxies this gateway's gitHubURL host is bound to —
	// via its own defaultProxyRef and via the proxyRef of every RunnerSet under it —
	// for the noProxyCIDRs GitHub-bypass guard (Q322, noproxy_referrers.go). It is
	// the manager's uncached API reader in production (wired by
	// SetupActionsGatewayWebhookWithManager). A nil reader disables the check
	// (direct-construction unit tests not exercising it).
	reader client.Reader

	// reservedNamespaces is the set of namespaces where a v2 ActionsGateway may
	// not be created (Q323): a gateway makes the GMC provision an AGC control
	// plane into its namespace, which must never land in kube-system/kube-public
	// or the GMC's own install namespace. Built by
	// SetupActionsGatewayWebhookWithManager from validation.ReservedNamespaces;
	// a nil set disables the guard (direct-construction unit tests not
	// exercising it).
	reservedNamespaces map[string]bool
}

// validateGitHubHostVsProxies rejects a gateway write whose gitHubURL host —
// a GitHub Enterprise Server host in particular — falls in the noProxyCIDRs of any
// EgressProxy the host is bound to: the gateway's own defaultProxyRef, plus the
// proxyRef of every RunnerSet targeting this gateway (those sets serve THIS
// gateway's GitHub host through THEIR proxy). The RunnerSet half closes the
// create-order gap: a RunnerSet+EgressProxy pair naming a not-yet-applied gateway
// admits unchecked on both sides (§H.7), so the arriving gateway is the first
// object that can see the conflict. gitHubURL itself is immutable (CRD CEL), but
// defaultProxyRef is not, so updates re-check too. Missing referents admit (§H.7);
// List errors fail closed.
func (v *ActionsGatewayCustomValidator) validateGitHubHostVsProxies(ctx context.Context, gw *agcv2alpha1.ActionsGateway) error {
	if v.reader == nil {
		return nil
	}
	host := gitHubURLHost(gw.Spec.GitHubURL)
	if host == "" {
		return nil
	}
	checked := map[string]bool{}
	if gw.Spec.DefaultProxyRef != nil {
		checked[gw.Spec.DefaultProxyRef.Name] = true
		if err := validateGitHubHostAgainstProxy(ctx, v.reader, gw.Namespace, gw.Spec.DefaultProxyRef.Name, "spec.defaultProxyRef", host); err != nil {
			return err
		}
	}
	var runnerSets agcv2alpha1.RunnerSetList
	if err := v.reader.List(ctx, &runnerSets, client.InNamespace(gw.Namespace)); err != nil {
		// Fail closed: admitting an unverifiable gitHubURL/noProxyCIDRs pair is the
		// bypass this guards against.
		return fmt.Errorf("cannot verify gitHubURL host %q against RunnerSet-referenced EgressProxies: %w", host, err)
	}
	for i := range runnerSets.Items {
		rs := &runnerSets.Items[i]
		if rs.Spec.GatewayRef.Name != gw.Name || rs.Spec.ProxyRef == nil || checked[rs.Spec.ProxyRef.Name] {
			continue
		}
		checked[rs.Spec.ProxyRef.Name] = true
		refField := fmt.Sprintf("spec.githubURL (EgressProxy referenced by RunnerSet %q)", rs.Name)
		if err := validateGitHubHostAgainstProxy(ctx, v.reader, gw.Namespace, rs.Spec.ProxyRef.Name, refField, host); err != nil {
			return err
		}
	}
	return nil
}

// ValidateCreate rejects a v2 ActionsGateway created in a reserved namespace, with a
// structurally malformed gitHubURL, whose spec.scheduling.priorityClassName is
// not on the infra allowlist, or whose gitHubURL host a bound EgressProxy's
// noProxyCIDRs would route around the proxy.
func (v *ActionsGatewayCustomValidator) ValidateCreate(ctx context.Context, obj *agcv2alpha1.ActionsGateway) (admission.Warnings, error) {
	if v.reservedNamespaces[obj.Namespace] {
		return nil, logRejection(ctx, "ActionsGateway", "create", obj.Namespace, obj.Name,
			fmt.Errorf("ActionsGateway may not be created in reserved namespace %q", obj.Namespace))
	}
	if err := validation.GitHubURL(obj.Spec.GitHubURL); err != nil {
		return nil, logRejection(ctx, "ActionsGateway", "create", obj.Namespace, obj.Name, err)
	}
	if err := validateSchedulingPriorityClass(obj.Spec.Scheduling, v.InfraPriorityClasses); err != nil {
		return nil, logRejection(ctx, "ActionsGateway", "create", obj.Namespace, obj.Name, err)
	}
	if err := v.validateGitHubHostVsProxies(ctx, obj); err != nil {
		return nil, logRejection(ctx, "ActionsGateway", "create", obj.Namespace, obj.Name, err)
	}
	return nil, nil
}

// ValidateUpdate applies the same gates on update, so an existing gateway cannot be
// edited to name an off-allowlist infra PriorityClass — or to point defaultProxyRef
// (gitHubURL itself is immutable) at a proxy whose noProxyCIDRs exclude its GitHub
// host. The reserved-namespace guard is create-only (namespace is immutable), and
// the structural gitHubURL check re-runs as version-agnostic defense even though
// the CRD's immutability CEL should make it unreachable on update.
func (v *ActionsGatewayCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *agcv2alpha1.ActionsGateway) (admission.Warnings, error) {
	if err := validation.GitHubURL(newObj.Spec.GitHubURL); err != nil {
		return nil, logRejection(ctx, "ActionsGateway", "update", newObj.Namespace, newObj.Name, err)
	}
	if err := validateSchedulingPriorityClass(newObj.Spec.Scheduling, v.InfraPriorityClasses); err != nil {
		return nil, logRejection(ctx, "ActionsGateway", "update", newObj.Namespace, newObj.Name, err)
	}
	if err := v.validateGitHubHostVsProxies(ctx, newObj); err != nil {
		return nil, logRejection(ctx, "ActionsGateway", "update", newObj.Namespace, newObj.Name, err)
	}
	return nil, nil
}

// ValidateDelete is a no-op.
func (v *ActionsGatewayCustomValidator) ValidateDelete(_ context.Context, _ *agcv2alpha1.ActionsGateway) (admission.Warnings, error) {
	return nil, nil
}

// SetupActionsGatewayWebhookWithManager registers the validating webhook for the v2
// ActionsGateway kind, wired to the infra-only PriorityClass allowlist (Q284), the
// manager's uncached API reader for the noProxyCIDRs GitHub-bypass guard (Q322), and
// the reserved-namespace set (Q323) — the GMC's own install namespace is read from
// the POD_NAMESPACE env var (populated by the Deployment via the downward API),
// matching the v1 webhook. The manager's scheme must already include agcv2alpha1
// (the GMC registers it at startup).
func SetupActionsGatewayWebhookWithManager(mgr ctrl.Manager, infraPriorityClasses *allowlist.PriorityClassAllowlist) error {
	v := &ActionsGatewayCustomValidator{
		InfraPriorityClasses: infraPriorityClasses,
		reader:               mgr.GetAPIReader(),
		reservedNamespaces:   validation.ReservedNamespaces(os.Getenv("POD_NAMESPACE")),
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &agcv2alpha1.ActionsGateway{}).
		WithValidator(v).
		Complete(); err != nil {
		return fmt.Errorf("register v2 ActionsGateway webhook: %w", err)
	}
	return nil
}
