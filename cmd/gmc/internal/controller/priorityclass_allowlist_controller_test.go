package controller

import (
	"context"
	"reflect"
	"testing"

	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParsePriorityClassNames_SortsForStableLogging(t *testing.T) {
	got, err := parsePriorityClassNames([]string{"runner-standard", "runner-bursty"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"runner-bursty", "runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePriorityClassNames_Deduplicates(t *testing.T) {
	// The CRD marks the list x-kubernetes-list-type: set, so the apiserver rejects
	// duplicates on write. This is the defence-in-depth path for an object stored
	// before that marker existed, or written through a path that skipped validation.
	got, err := parsePriorityClassNames([]string{"runner-standard", "runner-standard"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePriorityClassNames_EmptyListIsValid(t *testing.T) {
	got, err := parsePriorityClassNames(nil)
	if err != nil {
		t.Fatalf("an empty list must be valid (no dynamic additions): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no names, got %v", got)
	}
}

func TestParsePriorityClassNames_BlankEntriesSkipped(t *testing.T) {
	got, err := parsePriorityClassNames([]string{"runner-standard", "   ", ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePriorityClassNames_InvalidNameRejectsWholeObject(t *testing.T) {
	// A single malformed entry must fail the whole parse — the valid sibling must
	// NOT be partially applied, or a typo could smuggle a class in alongside junk.
	got, err := parsePriorityClassNames([]string{"runner-standard", "Not A Valid Name!"})
	if err == nil {
		t.Fatalf("an invalid PriorityClass name must reject the whole object, got %v", got)
	}
}

func TestParsePriorityClassNames_RejectsUppercase(t *testing.T) {
	if _, err := parsePriorityClassNames([]string{"Runner-Standard"}); err == nil {
		t.Errorf("an uppercase name is not a valid DNS-1123 subdomain and must be rejected")
	}
}

// pcaScheme is a scheme carrying only the watched kind — the reconciler reads
// nothing else.
func pcaScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v2beta1.AddToScheme(s))
	return s
}

// reconcilePCA runs one Reconcile against a fake client holding objs, returning the
// two live allowlists the admission webhooks would read.
func reconcilePCA(t *testing.T, worker, infra *allowlist.PriorityClassAllowlist, objs ...client.Object) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(pcaScheme(t)).WithObjects(objs...).Build()
	r := &PriorityClassAllowlistReconciler{Client: c, Name: "gag-priorityclass-allowlist", Allowlist: worker, InfraAllowlist: infra}
	_, err := r.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
}

func pcaObject(worker, infra []string) *v2beta1.PriorityClassAllowlist {
	return &v2beta1.PriorityClassAllowlist{
		ObjectMeta: metav1.ObjectMeta{Name: "gag-priorityclass-allowlist"},
		Spec: v2beta1.PriorityClassAllowlistSpec{
			AllowedPriorityClasses:      worker,
			AllowedInfraPriorityClasses: infra,
		},
	}
}

func newPairedAllowlists() (*allowlist.PriorityClassAllowlist, *allowlist.PriorityClassAllowlist) {
	worker := allowlist.New([]string{"runner-standard"})
	infra := allowlist.New([]string{"gag-infra-critical"})
	allowlist.Pair(worker, infra)
	return worker, infra
}

func TestReconcile_AugmentsBothAllowlists(t *testing.T) {
	worker, infra := newPairedAllowlists()
	reconcilePCA(t, worker, infra, pcaObject([]string{"runner-bursty"}, []string{"gag-infra-high"}))

	assert.True(t, worker.Allowed("runner-bursty"), "the CR's worker list must reach the worker allowlist")
	assert.True(t, infra.Allowed("gag-infra-high"), "the CR's infra list must reach the infra allowlist (Q298)")
	assert.True(t, worker.Allowed("runner-standard"), "the static worker flag must survive")
	assert.True(t, infra.Allowed("gag-infra-critical"), "the static infra flag must survive")
}

func TestReconcile_MissingObjectWidensNothing(t *testing.T) {
	worker, infra := newPairedAllowlists()
	reconcilePCA(t, worker, infra)

	assert.False(t, infra.Allowed("gag-infra-high"), "no CR must mean no dynamic infra additions")
	assert.False(t, worker.Allowed("runner-bursty"), "no CR must mean no dynamic worker additions")
	assert.True(t, infra.Allowed("gag-infra-critical"), "the static infra flag must remain in force")
	assert.True(t, worker.Allowed("runner-standard"), "the static worker flag must remain in force")
}

func TestReconcile_InfraEntryCollidingWithTheWorkerFlagIsRefused(t *testing.T) {
	// The overlap the CRD's CEL rule cannot see: the CR's infra list names a class
	// the WORKER *flag* pins. Admitting it would let a tenant lift its workers to
	// infra priority — the inversion the two allowlists exist to prevent.
	worker, infra := newPairedAllowlists()
	reconcilePCA(t, worker, infra, pcaObject([]string{"runner-bursty"}, []string{"runner-standard"}))

	assert.False(t, infra.Allowed("runner-standard"), "a worker-allowed class must never become infra-allowed")
	assert.False(t, worker.Allowed("runner-bursty"), "a refused pair must not leave the worker half applied")
	assert.True(t, worker.Allowed("runner-standard"), "the refused pair must fall back to the static flags")
	assert.True(t, infra.Allowed("gag-infra-critical"), "the refused pair must fall back to the static flags")
}

func TestReconcile_InvalidInfraEntryWithholdsBothLists(t *testing.T) {
	// The CRD pattern rejects this at write time; an object stored before the field
	// existed must not be trusted. A bad infra entry must not let a good worker
	// entry through — the whole object is refused.
	worker, infra := newPairedAllowlists()
	reconcilePCA(t, worker, infra, pcaObject([]string{"runner-bursty"}, []string{"Not A Valid Name!"}))

	assert.False(t, worker.Allowed("runner-bursty"), "a malformed infra list must withhold the worker list too")
	assert.True(t, worker.Allowed("runner-standard"), "the static flags must remain in force")
	assert.True(t, infra.Allowed("gag-infra-critical"), "the static flags must remain in force")
}

func TestSetupWithManager_RequiresBothAllowlists(t *testing.T) {
	// The infra allowlist is not optional: a nil one would silently reconcile the
	// CR's infra list into nothing while reporting success.
	r := &PriorityClassAllowlistReconciler{Name: "x", Allowlist: allowlist.New(nil)}
	require.Error(t, r.SetupWithManager(nil))
}
