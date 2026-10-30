package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// applyManagedChild is the single CreateOrPatch code path shared by every GMC
// child-object apply* helper across the three reconcilers (v1 ActionsGateway, v2
// ActionsGateway, and EgressProxy).
//
// It keys obj by desired's namespace and name, then creates or patches the live
// object: it merges the controller-managed labels over the live ones (see
// mergeLabels), invokes copyManaged to copy the type-specific managed fields (nil
// for a labels-only child), and — only when owner is non-nil — stamps a controller
// owner reference via controllerutil.SetControllerReference. obj must be an empty
// shell of desired's concrete type (its namespace/name are set from desired); for an
// unstructured child the caller sets the GVK on the shell before calling.
//
// The owner argument is load-bearing: it selects whether the child is also
// reclaimed by Kubernetes cascade garbage collection (owner set) or only by the
// reconciler's reconcileDelete finalizer (owner nil). The policy across all three
// reconcilers is now uniform (Q394): **every namespaced child passes a non-nil
// owner**, so GC backstops the finalizer and a force-removed finalizer cannot
// leak children. Only a cluster-scoped child passes nil — a namespaced
// ActionsGateway cannot own one (the apiserver rejects the cross-scope reference
// and never collects it), so reconcileDelete removes it explicitly. Today that is
// exactly one call site, applyClusterRunnerTemplateReaderBinding. Dropping the
// owner on a namespaced child re-opens the leak; the apply_helpers
// ownerRef-contract tests pin the per-helper contract.
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
		mergeLabels(obj, desired.GetLabels())
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

// applyManagedSecret is applyManagedChild for a Secret the owner controls, writing
// its labels, type and data. It reads then updates rather than patching, because
// the GMC holds no patch on Secrets: a PATCH response carries the stored object, so
// the verb would read any Secret admission lets through, beyond the tenant
// namespaces its read binding covers (Q1188, Q1214). The update carries the read's
// resourceVersion, so a concurrent writer conflicts and the reconcile retries.
func applyManagedSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, desired *corev1.Secret) error {
	obj := &corev1.Secret{}
	obj.SetNamespace(desired.Namespace)
	obj.SetName(desired.Name)
	_, err := controllerutil.CreateOrUpdate(ctx, c, obj, func() error {
		mergeLabels(obj, desired.Labels)
		obj.Type = desired.Type
		obj.Data = desired.Data
		return controllerutil.SetControllerReference(owner, obj, scheme)
	})
	return err
}

// mergeLabels writes desired over obj's live labels, leaving every key the GMC does
// not set untouched, so a label another controller or an operator added survives
// the apply (Q1215). A key the GMC stops setting is therefore never removed; no
// child has ever dropped one, and a removal would need a tracked owned-key set.
func mergeLabels(obj client.Object, desired map[string]string) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string, len(desired))
	}
	for k, v := range desired {
		labels[k] = v
	}
	obj.SetLabels(labels)
}
