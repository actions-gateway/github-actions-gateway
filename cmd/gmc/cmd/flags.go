package main

import (
	"flag"
	"time"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
)

// gmcFlags holds every command-line flag the GMC binds. Declaring them into a
// struct via addFlags (rather than inline in main) makes the ~25-flag surface and
// its defaults visible in one place and unit-testable against a throwaway
// flag.FlagSet (Q367).
type gmcFlags struct {
	metricsAddr     string
	metricsCertPath string
	metricsCertName string
	metricsCertKey  string

	webhookCertPath string
	webhookCertName string
	webhookCertKey  string

	enableLeaderElection bool
	probeAddr            string
	secureMetrics        bool
	enableHTTP2          bool

	leaderElectLeaseDuration   time.Duration
	leaderElectRenewDeadline   time.Duration
	leaderElectRetryPeriod     time.Duration
	leaderElectReleaseOnCancel bool

	allowAgcExtraEnv            bool
	allowFloatingImageTags      bool
	enableTenantServiceMonitors bool

	allowedPriorityClasses      string
	allowedInfraPriorityClasses string
	priorityClassAllowlistName  string

	allowedEgressFQDNs                  string
	allowedEgressCIDRs                  string
	egressDestinationAllowlistConfigMap string

	fqdnPolicyBackend string
	apiServerCIDRs    string
}

// addFlags declares the GMC's flags into fs and returns the struct they bind to.
// main passes flag.CommandLine; tests pass a fresh flag.FlagSet to assert
// defaults and parsing.
func addFlags(fs *flag.FlagSet) *gmcFlags {
	f := &gmcFlags{}
	fs.StringVar(&f.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	fs.StringVar(&f.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&f.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	// Leader-election timing knobs. Defaults match controller-runtime/client-go
	// (15s/10s/2s) so existing deployments behave identically. They are exposed
	// so operators can tune the failover/false-positive trade-off per cluster:
	// larger values tolerate slower apiservers (fewer spurious lease losses),
	// smaller values fail over faster (k8s audit F4). The invariant
	// LeaseDuration > RenewDeadline > RetryPeriod×1.2 is validated in main.
	fs.DurationVar(&f.leaderElectLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"Duration non-leader candidates wait before force-acquiring leadership.")
	fs.DurationVar(&f.leaderElectRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"Duration the acting leader retries refreshing leadership before giving up.")
	fs.DurationVar(&f.leaderElectRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"Interval between leader-election action attempts.")
	// ReleaseOnCancel makes the active manager step down voluntarily on SIGTERM
	// instead of holding the lease until it expires, so the standby takes over in
	// ~RetryPeriod rather than ~LeaseDuration. This closes the rollout reconcile
	// gap that terminationGracePeriodSeconds (10s) < LeaseDuration (15s) would
	// otherwise leave (k8s audit F3). Safe here: main() exits immediately once
	// mgr.Start returns, with no post-stop cleanup that could race the release.
	fs.BoolVar(&f.leaderElectReleaseOnCancel, "leader-elect-release-on-cancel", true,
		"Release the leader lease on graceful shutdown for faster failover. "+
			"Only safe when the process exits promptly after the manager stops.")
	fs.BoolVar(&f.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	fs.StringVar(&f.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	fs.StringVar(&f.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	fs.StringVar(&f.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	fs.StringVar(&f.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	fs.StringVar(&f.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	fs.StringVar(&f.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	fs.BoolVar(&f.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	fs.BoolVar(&f.allowAgcExtraEnv, "allow-agc-extra-env", false,
		"Forward AGC_EXTRA_* environment variables from the GMC pod to AGC Deployments. Intended for testing only.")
	fs.BoolVar(&f.allowFloatingImageTags, "allow-floating-image-tags", false,
		"Permit non-digest-pinned AGC_IMAGE/PROXY_IMAGE references (floating tags). "+
			"Intended for dev/test only; production requires the name@sha256:<digest> form.")
	fs.BoolVar(&f.enableTenantServiceMonitors, "enable-tenant-service-monitors", false,
		"Create a per-tenant Prometheus-Operator ServiceMonitor for the proxy and AGC mTLS "+
			"metrics ports (:8443). Off by default: requires the monitoring.coreos.com CRD "+
			"(Prometheus Operator) installed. When off, the metrics Services are still created "+
			"but nothing is wired to scrape them. The chart sets this from metrics.serviceMonitor.enabled.")
	fs.StringVar(&f.allowedPriorityClasses, "allowed-priority-classes", "",
		"Comma-separated allowlist of cluster-scoped PriorityClass names that tenant "+
			"RunnerGroups may reference in priorityTiers. The platform admin pre-creates "+
			"these classes and lists them here; the admission webhook rejects any other "+
			"name so a tenant cannot preempt other tenants' worker pods. Empty (default) "+
			"forbids all priorityTiers PriorityClass references.")
	fs.StringVar(&f.allowedInfraPriorityClasses, "allowed-infra-priority-classes", "",
		"Comma-separated allowlist of cluster-scoped PriorityClass names a tenant "+
			"EgressProxy or v2 ActionsGateway may reference in spec.scheduling.priorityClassName "+
			"for its INFRA pods — the proxy pool and the AGC control plane (Q284). An evicted "+
			"proxy takes a tenant's whole egress path down, so these pods need a priority ABOVE "+
			"best-effort workers. MUST be disjoint from --allowed-priority-classes (the worker "+
			"allowlist): the GMC refuses to start if they intersect, because a class nameable "+
			"from both surfaces would let a tenant lift its WORKERS to infra priority and preempt "+
			"other tenants' proxy pods. Empty (default) forbids all "+
			"spec.scheduling.priorityClassName references. Augmented at runtime by the "+
			"PriorityClassAllowlist CR's allowedInfraPriorityClasses (Q298), which the GMC "+
			"refuses to apply if it would intersect the worker allowlist.")
	fs.StringVar(&f.priorityClassAllowlistName, "priority-class-allowlist-name", "",
		"Name of the cluster-scoped PriorityClassAllowlist CR whose "+
			"spec.allowedPriorityClasses and spec.allowedInfraPriorityClasses AUGMENT the "+
			"--allowed-priority-classes and --allowed-infra-priority-classes flag allowlists "+
			"respectively, watched so additions take effect without a GMC restart (Q188, Q298). "+
			"The two effective sets must stay disjoint: a CR that would make them intersect is "+
			"refused wholesale, leaving both flag allowlists in force. "+
			"The same object is the priorityclass-allowlist-guard policy's paramKind, so "+
			"the webhook and the policy read one source and cannot drift. Additive and "+
			"fail-safe: a missing or malformed object leaves the static flag allowlist in "+
			"force. Empty (default) disables the watch — flag-only behavior, unchanged. "+
			"Replaced --priority-class-allowlist-configmap in Q492: a ConfigMap paramKind "+
			"is destroyed by the Q444 apiserver defect.")
	fs.StringVar(&f.allowedEgressFQDNs, "allowed-egress-fqdns", "",
		"Comma-separated allowlist of FQDN host suffixes a tenant EgressProxy may "+
			"request in spec.destinationFQDNs (Q242 G.1). A request matches if it equals "+
			"or is a subdomain of an entry (allowing golang.org permits proxy.golang.org). "+
			"The admission webhook rejects any destinationFQDNs entry not covered here. "+
			"GitHub is always allowed implicitly; empty (default) forbids all non-GitHub "+
			"FQDN destinations.")
	fs.StringVar(&f.allowedEgressCIDRs, "allowed-egress-cidrs", "",
		"Comma-separated allowlist of CIDR ranges a tenant EgressProxy may request in "+
			"spec.destinationCIDRs (Q242 G.1). A request matches by subnet containment "+
			"(allowing 10.0.0.0/8 permits a requested 10.1.0.0/16). The admission webhook "+
			"rejects any destinationCIDRs entry not contained here. Each entry must be a "+
			"valid CIDR; a malformed value fails startup. Empty (default) forbids all "+
			"non-GitHub CIDR destinations.")
	fs.StringVar(&f.egressDestinationAllowlistConfigMap, "egress-destination-allowlist-configmap", "",
		"Name of a ConfigMap in the GMC's own namespace whose entries AUGMENT the "+
			"--allowed-egress-fqdns/--allowed-egress-cidrs flag allowlists, watched so "+
			"additions take effect without a GMC restart (Q242 G.1). The ConfigMap's "+
			"data."+controller.EgressDestinationFQDNsConfigMapKey+" and data."+
			controller.EgressDestinationCIDRsConfigMapKey+" values list FQDN suffixes and "+
			"CIDRs (comma/newline-separated). Additive and fail-safe: a missing or "+
			"malformed ConfigMap leaves the static flag allowlists in force. Empty "+
			"(default) disables the watch — flag-only behavior.")
	fs.StringVar(&f.fqdnPolicyBackend, "fqdn-policy-backend", string(controller.FQDNBackendNone),
		"CNI/platform mechanism the GMC uses to enforce an FQDN egressPolicyMode intent "+
			"(Q245): none|cilium|calico|gke. This is the operator's install-wide choice — a "+
			"tenant expresses intent (egressPolicyMode: FQDN) and this flag picks how it is "+
			"enforced (cilium=CiliumNetworkPolicy, calico=Calico NetworkPolicy, gke=GKE "+
			"Dataplane V2 FQDNNetworkPolicy). The secure default 'none' declares no backend, "+
			"so an EgressProxy requesting FQDN intent is rejected at admission rather than "+
			"silently degrading. The deprecated CiliumFQDN/CalicoFQDN modes ignore this flag "+
			"and always emit their namesake policy. An unrecognized value fails startup.")
	fs.StringVar(&f.apiServerCIDRs, "apiserver-cidrs", "",
		"Comma-separated CIDR allowlist for the AGC NetworkPolicy's Kubernetes API server "+
			"(443/6443) egress rule. When set, the rule is scoped to these CIDRs (ipBlock) "+
			"instead of allowing any destination — an opt-in tightening for platforms whose "+
			"apiserver endpoint exposes a stable CIDR (Q145). Empty (default) keeps the "+
			"any-destination rule, required where the post-DNAT apiserver IP is not "+
			"predictable. Each entry must be a valid CIDR; a malformed value fails startup.")
	return f
}
