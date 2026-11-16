package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
)

func TestActionsGatewayV2Reconcile_ControlPlanePriorityClass(t *testing.T) {
	for _, tc := range []struct {
		name       string
		class      string
		scheduling *gmcv2.PodScheduling
		want       string
	}{
		{name: "a gateway naming no class gets the control-plane class", class: "gag-control-plane", want: "gag-control-plane"},
		{name: "a tenant's infra class stays", class: "gag-control-plane",
			scheduling: &gmcv2.PodScheduling{PriorityClassName: "gag-infra-critical"}, want: "gag-infra-critical"},
		{name: "no control-plane class leaves the pod unprioritized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := actionsGatewayV2TestScheme(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
			proxy := &gmcv2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"}}
			ag := v2Gateway("gw", "team-a", "github-app", "shared")
			ag.Spec.Scheduling = tc.scheduling
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, secret, proxy, ag).WithStatusSubresource(ag).Build()

			r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", ControlPlanePriorityClass: tc.class}
			reconcileV2Gateway(t, r, "team-a", "gw")

			var dep appsv1.Deployment
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
			assert.Equal(t, tc.want, dep.Spec.Template.Spec.PriorityClassName)
		})
	}
}

func TestEgressProxyReconcile_ControlPlanePriorityClass(t *testing.T) {
	for _, tc := range []struct {
		name       string
		class      string
		scheduling *gmcv2.PodScheduling
		want       string
	}{
		{name: "a proxy naming no class gets the control-plane class", class: "gag-control-plane", want: "gag-control-plane"},
		{name: "a tenant's infra class stays", class: "gag-control-plane",
			scheduling: &gmcv2.PodScheduling{PriorityClassName: "gag-infra-critical"}, want: "gag-infra-critical"},
		{name: "no control-plane class leaves the pod unprioritized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := egressProxyTestScheme(t)
			ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) { ep.Spec.Scheduling = tc.scheduling })
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).WithStatusSubresource(ep).Build()

			r := &EgressProxyReconciler{Client: c, Scheme: scheme, IPCache: &IPRangeCache{}, ProxyImage: "proxy:test", ControlPlanePriorityClass: tc.class}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}})
			require.NoError(t, err)

			var dep appsv1.Deployment
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}, &dep))
			assert.Equal(t, tc.want, dep.Spec.Template.Spec.PriorityClassName)
		})
	}
}

func TestVerifyPriorityOrder(t *testing.T) {
	for _, tc := range []struct {
		name         string
		classes      []*schedulingClass
		worker       []string
		controlPlane string
		wantErr      string
	}{
		{
			name:         "ordered",
			classes:      []*schedulingClass{{"runner-standard", 100000, false}, {"gag-control-plane", 1000000000, false}},
			worker:       []string{"runner-standard"},
			controlPlane: "gag-control-plane",
		},
		{
			name:         "the control-plane class must exist",
			classes:      []*schedulingClass{{"runner-standard", 100000, false}},
			controlPlane: "gag-control-plane",
			wantErr:      `--control-plane-priority-class "gag-control-plane" names no PriorityClass`,
		},
		{
			name:         "a worker class at control-plane priority refuses the start",
			classes:      []*schedulingClass{{"runner-urgent", 1000000000, false}, {"gag-control-plane", 1000000000, false}},
			worker:       []string{"runner-urgent"},
			controlPlane: "gag-control-plane",
			wantErr:      `worker class "runner-urgent" (1000000000) is not below control-plane class "gag-control-plane"`,
		},
		{
			name:         "a globalDefault class above the control plane refuses the start",
			classes:      []*schedulingClass{{"cluster-default", 2000000000, true}, {"gag-control-plane", 1000000000, false}},
			controlPlane: "gag-control-plane",
			wantErr:      `worker class "cluster-default"`,
		},
		{
			name:    "no control-plane class and no infra class checks nothing",
			classes: []*schedulingClass{{"runner-urgent", 1000000000, false}},
			worker:  []string{"runner-urgent"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(pcaScheme(t))
			for _, pc := range tc.classes {
				b = b.WithObjects(priorityClass(pc.name, pc.value, pc.globalDefault))
			}
			var infraNames []string
			if tc.controlPlane != "" {
				infraNames = []string{tc.controlPlane}
			}
			worker, infra := allowlist.New(tc.worker), allowlist.New(infraNames)
			allowlist.Pair(worker, infra)

			err := VerifyPriorityOrder(context.Background(), b.Build(), worker, infra, tc.controlPlane)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

type schedulingClass struct {
	name          string
	value         int32
	globalDefault bool
}
