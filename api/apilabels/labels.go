// Package apilabels centralises the Kubernetes recommended ("well-known")
// app.kubernetes.io/* label set that the GMC and AGC stamp on every object they
// create — AGC/proxy/worker Pods, Deployments, Services, NetworkPolicies,
// ServiceAccounts, RBAC, Secrets, and the per-tenant CRs.
//
// It lives in the neutral api module so both controllers share one definition of
// the keys and the part-of value: a divergence between the GMC's and AGC's idea of
// these labels would silently break operator tooling (Lens/k9s/Argo grouping,
// Prometheus relabel rules, OpenCost cost attribution) that keys on them.
//
// These labels are ADDITIVE metadata. They are never used as a controller's
// functional pod/Service selector — those keep their existing project-specific
// keys (app:, actions-gateway/component: workload, …). Callers merge the
// recommended set alongside, never in place of, their selector labels.
package apilabels

// The Kubernetes recommended-label keys. See
// https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/.
const (
	// Name is app.kubernetes.io/name — the name of the application (the workload
	// kind), e.g. actions-gateway-controller, actions-gateway-proxy, actions-runner.
	Name = "app.kubernetes.io/name"
	// Instance is app.kubernetes.io/instance — a unique name identifying this
	// instance of the application, e.g. the ActionsGateway / EgressProxy / RunnerGroup
	// name. Groups all objects of one tenant deployment together.
	Instance = "app.kubernetes.io/instance"
	// Component is app.kubernetes.io/component — the role within the architecture:
	// controller, proxy, or runner.
	Component = "app.kubernetes.io/component"
	// PartOf is app.kubernetes.io/part-of — the higher-level application every GAG
	// object belongs to (PartOfValue). Lets tooling group the whole system.
	PartOf = "app.kubernetes.io/part-of"
	// ManagedBy is app.kubernetes.io/managed-by — the tool managing the object: the
	// GMC for control-plane children, the AGC for worker pods and job Secrets.
	ManagedBy = "app.kubernetes.io/managed-by"
	// Version is app.kubernetes.io/version — the version of the application. Set only
	// where a meaningful version exists (the runner version on worker pods/Secrets);
	// omitted on versionless infra objects (RBAC, NetworkPolicies, Services, TLS
	// Secrets) and where no build version is plumbed to the controller.
	Version = "app.kubernetes.io/version"
)

// PartOfValue is the constant app.kubernetes.io/part-of value carried by every GAG
// object — the project name, so `kubectl get all -l app.kubernetes.io/part-of=actions-gateway`
// (and Lens/k9s/Argo grouping) selects the whole system across tenants and tiers.
const PartOfValue = "actions-gateway"

// Recommended returns a fresh map of the app.kubernetes.io/* recommended-label set.
// version is omitted from the map when empty (versionless infra objects), so a
// caller with no meaningful version simply passes "".
func Recommended(name, instance, component, version, managedBy string) map[string]string {
	l := map[string]string{
		Name:      name,
		Instance:  instance,
		Component: component,
		PartOf:    PartOfValue,
		ManagedBy: managedBy,
	}
	if version != "" {
		l[Version] = version
	}
	return l
}

// Merge stamps the app.kubernetes.io/* recommended set onto dst — a label map that
// already carries the object's functional/selector labels — and returns dst. An
// existing key in dst is never overwritten: the recommended labels are additive
// metadata and must not clobber a functional selector label a controller relies on.
// A nil dst is allocated.
func Merge(dst map[string]string, name, instance, component, version, managedBy string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range Recommended(name, instance, component, version, managedBy) {
		if _, ok := dst[k]; !ok {
			dst[k] = v
		}
	}
	return dst
}
