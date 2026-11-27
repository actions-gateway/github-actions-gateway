package controller

import (
	"context"
	"testing"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestApplyNamespacePSA_OverriddenEventNamesProfileSource pins the PSALabelsOverridden
// Event text: it names the profile being re-applied and where it came from, and never
// claims a security-profile label the namespace does not carry.
func TestApplyNamespacePSA_OverriddenEventNamesProfileSource(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "labelled namespace",
			labels: map[string]string{gmcv2.SecurityProfileLabel: gmcv2.SecurityProfileRestricted},
			want:   `to the "restricted" profile (set by its ` + gmcv2.SecurityProfileLabel + ` label)`,
		},
		{
			name: "unlabelled namespace",
			want: `to the "baseline" profile (the default for a namespace without an ` + gmcv2.SecurityProfileLabel + ` label)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Labels: tc.labels}}
			applies := 0
			c := fake.NewClientBuilder().WithObjects(ns).
				WithInterceptorFuncs(interceptor.Funcs{
					// The first, non-forced apply conflicts with another field manager;
					// the forced re-apply succeeds.
					Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
						applies++
						if applies == 1 {
							return apierrors.NewConflict(schema.GroupResource{Resource: "namespaces"}, ns.Name, nil)
						}
						return nil
					},
				}).Build()
			rec := events.NewFakeRecorder(4)
			r := &NamespacePSAReconciler{Client: c, Recorder: rec}

			require.NoError(t, r.applyNamespacePSA(context.Background(), ns))
			assert.Equal(t, 2, applies, "a conflict must be re-applied with ForceOwnership")

			select {
			case msg := <-rec.Events:
				assert.Contains(t, msg, "PSALabelsOverridden")
				assert.Contains(t, msg, tc.want)
				if tc.labels == nil {
					assert.NotContains(t, msg, gmcv2.SecurityProfileLabel+"=",
						"the Event must not name a label the namespace lacks")
				}
			default:
				t.Fatal("expected a PSALabelsOverridden Warning event on the namespace")
			}
		})
	}
}
