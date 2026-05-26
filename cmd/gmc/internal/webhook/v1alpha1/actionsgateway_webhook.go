package v1alpha1

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
)

// defaultReservedNamespaces are namespaces in which an ActionsGateway CR is
// forbidden regardless of where the GMC is installed. `kube-system` and
// `kube-public` are universal; `gmc-system` is the default install namespace
// shipped by the project. Custom installs add their own namespace at setup
// time via the downward API (see SetupActionsGatewayWebhookWithManager).
var defaultReservedNamespaces = []string{
	"kube-system",
	"kube-public",
	"gmc-system",
}

// newReservedNamespaces returns the full set of forbidden namespaces. The
// defaults always apply; podNamespace is added when non-empty so that a
// non-default install (e.g. `actions-gateway-operator`) is also protected.
func newReservedNamespaces(podNamespace string) map[string]bool {
	s := make(map[string]bool, len(defaultReservedNamespaces)+1)
	for _, ns := range defaultReservedNamespaces {
		s[ns] = true
	}
	if podNamespace != "" {
		s[podNamespace] = true
	}
	return s
}

// SetupActionsGatewayWebhookWithManager registers the webhook for
// ActionsGateway in the manager. The GMC's own install namespace is read
// from the POD_NAMESPACE env var (which the Deployment populates via the
// downward API) and added to the reserved-namespace set so tenants cannot
// create an ActionsGateway in the operator's own namespace.
func SetupActionsGatewayWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &gmcv1alpha1.ActionsGateway{}).
		WithValidator(NewActionsGatewayCustomValidator(os.Getenv("POD_NAMESPACE"))).
		Complete()
}

// NewActionsGatewayCustomValidator returns a validator whose reserved-namespace
// set includes the universal Kubernetes reserved namespaces, the GMC's default
// install namespace, and the supplied podNamespace if non-empty. Tests use this
// to drive the reservation behavior without relying on the global environment.
func NewActionsGatewayCustomValidator(podNamespace string) *ActionsGatewayCustomValidator {
	return &ActionsGatewayCustomValidator{
		reservedNamespaces: newReservedNamespaces(podNamespace),
	}
}

// +kubebuilder:webhook:path=/validate-actions-gateway-github-com-v1alpha1-actionsgateway,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.github.com,resources=actionsgateways,verbs=create;update,versions=v1alpha1,name=vactionsgateway-v1alpha1.kb.io,admissionReviewVersions=v1

// ActionsGatewayCustomValidator validates ActionsGateway resources.
//
// +kubebuilder:object:generate=false
type ActionsGatewayCustomValidator struct {
	// reservedNamespaces is the set of namespaces where ActionsGateway CRs
	// are forbidden. Populated by NewActionsGatewayCustomValidator. If nil
	// (e.g. a test that constructs the struct directly), the reservation
	// check is a no-op — those tests are responsible for not relying on it.
	// Production paths go through the constructor.
	reservedNamespaces map[string]bool
}

// ValidateCreate rejects CRs created in reserved namespaces, with a cross-namespace
// gitHubAppRef, or with privileged containers.
func (v *ActionsGatewayCustomValidator) ValidateCreate(_ context.Context, obj *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	if v.reservedNamespaces[obj.Namespace] {
		return nil, fmt.Errorf("ActionsGateway may not be created in reserved namespace %q", obj.Namespace)
	}
	if err := validateGitHubAppRef(obj); err != nil {
		return nil, err
	}
	if err := validateRunnerGroups(obj); err != nil {
		return nil, err
	}
	return proxyResourceWarnings(obj), nil
}

// ValidateUpdate rejects updates that introduce a cross-namespace gitHubAppRef or privileged containers.
func (v *ActionsGatewayCustomValidator) ValidateUpdate(_ context.Context, _, newObj *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	if err := validateGitHubAppRef(newObj); err != nil {
		return nil, err
	}
	if err := validateRunnerGroups(newObj); err != nil {
		return nil, err
	}
	return proxyResourceWarnings(newObj), nil
}

// ValidateDelete is a no-op.
func (v *ActionsGatewayCustomValidator) ValidateDelete(_ context.Context, _ *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	return nil, nil
}

// validateGitHubAppRef rejects a non-empty gitHubAppRef.namespace. The field is
// ignored by the Secret lookup (which always uses the CR's own namespace), but it
// looks like a cross-namespace reference to users — a confused-deputy footgun.
// A CEL XValidation rule is not used here because k8s ≤ 1.30 CEL cannot apply
// has() to optional non-pointer string fields; the webhook is version-agnostic.
func validateGitHubAppRef(ag *gmcv1alpha1.ActionsGateway) error {
	if ag.Spec.GitHubAppRef.Namespace != "" {
		return fmt.Errorf("gitHubAppRef.namespace is not supported; the Secret must reside in the ActionsGateway's own namespace (got %q)", ag.Spec.GitHubAppRef.Namespace)
	}
	return nil
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

// proxyResourceWarnings returns a warning when proxy.resources.requests is set
// without a cpu key. The builder merges user values over defaults, so the
// default cpu request is preserved in the Deployment, but callers who expect
// their explicit requests map to be the authoritative source will be surprised.
// A warning surfaces the issue at apply time without blocking the operation.
func proxyResourceWarnings(ag *gmcv1alpha1.ActionsGateway) admission.Warnings {
	if ag.Spec.Proxy.Resources.Requests != nil {
		if _, hasCPU := ag.Spec.Proxy.Resources.Requests[corev1.ResourceCPU]; !hasCPU {
			return admission.Warnings{"proxy.resources.requests does not include cpu; " +
				"HPA requires a cpu request to compute utilization — autoscaling will not function if the default is later removed"}
		}
	}
	return nil
}

// isPrivileged returns true when the SecurityContext explicitly sets privileged: true.
func isPrivileged(sc *corev1.SecurityContext) bool {
	return sc != nil && sc.Privileged != nil && *sc.Privileged
}
