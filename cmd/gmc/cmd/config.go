package main

import (
	"crypto/tls"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
)

// resolvedConfig holds the validated, live objects derived from the GMC's flags
// and environment that the manager, controllers, and webhooks all consume. It is
// produced once by resolveConfig so the cross-flag validation (allowlist
// disjointness, CIDR/FQDN parsing, ConfigMap-informer scoping) lives in one
// test-reachable place rather than inline in main (F8 / Q367). The allowlists are
// the SAME live instances wired into both the reconcilers and the admission
// webhooks — the ConfigMap watches mutate them in place.
type resolvedConfig struct {
	priorityClassAllowlist      *allowlist.PriorityClassAllowlist
	infraPriorityClassAllowlist *allowlist.PriorityClassAllowlist
	egressDestinationAllowlist  *allowlist.EgressDestinationAllowlist
	fqdnBackend                 controller.FQDNBackend
	podNamespace                string
	cacheOptions                cache.Options
}

// resolveConfig validates the GMC's cross-flag configuration and builds the live
// objects the rest of startup consumes: construct
// both PriorityClass allowlists and reject an intersection (a class nameable from
// both a worker and an infra pod would let a tenant lift its workers to infra
// priority and preempt other tenants' proxies, Q284); parse the egress-CIDR
// allowlist and build the egress-destination allowlist (Q242 G.1); parse the FQDN
// backend (Q245); then scope the ConfigMap informer cache to the GMC's own
// namespace when either allowlist watch is enabled (Q188/Q242 G.1). getenv reads
// POD_NAMESPACE (os.Getenv in production; a fake in tests). Any malformed value
// returns an error so the GMC fails closed at startup.
func resolveConfig(cfg *gmcFlags, getenv func(string) string) (*resolvedConfig, error) {
	// The PriorityClass allowlist (Q188): a live, shared set the admission webhook
	// reads and the ConfigMap watch augments. Seeded from the static
	// --allowed-priority-classes flag; the dynamic half starts empty until a
	// ConfigMap is applied.
	priorityClassAllowlist := allowlist.New(parseAllowedPriorityClasses(cfg.allowedPriorityClasses))

	// The infra PriorityClass allowlist (Q284): gates spec.scheduling.priorityClassName
	// on the EgressProxy and v2 ActionsGateway INFRA pods, seeded from the static
	// --allowed-infra-priority-classes flag. It is a SEPARATE instance from the worker
	// allowlist and MUST be disjoint from it. A boot-time intersection check converts
	// that silent priority inversion into a hard startup failure.
	infraPriorityClassAllowlist := allowlist.New(parseAllowedPriorityClasses(cfg.allowedInfraPriorityClasses))
	if shared := allowlist.Intersection(priorityClassAllowlist, infraPriorityClassAllowlist); len(shared) > 0 {
		return nil, fmt.Errorf("PriorityClass allowlists intersect (%v): --allowed-priority-classes (worker) and "+
			"--allowed-infra-priority-classes must be disjoint: a class on both would let a tenant lift its worker "+
			"pods to infra priority and preempt other tenants' proxy pods", shared)
	}
	// Both allowlists also take a watched dynamic half (Q188/Q298), so the flags are
	// no longer the only route to an overlap. Pairing makes the invariant hold at
	// READ time: a name that reaches both sets is admitted by neither webhook,
	// whatever let it through.
	allowlist.Pair(priorityClassAllowlist, infraPriorityClassAllowlist)

	// The platform egress destination allowlist (Q242 G.1): the EgressProxy admission
	// webhook reads it and the ConfigMap watch augments it. A malformed
	// --allowed-egress-cidrs value fails startup rather than silently dropping a
	// guardrail entry.
	egressCIDRs, err := parseAllowedEgressCIDRs(cfg.allowedEgressCIDRs)
	if err != nil {
		return nil, fmt.Errorf("invalid --allowed-egress-cidrs: %w", err)
	}
	egressDestinationAllowlist := allowlist.NewEgressDestination(
		parseAllowedPriorityClasses(cfg.allowedEgressFQDNs), egressCIDRs)

	// The FQDN egress backend (Q245): the operator's install-wide choice of how an
	// FQDN egressPolicyMode intent is enforced. An unrecognized value fails startup.
	fqdnBackend, err := controller.ParseFQDNBackend(cfg.fqdnPolicyBackend)
	if err != nil {
		return nil, fmt.Errorf("invalid --fqdn-policy-backend: %w", err)
	}

	podNamespace := getenv("POD_NAMESPACE")

	cacheOptions, err := buildCacheOptions(cfg, podNamespace)
	if err != nil {
		return nil, err
	}

	return &resolvedConfig{
		priorityClassAllowlist:      priorityClassAllowlist,
		infraPriorityClassAllowlist: infraPriorityClassAllowlist,
		egressDestinationAllowlist:  egressDestinationAllowlist,
		fqdnBackend:                 fqdnBackend,
		podNamespace:                podNamespace,
		cacheOptions:                cacheOptions,
	}, nil
}

// buildCacheOptions narrows the informers the two watched-allowlist features need
// (Q188, Q242 G.1) to the single object each reads, so the GMC needs no broad read
// grant for either.
//
//   - The egress destination allowlist is a ConfigMap in the GMC's own namespace:
//     the ConfigMap informer is scoped to that namespace and pinned to that one
//     name, so the GMC needs only namespaced get/list/watch, not cluster-wide
//     ConfigMap read. A watch with no POD_NAMESPACE to locate it is a hard startup
//     failure.
//   - The PriorityClass allowlist is a cluster-scoped PriorityClassAllowlist CR
//     (Q492, replacing the ConfigMap that Q444's apiserver defect made unusable as
//     a VAP paramKind). Being cluster-scoped it needs no namespace, only a
//     name-pinned field selector.
//
// With neither feature enabled no extra informer is ever started, so this returns
// empty options (a no-op).
func buildCacheOptions(cfg *gmcFlags, podNamespace string) (cache.Options, error) {
	cacheOptions := cache.Options{}
	byObject := map[client.Object]cache.ByObject{}

	if cfg.egressDestinationAllowlistConfigMap != "" {
		if podNamespace == "" {
			return cacheOptions, fmt.Errorf("--egress-destination-allowlist-configmap requires " +
				"POD_NAMESPACE (the GMC install namespace) to locate the ConfigMap")
		}
		byObject[&corev1.ConfigMap{}] = cache.ByObject{
			Namespaces: map[string]cache.Config{
				podNamespace: {
					FieldSelector: fields.OneTermEqualSelector("metadata.name",
						cfg.egressDestinationAllowlistConfigMap),
				},
			},
		}
	}

	if cfg.priorityClassAllowlistName != "" {
		byObject[&v2beta1.PriorityClassAllowlist{}] = cache.ByObject{
			Field: fields.OneTermEqualSelector("metadata.name", cfg.priorityClassAllowlistName),
		}
	}

	if len(byObject) > 0 {
		cacheOptions.ByObject = byObject
	}
	return cacheOptions, nil
}

// gmcImages holds the container-image configuration the GMC injects into the
// per-tenant gateways it provisions.
type gmcImages struct {
	agcImage    string
	proxyImage  string
	agcExtraEnv []corev1.EnvVar
}

// resolveImages reads and validates the GMC's image configuration from the
// environment (AGC_IMAGE/PROXY_IMAGE are required; WRAPPER_IMAGE is optional) and
// assembles the AGC_EXTRA_* env forwarded to AGC Deployments. getenv/environ are
// injected (os.Getenv/os.Environ in production; fakes in tests). Unless
// --allow-floating-image-tags is set, every injected image must be pinned by
// sha256 digest so a mutated tag cannot silently swap the code that runs inside a
// tenant's gateway (supply-chain hardening).
func resolveImages(cfg *gmcFlags, getenv func(string) string, environ func() []string) (gmcImages, error) {
	agcImage, err := mustEnv(getenv, "AGC_IMAGE")
	if err != nil {
		return gmcImages{}, err
	}
	proxyImage, err := mustEnv(getenv, "PROXY_IMAGE")
	if err != nil {
		return gmcImages{}, err
	}

	if cfg.allowFloatingImageTags {
		setupLog.Info("WARNING: --allow-floating-image-tags is set; AGC_IMAGE/PROXY_IMAGE digest pinning is NOT enforced (do not use in production)")
	} else {
		for _, img := range []struct{ name, ref string }{
			{"AGC_IMAGE", agcImage},
			{"PROXY_IMAGE", proxyImage},
		} {
			if err := validateImageDigest(img.name, img.ref); err != nil {
				return gmcImages{}, fmt.Errorf("image reference is not digest-pinned: %w", err)
			}
		}
	}

	// AGC_EXTRA_<NAME>=<VALUE> env vars on the GMC pod are forwarded verbatim to
	// each AGC Deployment the controller creates. Gate-flagged to prevent
	// accidental capability escalation in production deployments.
	// Sorted by name below: the slice lands in the AGC pod template, whose hash
	// decides whether a tenant's control plane rolls, and os.Environ's order is
	// unspecified (Q587).
	var agcExtraEnv []corev1.EnvVar
	if cfg.allowAgcExtraEnv {
		for _, kv := range environ() {
			const prefix = "AGC_EXTRA_"
			if !strings.HasPrefix(kv, prefix) {
				continue
			}
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 {
				agcExtraEnv = append(agcExtraEnv, corev1.EnvVar{
					Name:  strings.TrimPrefix(parts[0], prefix),
					Value: parts[1],
				})
			}
		}
		slices.SortFunc(agcExtraEnv, func(a, b corev1.EnvVar) int { return strings.Compare(a.Name, b.Name) })
	}

	// WRAPPER_IMAGE (optional) enables runtime worker-wrapper injection (Q235):
	// forwarded to every AGC so its provisioner injects the GAG wrapper into each
	// worker pod, letting the runner image be the unmodified upstream
	// actions-runner. Empty disables injection. Digest-pinned like AGC_IMAGE unless
	// floating tags are allowed. WRAPPER_DELIVERY (optional: imagevolume|init)
	// overrides the AGC's version-based auto-detection.
	if wrapperImage := getenv("WRAPPER_IMAGE"); wrapperImage != "" {
		if !cfg.allowFloatingImageTags {
			if err := validateImageDigest("WRAPPER_IMAGE", wrapperImage); err != nil {
				return gmcImages{}, fmt.Errorf("image reference is not digest-pinned: %w", err)
			}
		}
		agcExtraEnv = append(agcExtraEnv, corev1.EnvVar{Name: "WRAPPER_IMAGE", Value: wrapperImage})
		if d := getenv("WRAPPER_DELIVERY"); d != "" {
			agcExtraEnv = append(agcExtraEnv, corev1.EnvVar{Name: "WRAPPER_DELIVERY", Value: d})
		}
	}

	return gmcImages{agcImage: agcImage, proxyImage: proxyImage, agcExtraEnv: agcExtraEnv}, nil
}

// tlsOptions returns the TLS mutators applied to the metrics and webhook servers.
// HTTP/2 is disabled unless --enable-http2 is set, to avoid the HTTP/2 Stream
// Cancellation and Rapid Reset CVEs (GHSA-qppj-fm5r-hxr3, GHSA-4374-p667-p6c8).
func tlsOptions(enableHTTP2 bool) []func(*tls.Config) {
	var tlsOpts []func(*tls.Config)
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}
	return tlsOpts
}

// disableHTTP2 forces the connection down to HTTP/1.1.
func disableHTTP2(c *tls.Config) {
	setupLog.Info("Disabling HTTP/2")
	c.NextProtos = []string{"http/1.1"}
}

// newWebhookServer builds the admission/conversion webhook server, wiring the
// operator-provided certificate directory when --webhook-cert-path is set.
func newWebhookServer(cfg *gmcFlags, tlsOpts []func(*tls.Config)) webhook.Server {
	webhookServerOptions := webhook.Options{TLSOpts: tlsOpts}
	if len(cfg.webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", cfg.webhookCertPath, "webhook-cert-name", cfg.webhookCertName,
			"webhook-cert-key", cfg.webhookCertKey)
		webhookServerOptions.CertDir = cfg.webhookCertPath
		webhookServerOptions.CertName = cfg.webhookCertName
		webhookServerOptions.KeyName = cfg.webhookCertKey
	}
	return webhook.NewServer(webhookServerOptions)
}

// newMetricsServerOptions configures the controller-runtime metrics server. When
// --metrics-secure is set (the default) the endpoint is served over HTTPS behind
// the authn/authz FilterProvider (the metrics-auth RBAC the chart ships). When
// --metrics-cert-path is set the server uses the operator-provided certificate;
// otherwise controller-runtime self-signs (dev/test only).
func newMetricsServerOptions(cfg *gmcFlags, tlsOpts []func(*tls.Config)) metricsserver.Options {
	metricsServerOptions := metricsserver.Options{
		BindAddress:   cfg.metricsAddr,
		SecureServing: cfg.secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if cfg.secureMetrics {
		// FilterProvider protects the metrics endpoint with authn/authz so only
		// authorized users and service accounts can read it.
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if len(cfg.metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", cfg.metricsCertPath, "metrics-cert-name", cfg.metricsCertName,
			"metrics-cert-key", cfg.metricsCertKey)
		metricsServerOptions.CertDir = cfg.metricsCertPath
		metricsServerOptions.CertName = cfg.metricsCertName
		metricsServerOptions.KeyName = cfg.metricsCertKey
	}
	return metricsServerOptions
}
