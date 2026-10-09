package migrate

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

// These cover the sweep's decisions against a fake client, which neither converts
// between versions nor re-encodes storage. That the apiserver re-encodes and accepts
// the prune is TestStorageVersionSweep in the GMC envtest suite.

func storageScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	utilruntime.Must(v2.AddToScheme(s))
	utilruntime.Must(v2beta1.AddToScheme(s))
	return s
}

// storageCRD returns k's CRD storing storage, with storedVersions as given.
func storageCRD(k StorageKind, storage string, stored ...string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: k.CRDName()},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: storageGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: k.Resource, Kind: k.Kind},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v2beta1", Served: true, Storage: storage == "v2beta1"},
				{Name: "v2", Served: true, Storage: storage == "v2"},
			},
		},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{StoredVersions: stored},
	}
}

// allCRDs returns every StorageKinds CRD storing v2 with storedVersions as given.
func allCRDs(stored ...string) []client.Object {
	out := make([]client.Object, 0, len(StorageKinds))
	for _, k := range StorageKinds {
		out = append(out, storageCRD(k, "v2", stored...))
	}
	return out
}

func newSweepClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(storageScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&apiextensionsv1.CustomResourceDefinition{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

func storedVersions(t *testing.T, c client.Client, k StorageKind) []string {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: k.CRDName()}, &crd))
	return crd.Status.StoredVersions
}

func TestSweepStorageVersion_RewritesAndPrunes(t *testing.T) {
	objs := append(allCRDs("v2beta1", "v2"),
		&v2.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "team-a"}},
		&v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "linux", Namespace: "team-a"}},
		&v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "arm", Namespace: "team-b"}},
		&v2.PriorityClassAllowlist{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
	)
	var updates []string
	c := newSweepClient(t, &interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates = append(updates, obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
			return c.Update(ctx, obj, opts...)
		},
	}, objs...)

	reports, err := SweepStorageVersion(context.Background(), c, SweepOptions{Apply: true, PageSize: 1})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"ActionsGateway/acme", "RunnerSet/linux", "RunnerSet/arm", "PriorityClassAllowlist/default"}, updates,
		"every object is written back once, across pages")
	for i, k := range StorageKinds {
		assert.Equal(t, []string{"v2"}, storedVersions(t, c, k), k.CRDName())
		assert.True(t, reports[i].Pruned(), k.CRDName())
	}
	assert.Equal(t, 2, reports[2].Objects)
	assert.Equal(t, 2, reports[2].Rewritten)
}

func TestSweepStorageVersion_DryRunWritesNothing(t *testing.T) {
	objs := append(allCRDs("v2beta1", "v2"),
		&v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "linux", Namespace: "team-a"}})
	c := newSweepClient(t, &interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Fatal("a dry run must not write")
			return nil
		},
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			t.Fatal("a dry run must not prune")
			return nil
		},
	}, objs...)

	reports, err := SweepStorageVersion(context.Background(), c, SweepOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, reports[2].Objects)
	assert.Empty(t, reports[2].StoredAfter)
	assert.Equal(t, []string{"v2beta1", "v2"}, storedVersions(t, c, StorageKinds[2]))
}

func TestSweepStorageVersion_RefusedWriteLeavesItsKindUnpruned(t *testing.T) {
	objs := append(allCRDs("v2beta1", "v2"),
		&v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "team-a"}},
		&v2.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "team-a"}},
	)
	c := newSweepClient(t, &interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == "bad" {
				return apierrors.NewForbidden(schema.GroupResource{Group: storageGroup, Resource: "runnersets"}, "bad", errors.New("denied"))
			}
			return c.Update(ctx, obj, opts...)
		},
	}, objs...)

	reports, err := SweepStorageVersion(context.Background(), c, SweepOptions{Apply: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "team-a/bad")
	assert.Equal(t, []string{"v2beta1", "v2"}, storedVersions(t, c, StorageKinds[2]), "a refused write must leave its kind listing v2beta1")
	assert.False(t, reports[2].Pruned())
	assert.Equal(t, []string{"v2"}, storedVersions(t, c, StorageKinds[0]), "a kind with no refused write is still pruned")
}

func TestSweepStorageVersion_AlreadyPrunedIsANoOp(t *testing.T) {
	c := newSweepClient(t, &interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Version == "v2" {
				t.Fatal("a kind already storing only v2 must not be listed")
			}
			return c.List(ctx, list, opts...)
		},
	}, allCRDs("v2")...)
	reports, err := SweepStorageVersion(context.Background(), c, SweepOptions{Apply: true})
	require.NoError(t, err)
	for _, r := range reports {
		assert.True(t, r.Pruned(), r.CRD)
	}
}

func TestSweepStorageVersion_Preconditions(t *testing.T) {
	cases := map[string]struct {
		objs []client.Object
		want string
	}{
		"a CRD still stores v2beta1": {
			objs: append(allCRDs("v2beta1", "v2")[1:], storageCRD(StorageKinds[0], "v2beta1", "v2beta1")),
			want: `actionsgateways.actions-gateway.com: storage version is "v2beta1"`,
		},
		"a CRD is missing": {
			objs: allCRDs("v2beta1", "v2")[1:],
			want: "actionsgateways.actions-gateway.com: not installed",
		},
		"an EgressProxy names an alias": {
			objs: append(allCRDs("v2beta1", "v2"),
				&v2beta1.EgressProxy{
					ObjectMeta: metav1.ObjectMeta{Name: "pinned", Namespace: "team-a"},
					Spec:       v2beta1.EgressProxySpec{EgressPolicyMode: v2beta1.EgressPolicyModeCalicoFQDN},
				}),
			want: "team-a/pinned\tCalicoFQDN",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newSweepClient(t, &interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					t.Fatal("a failed precondition must write nothing")
					return nil
				},
			}, tc.objs...)
			_, err := SweepStorageVersion(context.Background(), c, SweepOptions{Apply: true})
			require.ErrorIs(t, err, ErrStoragePrecondition)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
