//go:build integration

package integration_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"time"

	apiv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	apiv2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// conversionWebhookCancel stops the conversion webhook manager at suite teardown.
var conversionWebhookCancel context.CancelFunc

// suiteGoroutineBaseline snapshots the goroutines running once the suite's shared
// infrastructure — the conversion webhook manager in particular — is up but before
// any per-test reconciler starts. The goroutine-leak assertions (goleak.VerifyNone)
// pass it so the long-lived webhook manager's goroutines are not mistaken for a leak
// from the reconciler under test. It is set once, in startConversionWebhook.
var suiteGoroutineBaseline goleak.Option

// The v2 CRDs the AGC suite installs (api/config/crd) are multi-version as of Q74:
// v2beta1 is the storage/hub version and v2alpha1 the served spoke. Because envtest
// recognizes them as convertible (both versions are in testScheme) it patches each
// CRD's spec.conversion to point at a local webhook server, so every v2 create/read
// in this suite is routed through /convert. In production the GMC hosts that webhook;
// here the suite must serve it locally or every v2alpha1 RunnerSet create would either
// fail (webhook unreachable) or silently prune the ScaleSet-only-stripped fields.
//
// startConversionWebhook starts a manager whose only job is serving /convert for the
// five v2 hub kinds, bound to the host/port/cert envtest allocated in
// testEnv.WebhookInstallOptions, then blocks until a conversion-exercising create
// succeeds.
func startConversionWebhook() error {
	opts := testEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    opts.LocalServingHost,
			Port:    opts.LocalServingPort,
			CertDir: opts.LocalServingCertDir,
		}),
	})
	if err != nil {
		return fmt.Errorf("create conversion webhook manager: %w", err)
	}
	// One /convert handler serves all five hub kinds; each NewWebhookManagedBy call
	// (no validator/defaulter) registers only the conversion webhook and asserts the
	// kind is convertible in the scheme.
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.ActionsGateway{}).Complete(); err != nil {
		return fmt.Errorf("register ActionsGateway conversion webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.EgressProxy{}).Complete(); err != nil {
		return fmt.Errorf("register EgressProxy conversion webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.RunnerSet{}).Complete(); err != nil {
		return fmt.Errorf("register RunnerSet conversion webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.RunnerTemplate{}).Complete(); err != nil {
		return fmt.Errorf("register RunnerTemplate conversion webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &apiv2beta1.ClusterRunnerTemplate{}).Complete(); err != nil {
		return fmt.Errorf("register ClusterRunnerTemplate conversion webhook: %w", err)
	}

	mgrCtx, mgrCancel := context.WithCancel(ctx)
	conversionWebhookCancel = mgrCancel
	go func() { _ = mgr.Start(mgrCtx) }()

	if err := waitForConversionReady(opts); err != nil {
		return err
	}
	// Snapshot the now-running infrastructure goroutines (the webhook manager and its
	// cache/listener) so the reconciler goroutine-leak assertions ignore them.
	suiteGoroutineBaseline = goleak.IgnoreCurrent()
	return nil
}

// waitForConversionReady blocks until the conversion webhook is serving and the
// apiserver can reach it: it waits for the TLS listener, then proves the /convert
// path end-to-end by creating a v2alpha1 RunnerSet (whose storage as the v2beta1 hub
// requires conversion) and confirming the round-trip preserves the defaulted
// acquisitionProtocol — the exact field that would be pruned if conversion were not
// wired.
func waitForConversionReady(opts envtest.WebhookInstallOptions) error {
	addr := net.JoinHostPort(opts.LocalServingHost, strconv.Itoa(opts.LocalServingPort))
	dialErr := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
		func(context.Context) (bool, error) {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr,
				&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // local envtest serving cert, identity is irrelevant
			if err != nil {
				return false, nil
			}
			_ = conn.Close()
			return true, nil
		})
	if dialErr != nil {
		return fmt.Errorf("conversion webhook TLS listener never came up at %s: %w", addr, dialErr)
	}

	const readinessNS = "agc-conversion-readiness"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: readinessNS}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create readiness namespace: %w", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	readyErr := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
		func(context.Context) (bool, error) {
			probe := &apiv2alpha1.RunnerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "conversion-probe", Namespace: readinessNS},
				Spec: apiv2alpha1.RunnerSetSpec{
					GatewayRef:   apiv2alpha1.ObjectRef{Name: "probe-gw"},
					RunnerLabels: []string{"probe"},
				},
			}
			if err := k8sClient.Create(ctx, probe); err != nil {
				// Conversion webhook not reachable yet — keep polling.
				return false, nil
			}
			defer func() { _ = k8sClient.Delete(ctx, probe) }()
			var got apiv2alpha1.RunnerSet
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: readinessNS, Name: "conversion-probe"}, &got); err != nil {
				return false, nil
			}
			// If conversion round-trips, the defaulted acquisitionProtocol survives; a
			// None-strategy prune would leave it empty.
			return got.Spec.AcquisitionProtocol == apiv2alpha1.AcquisitionProtocolScaleSet, nil
		})
	if readyErr != nil {
		return fmt.Errorf("conversion webhook never became ready: %w", readyErr)
	}
	return nil
}
