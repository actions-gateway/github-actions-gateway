package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Tests for the ConfigMap-backed GuardStore (Q606): the durable state the scale-set
// listener writes ahead of its message deletes, so a hard-killed AGC does not replay
// an assignment it concluded. The listener-side semantics (write-ahead ordering,
// retirement, the drained-queue sweep) are covered in the scalesetlistener package
// against a fake store; these cover the ConfigMap round-trip itself.

func guardScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v2.AddToScheme(s)
	return s
}

func guardStoreFor(t *testing.T, rs *v2.RunnerSet) *scaleSetGuardStore {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(guardScheme()).Build()
	r := &RunnerSetReconciler{Client: c}
	return r.scaleSetGuardStore(rs)
}

func testRunnerSet() *v2.RunnerSet {
	return &v2.RunnerSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "tenant-a", Name: "linux-large", UID: types.UID("uid-1"),
	}}
}

// TestScaleSetGuardStore_LoadBeforeFirstSaveIsEmpty pins the first-start shape: no
// ConfigMap is not an error, it is the empty state — a brand-new RunnerSet must come
// up without anyone pre-creating anything.
func TestScaleSetGuardStore_LoadBeforeFirstSaveIsEmpty(t *testing.T) {
	s := guardStoreFor(t, testRunnerSet())
	state, err := s.Load(context.Background())
	require.NoError(t, err)
	assert.Empty(t, state.Completed)
	assert.Empty(t, state.Abandoned)
}

// TestScaleSetGuardStore_SaveRoundTrips covers create-then-update: the first save
// creates the ConfigMap, later saves replace its state, and Load returns what was
// last saved.
func TestScaleSetGuardStore_SaveRoundTrips(t *testing.T) {
	s := guardStoreFor(t, testRunnerSet())
	ctx := context.Background()

	require.NoError(t, s.Save(ctx, scalesetlistener.GuardState{
		Completed: []string{"job-a"}, Abandoned: []string{"job-b"},
	}))
	state, err := s.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"job-a"}, state.Completed)
	assert.Equal(t, []string{"job-b"}, state.Abandoned)

	require.NoError(t, s.Save(ctx, scalesetlistener.GuardState{Completed: []string{"job-c"}}))
	state, err = s.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"job-c"}, state.Completed)
	assert.Empty(t, state.Abandoned, "a save replaces the state, it does not merge")
}

// TestScaleSetGuardStore_ConfigMapCarriesOwnershipAndLabel pins the lifecycle wiring:
// the ConfigMap is controller-owned by its RunnerSet — deleting the set (or its
// namespace) garbage-collects the guards, which is the only delete path the AGC has —
// and labelled the way the set's worker pods are, so an operator can find it.
func TestScaleSetGuardStore_ConfigMapCarriesOwnershipAndLabel(t *testing.T) {
	rs := testRunnerSet()
	c := fake.NewClientBuilder().WithScheme(guardScheme()).Build()
	r := &RunnerSetReconciler{Client: c}
	s := r.scaleSetGuardStore(rs)

	require.NoError(t, s.Save(context.Background(), scalesetlistener.GuardState{Completed: []string{"job-a"}}))

	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: rs.Namespace, Name: scaleSetGuardConfigMapName(rs.Name)}, &cm))
	require.Len(t, cm.OwnerReferences, 1)
	assert.Equal(t, "RunnerSet", cm.OwnerReferences[0].Kind)
	assert.Equal(t, rs.Name, cm.OwnerReferences[0].Name)
	assert.Equal(t, rs.UID, cm.OwnerReferences[0].UID)
	assert.Equal(t, rs.Name, cm.Labels[provisioner.LabelRunnerSet])
}

// TestScaleSetGuardStore_LoadRejectsUnparseableState pins the failure direction for a
// corrupt ConfigMap: an error, not an empty read — an empty read would silently reopen
// the replay window, while the error keeps the listener down until the ConfigMap is
// deleted or repaired, with the remediation in the message.
func TestScaleSetGuardStore_LoadRejectsUnparseableState(t *testing.T) {
	rs := testRunnerSet()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: rs.Namespace, Name: scaleSetGuardConfigMapName(rs.Name),
		},
		Data: map[string]string{scaleSetGuardDataKey: "{not json"},
	}
	c := fake.NewClientBuilder().WithScheme(guardScheme()).WithObjects(cm).Build()
	r := &RunnerSetReconciler{Client: c}
	s := r.scaleSetGuardStore(rs)

	_, err := s.Load(context.Background())
	require.ErrorContains(t, err, "delete the ConfigMap")
}

// TestRunnerSetTarget_InFlightJobsReadsTheListenersRecord pins the read abandoned-run
// recovery names a stranded job from (Q1195): the target reads the same ConfigMap the
// listener saves, and returns each in-flight job's run identity.
func TestRunnerSetTarget_InFlightJobsReadsTheListenersRecord(t *testing.T) {
	ctx := context.Background()
	rs := testRunnerSet()
	c := fake.NewClientBuilder().WithScheme(guardScheme()).Build()
	r := &RunnerSetReconciler{Client: c}
	require.NoError(t, r.scaleSetGuardStore(rs).Save(ctx, scalesetlistener.GuardState{
		InFlight: []scalesetlistener.InFlightJob{
			{JobID: "job-b", Owner: "myorg", Repository: "repo-b", RunID: "222"},
			{JobID: "job-c", Owner: "myorg", Repository: "repo-c", RunID: "333", Started: true},
		},
	}))

	jobs, err := r.provisionerTarget(rs).InFlightJobs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []provisioner.OrphanedWorker{
		{JobID: "job-b", Owner: "myorg", Repository: "repo-b", RunID: "222"},
		{JobID: "job-c", Owner: "myorg", Repository: "repo-c", RunID: "333", Started: true},
	}, jobs)
}

// staleOwnerConfigMap is a ConfigMap as a v1.10.0 AGC left it: its owner reference names
// a version v2.0.0 does not serve.
func staleOwnerConfigMap(rs *v2.RunnerSet, name, ownerUID string) *corev1.ConfigMap {
	ref := runnerSetOwnerRef(rs.Name, types.UID(ownerUID))
	ref.APIVersion = "actions-gateway.com/v2alpha1"
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: rs.Namespace, Name: name, OwnerReferences: []metav1.OwnerReference{ref},
	}}
}

func ownerVersionOf(t *testing.T, c client.Client, rs *v2.RunnerSet, name string) string {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: rs.Namespace, Name: name}, &cm))
	require.Len(t, cm.OwnerReferences, 1)
	return cm.OwnerReferences[0].APIVersion
}

// TestRestampConfigMapOwners moves each of the three ConfigMaps a set owns to v2, and
// reads them once per process.
func TestRestampConfigMapOwners(t *testing.T) {
	rs := testRunnerSet()
	names := append(provisioner.OwnedConfigMapNames(rs.Name), scaleSetGuardConfigMapName(rs.Name))
	require.Len(t, names, 3)
	b := fake.NewClientBuilder().WithScheme(guardScheme())
	for _, n := range names {
		b = b.WithObjects(staleOwnerConfigMap(rs, n, string(rs.UID)))
	}
	var gets atomic.Int64
	c := b.WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := &RunnerSetReconciler{Client: c}
	ctx := context.Background()

	r.restampConfigMapOwners(ctx, slog.Default(), rs)
	for _, n := range names {
		assert.Equal(t, "actions-gateway.com/v2", ownerVersionOf(t, c, rs, n), n)
	}

	gets.Store(0)
	r.restampConfigMapOwners(ctx, slog.Default(), rs)
	assert.Zero(t, gets.Load(), "a set already handled by this process costs no read")
}

// TestRestampConfigMapOwners_LeavesAnEarlierSetsConfigMapAlone: a same-named set
// recreated after the upgrade does not take ownership of what its predecessor left.
func TestRestampConfigMapOwners_LeavesAnEarlierSetsConfigMapAlone(t *testing.T) {
	rs := testRunnerSet()
	name := scaleSetGuardConfigMapName(rs.Name)
	c := fake.NewClientBuilder().WithScheme(guardScheme()).
		WithObjects(staleOwnerConfigMap(rs, name, "uid-of-a-deleted-set")).Build()
	r := &RunnerSetReconciler{Client: c}

	r.restampConfigMapOwners(context.Background(), slog.Default(), rs)
	assert.Equal(t, "actions-gateway.com/v2alpha1", ownerVersionOf(t, c, rs, name))
}

// TestRestampConfigMapOwners_FailedWriteRetriesOnTheNextReconcile: a refused write must
// not mark the set done, or the ConfigMap keeps its stale reference for the whole process.
func TestRestampConfigMapOwners_FailedWriteRetriesOnTheNextReconcile(t *testing.T) {
	rs := testRunnerSet()
	name := scaleSetGuardConfigMapName(rs.Name)
	refuse := true
	c := fake.NewClientBuilder().WithScheme(guardScheme()).
		WithObjects(staleOwnerConfigMap(rs, name, string(rs.UID))).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if refuse {
					return errors.New("apiserver unavailable")
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &RunnerSetReconciler{Client: c}
	ctx := context.Background()

	r.restampConfigMapOwners(ctx, slog.Default(), rs)
	require.Equal(t, "actions-gateway.com/v2alpha1", ownerVersionOf(t, c, rs, name))

	refuse = false
	r.restampConfigMapOwners(ctx, slog.Default(), rs)
	assert.Equal(t, "actions-gateway.com/v2", ownerVersionOf(t, c, rs, name))
}

// TestReconcile_RestampsATerminatingSetBeforeReleasingIt covers a set deleted across the
// upgrade: this AGC first sees it terminating, and removing the finalizer is what hands
// the ConfigMap to the garbage collector, so the reference has to be right by then.
func TestReconcile_RestampsATerminatingSetBeforeReleasingIt(t *testing.T) {
	rs := testRunnerSet()
	now := metav1.Now()
	rs.DeletionTimestamp = &now
	rs.Finalizers = []string{runnerSetFinalizer}
	name := scaleSetGuardConfigMapName(rs.Name)

	var versionAtRelease string
	c := fake.NewClientBuilder().WithScheme(guardScheme()).
		WithObjects(rs, staleOwnerConfigMap(rs, name, string(rs.UID))).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, wc client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, isSet := obj.(*v2.RunnerSet); isSet {
					versionAtRelease = ownerVersionOf(t, wc, rs, name)
				}
				return wc.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &RunnerSetReconciler{Client: c, Log: slog.Default()}
	r.ensureMaps()

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)})
	require.NoError(t, err)
	assert.Equal(t, "actions-gateway.com/v2", versionAtRelease,
		"the owner reference must be right before the finalizer comes off")
}
