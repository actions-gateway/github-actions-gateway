package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

// StorageVersion is the version every actions-gateway.com CustomResourceDefinition
// stores from 1.10 (Q1086). v2.0.0 removes the other served versions, which the
// apiserver refuses while a CRD's status.storedVersions still lists one.
const StorageVersion = "v2"

const storageGroup = "actions-gateway.com"

// StorageKind is one actions-gateway.com CustomResourceDefinition the storage sweep
// rewrites.
type StorageKind struct {
	Resource string // plural, the CRD name's first label
	Kind     string
}

// CRDName is the CustomResourceDefinition's metadata.name.
func (k StorageKind) CRDName() string { return k.Resource + "." + storageGroup }

// StorageKinds is every actions-gateway.com CRD: the five in the
// actions-gateway-crds-v2 chart and PriorityClassAllowlist in the main chart.
var StorageKinds = []StorageKind{
	{Resource: "actionsgateways", Kind: "ActionsGateway"},
	{Resource: "egressproxies", Kind: "EgressProxy"},
	{Resource: "runnersets", Kind: "RunnerSet"},
	{Resource: "runnertemplates", Kind: "RunnerTemplate"},
	{Resource: "clusterrunnertemplates", Kind: "ClusterRunnerTemplate"},
	{Resource: "priorityclassallowlists", Kind: "PriorityClassAllowlist"},
}

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// ErrStoragePrecondition reports a cluster the sweep must not run against yet. It
// is returned before any object is written.
var ErrStoragePrecondition = errors.New("storage-version sweep precondition not met")

// SweepOptions configures SweepStorageVersion.
type SweepOptions struct {
	// Apply rewrites objects and prunes storedVersions. False reports what Apply
	// would do and writes nothing.
	Apply bool
	// Out receives one progress line per kind.
	Out io.Writer
	// Retry wraps every write, so the CLI can ride out an admission webhook that is
	// briefly unreachable. Nil runs each write once.
	Retry func(what string, op func() error) error
	// PageSize bounds each LIST. Zero means 500.
	PageSize int64
}

// KindReport is the sweep's reading for one CRD.
type KindReport struct {
	CRD string
	// StoredBefore and StoredAfter are status.storedVersions as read before the
	// sweep and after the prune. StoredAfter is empty on a dry run, and on a kind
	// the sweep did not prune because an object failed.
	StoredBefore []string
	StoredAfter  []string
	Objects      int
	Rewritten    int
	// Failed names each object, as namespace/name, the apiserver refused to rewrite.
	Failed []string
}

// Pruned reports whether storedVersions reads [StorageVersion] after the sweep.
func (r KindReport) Pruned() bool { return slices.Equal(r.StoredAfter, []string{StorageVersion}) }

// SweepStorageVersion rewrites every stored object of each StorageKinds CRD at
// StorageVersion, then prunes the CRD's status.storedVersions to [StorageVersion].
//
// A write the apiserver accepts is re-encoded at the CRD's storage version even when
// nothing in the object changed, because the bytes it persists then differ from the
// bytes it read; an object already stored at StorageVersion is skipped by the
// apiserver as a no-op. A kind is pruned only when every one of its objects was
// rewritten, so a refused write leaves its CRD listing the old version, and the error
// names the object.
//
// Two preconditions are checked before anything is written: every CRD already marks
// StorageVersion as its storage version (the 1.10 CRDs are applied), and no
// EgressProxy names a CiliumFQDN/CalicoFQDN alias, which StorageVersion cannot
// represent (Q1085).
func SweepStorageVersion(ctx context.Context, c client.Client, opts SweepOptions) ([]KindReport, error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Retry == nil {
		opts.Retry = func(_ string, op func() error) error { return op() }
	}
	if opts.PageSize == 0 {
		opts.PageSize = 500
	}

	reports := make([]KindReport, len(StorageKinds))
	var problems []string
	for i, k := range StorageKinds {
		stored, problem, err := readStorage(ctx, c, k)
		if err != nil {
			return nil, err
		}
		if problem != "" {
			problems = append(problems, problem)
		}
		reports[i] = KindReport{CRD: k.CRDName(), StoredBefore: stored}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: apply the CustomResourceDefinitions of the release that stores %s first:\n  %s",
			ErrStoragePrecondition, StorageVersion, strings.Join(problems, "\n  "))
	}
	aliases, err := aliasEgressProxies(ctx, c, opts.PageSize)
	if err != nil {
		return nil, err
	}
	if len(aliases) > 0 {
		return nil, fmt.Errorf("%w: %d EgressProxy object(s) still name a deprecated egressPolicyMode alias, "+
			"which %s cannot store; set egressPolicyMode: FQDN on each and the matching GMC --fqdn-policy-backend, then re-run:\n  %s",
			ErrStoragePrecondition, len(aliases), StorageVersion, strings.Join(aliases, "\n  "))
	}

	var failed int
	for i, k := range StorageKinds {
		r := &reports[i]
		if slices.Equal(r.StoredBefore, []string{StorageVersion}) {
			r.StoredAfter = r.StoredBefore
			fprintf(opts.Out, "%s: storedVersions already [%s]; nothing to do\n", r.CRD, StorageVersion)
			continue
		}
		if err := sweepKind(ctx, c, k, opts, r); err != nil {
			return reports, err
		}
		switch {
		case !opts.Apply:
			fprintf(opts.Out, "%s: storedVersions %v; would rewrite %d object(s) and prune to [%s]\n",
				r.CRD, r.StoredBefore, r.Objects, StorageVersion)
		case len(r.Failed) > 0:
			failed += len(r.Failed)
			fprintf(opts.Out, "%s: rewrote %d of %d object(s); %d refused, storedVersions left at %v\n",
				r.CRD, r.Rewritten, r.Objects, len(r.Failed), r.StoredBefore)
		default:
			after, err := pruneStoredVersions(ctx, c, k, opts)
			if err != nil {
				return reports, err
			}
			r.StoredAfter = after
			fprintf(opts.Out, "%s: rewrote %d object(s); storedVersions %v -> %v\n", r.CRD, r.Rewritten, r.StoredBefore, after)
		}
	}
	if failed > 0 {
		var names []string
		for _, r := range reports {
			for _, f := range r.Failed {
				names = append(names, r.CRD+" "+f)
			}
		}
		return reports, fmt.Errorf("%d object(s) could not be rewritten, so their CRDs still list an older stored version; fix each and re-run:\n  %s",
			failed, strings.Join(names, "\n  "))
	}
	return reports, nil
}

// readStorage returns k's status.storedVersions, and a non-empty problem when the
// CRD is missing or does not yet store StorageVersion.
func readStorage(ctx context.Context, c client.Client, k StorageKind) ([]string, string, error) {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, k.CRDName() + ": not installed", nil
		}
		return nil, "", fmt.Errorf("get CustomResourceDefinition %s: %w", k.CRDName(), err)
	}
	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil {
		return nil, "", fmt.Errorf("read %s spec.versions: %w", k.CRDName(), err)
	}
	storage := ""
	for _, raw := range versions {
		v, _ := raw.(map[string]any)
		if s, _ := v["storage"].(bool); s {
			storage, _ = v["name"].(string)
		}
	}
	stored, _, err := unstructured.NestedStringSlice(crd.Object, "status", "storedVersions")
	if err != nil {
		return nil, "", fmt.Errorf("read %s status.storedVersions: %w", k.CRDName(), err)
	}
	if storage != StorageVersion {
		return stored, fmt.Sprintf("%s: storage version is %q", k.CRDName(), storage), nil
	}
	if !slices.Contains(stored, StorageVersion) {
		return stored, fmt.Sprintf("%s: storedVersions %v does not list %s yet", k.CRDName(), stored, StorageVersion), nil
	}
	return stored, "", nil
}

// aliasEgressProxies lists every EgressProxy naming a deprecated alias, read at
// v2beta1 because v2 shows a stored alias as FQDN.
func aliasEgressProxies(ctx context.Context, c client.Client, pageSize int64) ([]string, error) {
	var out []string
	err := eachObject(ctx, c, schema.GroupVersionKind{Group: storageGroup, Version: v2beta1.GroupVersion.Version, Kind: "EgressProxy"}, pageSize,
		func(o *unstructured.Unstructured) error {
			mode, _, _ := unstructured.NestedString(o.Object, "spec", "egressPolicyMode")
			if mode == string(v2beta1.EgressPolicyModeCiliumFQDN) || mode == string(v2beta1.EgressPolicyModeCalicoFQDN) {
				out = append(out, fmt.Sprintf("%s/%s\t%s", o.GetNamespace(), o.GetName(), mode))
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("list EgressProxies at v2beta1: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

// sweepKind lists every object of k at StorageVersion, counting them into r and,
// under opts.Apply, writing each back unchanged.
func sweepKind(ctx context.Context, c client.Client, k StorageKind, opts SweepOptions, r *KindReport) error {
	gvk := schema.GroupVersionKind{Group: storageGroup, Version: StorageVersion, Kind: k.Kind}
	err := eachObject(ctx, c, gvk, opts.PageSize, func(o *unstructured.Unstructured) error {
		r.Objects++
		if !opts.Apply {
			return nil
		}
		id := o.GetName()
		if ns := o.GetNamespace(); ns != "" {
			id = ns + "/" + id
		}
		err := opts.Retry(k.Kind+"/"+id, func() error { return rewrite(ctx, c, o) })
		switch {
		case err == nil:
			r.Rewritten++
		case apierrors.IsNotFound(err):
			// Deleted since the LIST: nothing left to store.
			r.Objects--
		default:
			r.Failed = append(r.Failed, fmt.Sprintf("%s: %v", id, err))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("list %s at %s: %w", k.Kind, StorageVersion, err)
	}
	return nil
}

// rewrite writes o back unchanged, re-reading it on a conflict.
func rewrite(ctx context.Context, c client.Client, o *unstructured.Unstructured) error {
	first := true
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !first {
			if err := c.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
				return err
			}
		}
		first = false
		return c.Update(ctx, o, client.FieldOwner("gag-migrate"))
	})
}

// pruneStoredVersions sets k's status.storedVersions to [StorageVersion] and returns
// it as read back from the apiserver.
func pruneStoredVersions(ctx context.Context, c client.Client, k StorageKind, opts SweepOptions) ([]string, error) {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	key := client.ObjectKey{Name: k.CRDName()}
	err := opts.Retry("CustomResourceDefinition/"+k.CRDName(), func() error {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.Get(ctx, key, crd); err != nil {
				return err
			}
			if err := unstructured.SetNestedStringSlice(crd.Object, []string{StorageVersion}, "status", "storedVersions"); err != nil {
				return err
			}
			return c.Status().Update(ctx, crd, client.FieldOwner("gag-migrate"))
		})
	})
	if err != nil {
		return nil, fmt.Errorf("prune %s status.storedVersions: %w", k.CRDName(), err)
	}
	if err := c.Get(ctx, key, crd); err != nil {
		return nil, fmt.Errorf("read back %s: %w", k.CRDName(), err)
	}
	after, _, err := unstructured.NestedStringSlice(crd.Object, "status", "storedVersions")
	if err != nil {
		return nil, fmt.Errorf("read back %s status.storedVersions: %w", k.CRDName(), err)
	}
	return after, nil
}

// eachObject calls fn for every object of gvk across all namespaces, a page at a time.
func eachObject(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, pageSize int64, fn func(*unstructured.Unstructured) error) error {
	cont := ""
	for {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := c.List(ctx, list, client.Limit(pageSize), client.Continue(cont)); err != nil {
			return err
		}
		for i := range list.Items {
			if err := fn(&list.Items[i]); err != nil {
				return err
			}
		}
		cont = list.GetContinue()
		if cont == "" {
			return nil
		}
	}
}

func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
