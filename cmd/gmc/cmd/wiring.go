package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	webhookv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v1alpha1"
	webhookv2alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v2alpha1"
	webhookv2beta1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v2beta1"
)

// newManager constructs the controller-runtime manager from the resolved config,
// metrics server options, and webhook server. The options include the two-layer
// Secret isolation (a metadata-only Secret informer via WatchesMetadata on the
// controllers, plus DisableFor here so r.Get() bypasses the cache and reads
// Secret bodies fresh) and LeaderElectionReleaseOnCancel for fast failover
// (safe because main() exits immediately after mgr.Start returns).
func newManager(cfg *gmcFlags, rc *resolvedConfig, metricsOpts metricsserver.Options,
	webhookServer webhook.Server) (ctrl.Manager, error) {
	// Dedupe API server warnings to one log line per unique message per process.
	// The default handler logs every occurrence, and the v1alpha1/v2alpha1
	// deprecation warnings repeat on every read/write of a deprecated-version CR —
	// under reconcile churn they dominate the log (Q515).
	restCfg := ctrl.GetConfigOrDie()
	restCfg.WarningHandlerWithContext = logf.NewKubeAPIWarningLogger(
		logf.KubeAPIWarningLoggerOptions{Deduplicate: true})
	return ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOpts,
		WebhookServer:          webhookServer,
		Cache:                  rc.cacheOptions,
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.enableLeaderElection,
		LeaderElectionID:       "actions-gateway-gmc-leader",
		LeaseDuration:          &cfg.leaderElectLeaseDuration,
		RenewDeadline:          &cfg.leaderElectRenewDeadline,
		RetryPeriod:            &cfg.leaderElectRetryPeriod,
		// DisableFor ensures r.Get() calls bypass the cache entirely and hit the
		// API server directly, so the actual Secret contents are always read fresh
		// and never persist in-process after the call returns.
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
		// LeaderElectionReleaseOnCancel makes the leader step down voluntarily when
		// the Manager ends, so the standby takes over in ~RetryPeriod instead of
		// waiting out LeaseDuration. Safe because main() exits immediately after
		// mgr.Start returns, with no post-stop cleanup that could race the release.
		LeaderElectionReleaseOnCancel: cfg.leaderElectReleaseOnCancel,
	})
}

// registerControllers wires every GMC controller onto the manager. It detects the
// opt-in v2 CRDs once (V2alpha1Installed), fails closed when an installed CRD's
// schema is behind this binary on a field that bounds tenant access
// (VerifyCRDSchemas), registers the GMC metrics collectors
// and build-info gauge, parses the apiserver-CIDR allowlist, and registers the v1
// ActionsGateway reconciler, the v2 controllers (only when the CRDs are present),
// the periodic IP-range reconciler, and the two allowlist ConfigMap watches. It
// returns an error instead of exiting so the flow stays test-reachable and main
// owns the single exit. The v2 detection must happen before NewMetrics so the
// scrape-time collectors can count v2 gateways without spinning a failed informer
// on a v1-only cluster (Q320).
func registerControllers(mgr ctrl.Manager, cfg *gmcFlags, rc *resolvedConfig, img gmcImages) error {
	// Bounded client for the GitHub meta IP-range fetch (Q138): an overall 60s
	// deadline plus a transport ResponseHeaderTimeout, so a slow api.github.com
	// cannot wedge the reconcile.
	httpClient := httpx.NewClientWithTimeout(60 * time.Second)

	// IPRangeCache is shared between the per-CR reconciler (read path) and the
	// periodic IPRangeReconciler (write path), keeping per-CR reconciles off the
	// network.
	ipCache := &controller.IPRangeCache{}

	// The v2 controllers and the IPRange reconciler's v2 refresh paths depend on
	// the actions-gateway.com/v2alpha1 CRDs, which ship in the SEPARATE, opt-in
	// actions-gateway-crds-v2 chart. Detect them once at startup: on a v1-only
	// install the kinds are absent, so registering the v2 controllers
	// unconditionally would spin a source.Kind retry loop. Installing the v2 CRDs
	// later requires a GMC restart to enable the v2 controllers (Q228).
	v2Enabled, err := controller.V2alpha1Installed(mgr.GetRESTMapper())
	if err != nil {
		return fmt.Errorf("detect actions-gateway.com/v2alpha1 CRDs: %w", err)
	}
	if v2Enabled {
		setupLog.Info("actions-gateway.com/v2alpha1 CRDs detected; enabling v2 controllers")
	} else {
		setupLog.Info("actions-gateway.com/v2alpha1 CRDs not installed; v2 controllers disabled " +
			"(install the actions-gateway-crds-v2 chart and restart the GMC to enable them)")
	}

	// The kinds being served says nothing about their SCHEMA being current, and the
	// v2 CRDs are applied out-of-band, so `helm upgrade` never carries a field
	// change into them (Q852). A field the stored schema does not declare is pruned
	// on write with no error, so a boundary a tenant declared can be inert. Refuse
	// to start on one rather than provision against it. Bounded so a wedged
	// apiserver cannot hold startup open indefinitely.
	crdCtx, cancelCRDCheck := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCRDCheck()
	if err := controller.VerifyCRDSchemas(crdCtx, mgr.GetAPIReader()); err != nil {
		return fmt.Errorf("verify installed CRD schemas: %w", err)
	}

	// Register the GMC's custom metrics. The scrape-time collectors list the
	// relevant CRs from the manager cache, so they must be given a cached reader;
	// v2Enabled gates the v2 ActionsGateway/EgressProxy passes (Q320).
	gmcMetrics := controller.NewMetrics(mgr.GetClient(), v2Enabled)
	// Emit actions_gateway_build_info{component="gmc",version=…} 1 so the running
	// binary version is correlatable straight from metrics during an incident (Q318).
	registerBuildInfo(ctrlmetrics.Registry, "gmc", version)

	parsedAPIServerCIDRs, err := parseAPIServerCIDRs(cfg.apiServerCIDRs)
	if err != nil {
		return fmt.Errorf("invalid --apiserver-cidrs value: %w", err)
	}

	// IP-range refresh cadence; EgressRulesStale (Q157) trips when a gateway's
	// allowlist goes unrefreshed for just over two of these intervals.
	ipInterval := 24 * time.Hour

	if err := (&controller.ActionsGatewayReconciler{
		Client:                      mgr.GetClient(),
		Scheme:                      mgr.GetScheme(),
		IPCache:                     ipCache,
		AGCImage:                    img.agcImage,
		ProxyImage:                  img.proxyImage,
		AGCExtraEnv:                 img.agcExtraEnv,
		EnableTenantServiceMonitors: cfg.enableTenantServiceMonitors,
		APIServerCIDRs:              parsedAPIServerCIDRs,
		EgressStaleThreshold:        2*ipInterval + time.Hour,
		Recorder:                    mgr.GetEventRecorder("actionsgateway-controller"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("create actionsgateway controller: %w", err)
	}

	if v2Enabled {
		// ActionsGateway reconciler (v2 M3a): provisions the per-tenant AGC control
		// plane and wires its egress through the EgressProxy named by defaultProxyRef.
		if err := (&controller.ActionsGatewayV2Reconciler{
			Client:         mgr.GetClient(),
			Scheme:         mgr.GetScheme(),
			AGCImage:       img.agcImage,
			AGCExtraEnv:    img.agcExtraEnv,
			APIServerCIDRs: parsedAPIServerCIDRs,
			IPCache:        ipCache,
			Recorder:       mgr.GetEventRecorder("actionsgateway-v2-controller"),
			Reader:         mgr.GetAPIReader(),
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("create actionsgateway-v2 controller: %w", err)
		}

		// EgressProxy reconciler (v2 M2): reconciles a standalone EgressProxy into a
		// proxy pool it owns. Shares the GitHub IP-range cache so the proxy
		// NetworkPolicy's GitHub-CIDR egress allowlist stays current.
		if err := (&controller.EgressProxyReconciler{
			Client:               mgr.GetClient(),
			APIReader:            mgr.GetAPIReader(),
			Scheme:               mgr.GetScheme(),
			IPCache:              ipCache,
			ProxyImage:           img.proxyImage,
			FQDNBackend:          rc.fqdnBackend,
			EnableServiceMonitor: cfg.enableTenantServiceMonitors,
			EgressStaleThreshold: 2*ipInterval + time.Hour,
			Recorder:             mgr.GetEventRecorder("egressproxy-controller"),
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("create egressproxy controller: %w", err)
		}

		// NamespacePSA reconciler (v2 Q175): stamps the namespace Pod Security
		// Admission labels from its actions-gateway.com/security-profile label.
		if err := (&controller.NamespacePSAReconciler{
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorder("namespace-psa-controller"),
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("create namespace-psa controller: %w", err)
		}
	}

	if err := mgr.Add(&controller.IPRangeReconciler{
		Client:         mgr.GetClient(),
		Fetcher:        &controller.HTTPGitHubIPRangeFetcher{Client: httpClient},
		Cache:          ipCache,
		Interval:       ipInterval,
		Log:            slog.New(logr.ToSlogHandler(ctrl.Log.WithName("ipranges"))),
		Metrics:        gmcMetrics,
		APIServerCIDRs: parsedAPIServerCIDRs,
		// On a v1-only install the v2 EgressProxy / ActionsGateway refresh passes
		// are disabled to avoid a "no matches for kind" error on every tick (Q228).
		V2Enabled: v2Enabled,
	}); err != nil {
		return fmt.Errorf("register IP range reconciler: %w", err)
	}

	// PriorityClass allowlist watch (Q188 worker, Q298 infra): when enabled,
	// reconcile the designated cluster-scoped PriorityClassAllowlist into the
	// dynamic halves of both shared allowlists, keeping them disjoint. The same
	// object is the priorityclass-allowlist-guard policy's paramKind (Q492), so the
	// webhook and the policy cannot drift. Runs in every replica (the reconciler
	// disables leader election) because every replica serves the admission webhook.
	// Disabled by default (empty flag).
	if cfg.priorityClassAllowlistName != "" {
		if err := (&controller.PriorityClassAllowlistReconciler{
			Client:         mgr.GetClient(),
			Name:           cfg.priorityClassAllowlistName,
			Allowlist:      rc.priorityClassAllowlist,
			InfraAllowlist: rc.infraPriorityClassAllowlist,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("create priorityclass-allowlist controller: %w", err)
		}
	}
	// Egress destination allowlist ConfigMap watch (Q242 G.1): same shape as the
	// PriorityClass watch — reconcile the designated ConfigMap into the dynamic half
	// of the shared egress allowlist. Disabled by default (empty flag).
	if cfg.egressDestinationAllowlistConfigMap != "" {
		if err := (&controller.EgressDestinationAllowlistReconciler{
			Client:        mgr.GetClient(),
			ConfigMapName: cfg.egressDestinationAllowlistConfigMap,
			Namespace:     rc.podNamespace,
			Allowlist:     rc.egressDestinationAllowlist,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("create egress-destination-allowlist controller: %w", err)
		}
	}
	return nil
}

// registerWebhooks registers the GMC's admission and conversion webhooks. The
// caller gates this on ENABLE_WEBHOOKS != "false". It returns an error instead of
// exiting so the flow stays test-reachable and main owns the single exit.
func registerWebhooks(mgr ctrl.Manager, rc *resolvedConfig) error {
	if err := webhookv1alpha1.SetupActionsGatewayWebhookWithManager(mgr, rc.priorityClassAllowlist); err != nil {
		return fmt.Errorf("create ActionsGateway (v1alpha1) webhook: %w", err)
	}
	// v2 M2: reserved-pod-field validating webhooks for the RunnerTemplate data
	// kinds. Q289: the namespaced kind's podTemplate.spec.priorityClassName is gated
	// against the same platform PriorityClass allowlist as priorityTiers.
	if err := webhookv2alpha1.SetupRunnerTemplateWebhooksWithManager(mgr, rc.priorityClassAllowlist); err != nil {
		return fmt.Errorf("create RunnerTemplate webhook: %w", err)
	}
	// Q242 G.1: gate tenant-authored EgressProxy destinationFQDNs/destinationCIDRs
	// against the platform egress allowlist. Q245: reject FQDN intent when the
	// cluster declares no --fqdn-policy-backend. Q284: gate
	// spec.scheduling.priorityClassName against the infra-only allowlist.
	if err := webhookv2alpha1.SetupEgressProxyWebhookWithManager(
		mgr, rc.egressDestinationAllowlist, rc.fqdnBackend, rc.infraPriorityClassAllowlist); err != nil {
		return fmt.Errorf("create EgressProxy webhook: %w", err)
	}
	// Q284: gate the v2 ActionsGateway spec.scheduling.priorityClassName on the AGC
	// control-plane pod against the infra-only allowlist.
	if err := webhookv2alpha1.SetupActionsGatewayWebhookWithManager(mgr, rc.infraPriorityClassAllowlist); err != nil {
		return fmt.Errorf("create ActionsGateway (v2alpha1) webhook: %w", err)
	}
	// Q264 P3: reject two ScaleSet-protocol RunnerSets sharing a runnerLabel under
	// one gateway. Q289: gate priorityTiers[].priorityClassName against the platform
	// PriorityClass allowlist.
	if err := webhookv2alpha1.SetupRunnerSetWebhookWithManager(mgr, rc.priorityClassAllowlist); err != nil {
		return fmt.Errorf("create RunnerSet webhook: %w", err)
	}
	// Q74: the GMC-hosted conversion webhook (/convert) for the five v2 hub kinds.
	if err := webhookv2beta1.SetupConversionWebhooksWithManager(mgr); err != nil {
		return fmt.Errorf("create conversion webhook: %w", err)
	}
	return nil
}

// setupHealthChecks registers the /healthz and /readyz probes. When webhooks are
// enabled it also gates readiness on the webhook server actually listening, so a
// fresh GMC pod is not added to the webhook Service endpoints before the admission
// port is bound (which would make every dependent kubectl apply time out for ~1s
// during a rollout).
func setupHealthChecks(mgr ctrl.Manager, enableWebhooks bool) error {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("set up ready check: %w", err)
	}
	if enableWebhooks {
		if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			return fmt.Errorf("set up webhook ready check: %w", err)
		}
	}
	return nil
}
