//go:build integration

package integration_test

import (
	"testing"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// TestV2alpha1Installed_PresentAgainstApiserver asserts the positive path of the
// Q228 startup gate: the shared suite installs the actions-gateway.com/v2alpha1
// CRDs, so V2alpha1Installed must report true against that apiserver's discovery
// — the signal main() uses to register the v2 controllers and enable the IPRange
// v2 refresh passes.
func TestV2alpha1Installed_PresentAgainstApiserver(t *testing.T) {
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	require.NoError(t, err)

	installed, err := controller.V2alpha1Installed(mgr.GetRESTMapper())
	require.NoError(t, err)
	require.True(t, installed, "expected v2alpha1 CRDs to be reported present when installed")
}
