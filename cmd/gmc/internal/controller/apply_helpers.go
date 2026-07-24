package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// applyManagedChild is the single CreateOrPatch code path shared by every GMC
// child-object apply* helper across the three reconcilers (v1 ActionsGateway, v2
// ActionsGateway, and EgressProxy). It replaces ~26 near-identical wrappers that
// differed only by Kubernetes type, which controller-managed fields they wrote,
// and whether they stamped a controller owner reference.
//
// It keys obj by desired's namespace and name, then creates or patches the live
// object: it writes the controller-managed labels, invokes copyManaged to copy
// the type-specific managed fields (nil for a labels-only child), and — only when
// owner is non-nil — stamps a controller owner reference via
// controllerutil.SetControllerReference. obj must be an empty shell of desired's
// concrete type (its namespace/name/labels are overwritten from desired); for an
// unstructured child the caller sets the GVK on the shell before calling.
//
// The owner argument is load-bearing and per-call-site: it selects whether the
// child is reclaimed by Kubernetes cascade garbage collection (owner set) or only
// by the reconciler's reconcileDelete finalizer (owner nil). The v1 ActionsGateway
// reconciler deliberately leaves several children un-owned and relies on the
// finalizer; passing a non-nil owner there — or dropping the owner on a currently
// owned child — silently changes garbage-collection semantics and, for the
// un-owned children, would let a force-removed finalizer leak them. The current
// per-child policy is intentionally NOT normalised here; it is documented and
// tracked for a separate, deliberately-reviewed decision (Q394). Preserve each
// call site's owner argument exactly; the apply_helpers ownerRef-contract tests
// pin it.
func applyManagedChild[T client.Object](
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	obj T,
	desired T,
	copyManaged func() error,
) error {
	obj.SetNamespace(desired.GetNamespace())
	obj.SetName(desired.GetName())
	_, err := controllerutil.CreateOrPatch(ctx, c, obj, func() error {
		obj.SetLabels(desired.GetLabels())
		if copyManaged != nil {
			if err := copyManaged(); err != nil {
				return err
			}
		}
		if owner != nil {
			return controllerutil.SetControllerReference(owner, obj, scheme)
		}
		return nil
	})
	return err
}
