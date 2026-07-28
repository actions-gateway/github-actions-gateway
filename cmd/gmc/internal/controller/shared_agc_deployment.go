package controller

import (
	"net"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The AGC control-plane Deployment assembly, shared by the v1 and v2
// ActionsGateway reconcilers. Both versions mount credentials as files (never env),
// pin the proxy CA, mount the metrics mTLS bundle, and run the same hardened
// container; they differ only in the derived names, metadata labels, credential
// Secret name, env list, and container resources, all of which are parameters here.

const (
	// agcTenantRoleName is the shipped singleton ClusterRole that defines
	// the AGC permission set. Per-tenant RoleBindings reference it; the GMC
	// holds `bind` on this exact name so it never needs `escalate` or to
	// hold the AGC's full permission set itself.
	agcTenantRoleName = "agc-tenant-role"

	// agcCredsVolumeName / agcCredsMountPath define how the GitHub App Secret is
	// projected into the AGC pod. Keys (appId, installationId, privateKey) are
	// mounted as read-only files; no credential ever appears in an env var.
	agcCredsVolumeName = "github-app-credentials"          //nolint:gosec // G101: a volume name, not a credential
	agcCredsMountPath  = "/etc/actions-gateway/github-app" //nolint:gosec // G101: a mount-path constant, not a credential

	// Workload-identity (Q201): the AGC presents a kubelet-projected ServiceAccount
	// token to Vault Kubernetes auth instead of mounting a GitHub App Secret. The
	// token is audience-scoped to Vault and read fresh from disk at each Vault login;
	// it is never a stored Secret. vaultTokenAudience is the projected token's
	// audience — operators bind their Vault k8s-auth role to it (the MVP fixes it; a
	// configurable audience is a documented follow-up).
	vaultTokenVolumeName = "vault-token"
	vaultTokenMountDir   = "/var/run/secrets/actions-gateway/vault-token" //nolint:gosec // G101: a mount-path constant, not a credential
	vaultTokenFile       = "token"
	vaultTokenAudience   = "vault"
	// vaultTokenExpirationSeconds is the projected token's lifetime. The kubelet
	// rotates it well before expiry and the vaultsigner re-reads it on each login, so
	// a short lifetime bounds the blast radius of a leaked token without risking a
	// stale read.
	vaultTokenExpirationSeconds int64 = 600

	// defaultNoProxy is the static half of the AGC's cluster-internal proxy
	// exclusions. NO_PROXY is an egress-control surface — everything listed here
	// leaves the tenant's egress proxy unused, and so escapes the per-tenant
	// egress-IP attribution that isolates tenants (§H.8) — so the list is kept to
	// destinations that are cluster-internal by construction:
	//
	//   - svc.cluster.local     every in-cluster Service reached by FQDN (the
	//                           EgressProxy Service itself, fakegithub in e2e).
	//   - kubernetes.default.svc the API server's short in-cluster DNS name, which
	//                           svc.cluster.local does not cover on a cluster whose
	//                           cluster domain is not "cluster.local".
	//   - localhost/127.0.0.1   the pod's own loopback.
	//
	// The API server's ClusterIP is deliberately absent: it is per-cluster, so
	// apiServerNoProxyEntry derives it instead of guessing a Service CIDR (Q465).
	defaultNoProxy = "kubernetes.default.svc,svc.cluster.local,localhost,127.0.0.1"

	// apiServerHostEnv is the API server ClusterIP env var the kubelet injects into
	// every pod from the "kubernetes" Service in the default namespace, regardless
	// of the pod's enableServiceLinks setting.
	apiServerHostEnv = "KUBERNETES_SERVICE_HOST"
)

// apiServerNoProxyEntry returns the NO_PROXY entry that keeps the AGC's Kubernetes
// API traffic off the tenant's egress proxy.
//
// client-go's in-cluster config dials the API server by ClusterIP (read from
// KUBERNETES_SERVICE_HOST), never by DNS name, so the DNS entries in
// defaultNoProxy do not cover it: a proxied AGC CONNECTs to the API server through
// the tenant proxy, cannot verify the proxy's CA, and CrashLoopBackOffs at startup.
// That ClusterIP is the first address of the cluster's Service CIDR and differs per
// distribution — 10.96.0.1 on kind/kubeadm, 172.20.0.1 on EKS, 10.0.0.1 on AKS,
// 34.118.224.1 on the GKE cluster where this was measured — so the exemption must
// come from the cluster rather than from a hardcoded range (Q465).
//
// The GMC provisions AGCs into its own cluster, so the ClusterIP it reads from its
// own environment is the one the AGC pod will dial. When the variable is unset (a
// GMC run out-of-cluster: `make run`, envtest) the literal "$(KUBERNETES_SERVICE_HOST)"
// is emitted instead and the kubelet expands it from the AGC pod's own service env,
// which yields the same value at pod start.
//
// An IPv6 ClusterIP is bracketed: Go's NO_PROXY parser reads a bare "fd00::1" as
// host "fd00:" port "1" and the entry would never match.
func apiServerNoProxyEntry() string {
	host := strings.TrimSpace(os.Getenv(apiServerHostEnv))
	if host == "" {
		return "$(" + apiServerHostEnv + ")"
	}
	if strings.HasPrefix(host, "[") {
		return host
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "[" + host + "]"
	}
	return host
}

// buildNoProxy merges user-provided CIDRs with the mandatory cluster-internal
// exclusions: the static defaultNoProxy names plus this cluster's API server
// address (apiServerNoProxyEntry). User entries come first, matching the order
// operators see today; NO_PROXY matching is order-independent.
func buildNoProxy(userCIDRs []string) string {
	mandatory := defaultNoProxy + "," + apiServerNoProxyEntry()
	if len(userCIDRs) > 0 {
		return strings.Join(userCIDRs, ",") + "," + mandatory
	}
	return mandatory
}

// agcWorkloadNames carries the per-gateway derived names for the AGC
// control-plane Deployment, ServiceAccount, and metrics Secret. v1 (one gateway
// per namespace) passes the fixed singleton names; v2 (multi-gateway, M3b)
// derives them per ActionsGateway so two gateways in one namespace never collide
// on a fixed name (§H.16 #1). The `app` value is load-bearing three ways — it is
// the Deployment name, the pod `app` label value, and the AGC NetworkPolicy and
// Service selector — so all of them select exactly this gateway's AGC pods and
// two AGC Deployments never adopt each other's pods.
type agcWorkloadNames struct {
	app              string
	serviceAccount   string
	metricsTLSSecret string
}

// buildAGCDeploymentFrom assembles the AGC Deployment pod spec shared by the v1
// and v2 ActionsGateway reconcilers: the GitHub App credential file mount (never
// env), the proxy CA pin (public cert only), the metrics mTLS mount, the hardened
// container/pod SecurityContext, and the probes. The callers differ only in the
// metadata labels, the derived resource names (v1 fixed, v2 per-gateway), the
// credential/proxy-CA Secret names, the env list, and the container resources,
// which are passed in. metaLabels are the Deployment's metadata labels; the
// pod-template `app` label is names.app so it matches the AGC NetworkPolicy and
// Service selectors. resources is the AGC container's resource requirements — the
// zero value (v1) leaves the container without requests/limits, unchanged from
// before; v2 passes the platform default overlaid with spec.agcResources (Q171).
func buildAGCDeploymentFrom(namespace string, names agcWorkloadNames, metaLabels map[string]string, credSecretName, proxyTLSSecret, agcImage string, env []corev1.EnvVar, resources corev1.ResourceRequirements) *appsv1.Deployment {
	// 0o440 + fsGroup 65532 — see the matching block in buildProxyDeployment for
	// why 0o400 alone leaves the file unreadable to the non-root AGC user.
	credMode := int32(0o440)
	caMode := int32(0o444)

	volumes := []corev1.Volume{
		{
			// Metrics mTLS server bundle (ca.crt + tls.crt + tls.key). The AGC's
			// controller-runtime metrics server serves /metrics over mTLS on metricsPort
			// and verifies scraper client certs against ca.crt.
			Name: metricsTLSVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  names.metricsTLSSecret,
					DefaultMode: &credMode,
				},
			},
		},
	}
	volumeMounts := []corev1.VolumeMount{
		{Name: metricsTLSVolumeName, MountPath: metricsTLSMountPath, ReadOnly: true},
	}

	// Credential wiring, by union member (Q196/Q197/Q201):
	//   - possession (githubApp): credSecretName names the GitHub App Secret; mount
	//     its appId/installationId/privateKey as read-only files (never env).
	//   - delegation (workloadIdentity): credSecretName is "", so no Secret exists to
	//     mount. The AGC instead presents a kubelet-projected ServiceAccount token to
	//     Vault; project it audience-scoped to Vault, read-only. No App key, ever.
	if credSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: agcCredsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  credSecretName,
					DefaultMode: &credMode,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: agcCredsVolumeName, MountPath: agcCredsMountPath, ReadOnly: true,
		})
	} else {
		volumes = append(volumes, corev1.Volume{
			Name: vaultTokenVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					DefaultMode: &credMode,
					Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          vaultTokenAudience,
							ExpirationSeconds: ptr(vaultTokenExpirationSeconds),
							Path:              vaultTokenFile,
						},
					}},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: vaultTokenVolumeName, MountPath: vaultTokenMountDir, ReadOnly: true,
		})
	}
	// Proxy CA cert (public part only — private key excluded via Items). The AGC pins
	// the proxy's TLS cert rather than trusting the cluster CA, preventing MITM even
	// from a compromised cluster CA. Mounted only when egress is proxied: a v2 gateway
	// with no defaultProxyRef egresses directly (§H.10), has no proxy TLS Secret, and
	// mounting a non-existent Secret would wedge the pod at ContainerCreating.
	if proxyTLSSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: proxyCACertVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  proxyTLSSecret,
					Items:       []corev1.KeyToPath{{Key: corev1.TLSCertKey, Path: corev1.TLSCertKey}},
					DefaultMode: &caMode,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: proxyCACertVolumeName, MountPath: proxyCACertMountPath, ReadOnly: true,
		})
	}

	// Record the referenced GitHub App Secret name so kubectl rollout history shows
	// the cause of any credential-rotation rolling update. Omitted for workload
	// identity (credSecretName ""), which holds no Secret to rotate.
	var podAnnotations map[string]string
	if credSecretName != "" {
		podAnnotations = map[string]string{"actions-gateway/github-app-secret": credSecretName}
	}

	// "app"/"actions-gateway/component: workload" are the functional selectors; the
	// recommended app.kubernetes.io/* metadata is carried over from the Deployment's
	// metaLabels additively (works for both the v1 and v2 callers, whose metaLabels
	// already carry the per-gateway instance).
	podLabels := map[string]string{"app": names.app, labelManagedBy: labelManagerValue, labelComponent: componentWorkload}
	copyRecommendedLabels(podLabels, metaLabels)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: names.app, Namespace: namespace, Labels: metaLabels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": names.app}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: names.serviceAccount,
					SecurityContext:    nonrootPodSecurityContext(),
					// 60s lets the AGC's signal handler (ctrl.SetupSignalHandler in
					// cmd/agc/main.go) drain in-flight session work and release its
					// listener-renewal lock cleanly on rollout instead of losing the
					// lock to a SIGKILL.
					TerminationGracePeriodSeconds: ptr(int64(60)),
					Volumes:                       volumes,
					Containers: []corev1.Container{{
						Name:      agcContainerName,
						Image:     agcImage,
						Env:       env,
						Resources: resources,
						// The AGC pins its controller-runtime metrics server to
						// metricsPort and its health server to healthMetricsPort
						// (cmd/agc/main.go); declaring the ports documents the
						// listeners — metrics is the one buildAGCNetworkPolicy's
						// metrics-scrape ingress rule admits, health is kubelet-only.
						Ports: []corev1.ContainerPort{
							{Name: "health", ContainerPort: healthMetricsPort, Protocol: corev1.ProtocolTCP},
							{Name: "metrics", ContainerPort: metricsPort, Protocol: corev1.ProtocolTCP},
						},
						VolumeMounts: volumeMounts,
						// StartupProbe gives the AGC manager's informer cache room to
						// sync before liveness takes over (30 × 5s = 150s), mirroring
						// the GMC manager's probe. The AGC binds its health listener
						// early in mgr.Start — independently of the initial GitHub App
						// token fetch, which runs as a manager Runnable (see
						// cmd/agc/main.go) — so this budget covers cache sync, not the
						// token exchange.
						StartupProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(healthMetricsPort)},
							},
							FailureThreshold: 30,
							PeriodSeconds:    5,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(healthMetricsPort)},
							},
							PeriodSeconds: 20,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(healthMetricsPort)},
							},
							PeriodSeconds: 10,
						},
						SecurityContext: hardenedContainerSecurityContext(),
					}},
				},
			},
		},
	}
}
