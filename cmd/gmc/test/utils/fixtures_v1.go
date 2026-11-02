//go:build e2e
// +build e2e

package utils

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// This file holds the v1alpha1 tenant the v1→v2 migration spec starts from. It is
// the only v1 tenant the e2e suite still applies, so it carries only the DinD shape
// that spec migrates (Q414); every other spec applies the v2 TenantFixture.

// DinDSidecarImage is the Docker-in-Docker daemon image the DinD fixture runs. It
// matches the image the dogfood e2e tenant runs in production
// (deploy/templates/privileged-dind, which deploy/dogfood-e2e/overlays/dind consumes),
// so the fixture migrates the same shape a real tenant would.
const DinDSidecarImage = "docker:28-dind"

// v1RunnerGroupFixture describes one v1 RunnerGroup inside a V1TenantFixture.
type v1RunnerGroupFixture struct {
	RunnerLabels []string
	MaxListeners int32
	WorkerImage  string
	// DinD adds a privileged Docker-in-Docker daemon as a NATIVE sidecar and points
	// the runner container at it. See withDinD for why the sidecar shape matters.
	DinD bool
}

// V1TenantFixture is a v1alpha1 tenant — one ActionsGateway with an inline proxy and
// inline runner groups — ready to apply into an e2e namespace.
type V1TenantFixture struct {
	Namespace  string
	Name       string
	SecretName string
	// SecurityProfile is the v1 per-gateway Pod Security Admission profile.
	// "privileged" additionally requires the namespace to carry the platform
	// eligibility grant — see V1PrivilegedTenantNamespaceLabels.
	SecurityProfile  string
	LogLevel         string
	ProxyMinReplicas int32
	ProxyMaxReplicas int32
	NoProxyCIDRs     []string
	RunnerGroups     []v1RunnerGroupFixture
}

// V1DinDTenant is the representative Docker-in-Docker tenant (Q414): a runner
// container driving a privileged `docker:dind` daemon, exactly the shape the dogfood
// e2e tenant runs. A privileged worker shape is the case that distinguishes the
// namespaced RunnerTemplate from the cluster-scoped ClusterRunnerTemplate on the v2
// side, which is what makes it the tenant worth migrating.
//
// The gateway opts into securityProfile: privileged, which is the ONLY configuration
// v1's own admission webhook admits a privileged worker container under — and it
// additionally requires the namespace to carry the platform eligibility grant, so
// create the namespace with V1PrivilegedTenantNamespaceLabels().
func V1DinDTenant(ns, name, secretName, runnerImage string) V1TenantFixture {
	return V1TenantFixture{
		Namespace:        ns,
		Name:             name,
		SecretName:       secretName,
		SecurityProfile:  "privileged",
		LogLevel:         "debug",
		ProxyMinReplicas: 1,
		ProxyMaxReplicas: 3,
		NoProxyCIDRs:     []string{"svc.cluster.local"},
		RunnerGroups: []v1RunnerGroupFixture{{
			RunnerLabels: []string{"e2e"},
			MaxListeners: 2,
			WorkerImage:  runnerImage,
			DinD:         true,
		}},
	}
}

// V1PrivilegedTenantNamespaceLabels returns the namespace labels a privileged v1
// tenant needs, for passing to CreateNamespace. The privileged-profile grant is
// fail-closed and platform-applied: without it the GMC webhook rejects
// securityProfile: privileged, so a DinD tenant applied into an ungranted namespace
// never provisions.
func V1PrivilegedTenantNamespaceLabels() map[string]string {
	return map[string]string{
		gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed,
	}
}

// ApplyWithWebhookRetry renders the fixture and applies it through
// ApplyManifestWithWebhookRetry, failing the spec on error (Q391, Q392).
func (f V1TenantFixture) ApplyWithWebhookRetry() {
	GinkgoHelper()
	Expect(ApplyManifestWithWebhookRetry(f.Manifest())).To(Succeed(),
		"apply v1 ActionsGateway %s/%s", f.Namespace, f.Name)
}

// Manifest renders the fixture to the YAML `kubectl apply` consumes.
func (f V1TenantFixture) Manifest() string {
	GinkgoHelper()
	out, err := yaml.Marshal(f.Object())
	Expect(err).NotTo(HaveOccurred(), "marshal v1 ActionsGateway %s/%s", f.Namespace, f.Name)
	return string(out)
}

// Object builds the typed v1alpha1 ActionsGateway the fixture describes.
func (f V1TenantFixture) Object() *gmcv1alpha1.ActionsGateway {
	groups := make([]agcv1alpha1.RunnerGroupSpec, 0, len(f.RunnerGroups))
	for _, g := range f.RunnerGroups {
		groups = append(groups, g.spec())
	}
	minReplicas, maxReplicas := f.ProxyMinReplicas, f.ProxyMaxReplicas
	return &gmcv1alpha1.ActionsGateway{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gmcv1alpha1.GroupVersion.String(),
			Kind:       "ActionsGateway",
		},
		ObjectMeta: metav1.ObjectMeta{Name: f.Name, Namespace: f.Namespace},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef:    gmcv1alpha1.SecretReference{Name: f.SecretName},
			GitHubURL:       e2eGitHubURL,
			SecurityProfile: f.SecurityProfile,
			LogLevel:        f.LogLevel,
			Proxy: gmcv1alpha1.ProxyConfig{
				NoProxyCIDRs: f.NoProxyCIDRs,
				MinReplicas:  &minReplicas,
				MaxReplicas:  &maxReplicas,
			},
			RunnerGroups: groups,
		},
	}
}

// spec builds the v1 RunnerGroupSpec for one fixture group.
func (g v1RunnerGroupFixture) spec() agcv1alpha1.RunnerGroupSpec {
	spec := agcv1alpha1.RunnerGroupSpec{
		RunnerLabels: g.RunnerLabels,
		MaxListeners: g.MaxListeners,
		WorkerImage:  g.WorkerImage,
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "runner",
					Image: g.WorkerImage,
				}},
			},
		},
	}
	if g.DinD {
		withDinD(&spec.PodTemplate.Spec)
	}
	return spec
}

// withDinD adds the Docker-in-Docker daemon to a worker pod shape and points the
// runner container at it.
//
// The daemon is a NATIVE sidecar — a restartPolicy: Always init container, Kubernetes
// >= 1.29 — not a regular container, and that is load-bearing rather than stylistic: a
// regular sidecar's dockerd never exits, so the pod never completes, the AGC keeps the
// session active, and the worker pool strands against maxWorkers (Q249). As a native
// sidecar, Kubernetes tears dockerd down when the runner container exits. The v2
// RunnerTemplate webhook emits a non-blocking admission warning for the regular-
// container form, so getting this wrong in a fixture would also make every migrated
// manifest carry a spurious warning.
//
// The daemon runs privileged, which is why a DinD tenant needs securityProfile:
// privileged and the platform eligibility grant on its namespace — and why, on the v2
// side, its worker shape can only live on a cluster-scoped ClusterRunnerTemplate.
func withDinD(pod *corev1.PodSpec) {
	privileged := true
	always := corev1.ContainerRestartPolicyAlways
	pod.InitContainers = append(pod.InitContainers, corev1.Container{
		Name:          "dind",
		Image:         DinDSidecarImage,
		RestartPolicy: &always,
		Args:          []string{"--host=tcp://0.0.0.0:2375", "--tls=false"},
		Env: []corev1.EnvVar{
			// Disable dockerd's automatic TLS: the daemon listens on plain TCP bound to
			// the pod's own network namespace, reachable only by the runner container.
			{Name: "DOCKER_TLS_CERTDIR", Value: ""},
		},
		SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		Resources: corev1.ResourceRequirements{
			// Deliberately small: an e2e fixture tenant never runs a real build. The
			// production sizing is measured per job class and lives in the dogfood
			// overlay (deploy/dogfood-e2e/overlays/dind, Q248), not here.
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	})
	for i := range pod.Containers {
		if pod.Containers[i].Name != "runner" {
			continue
		}
		pod.Containers[i].Env = append(pod.Containers[i].Env, corev1.EnvVar{
			Name: "DOCKER_HOST", Value: "tcp://localhost:2375",
		})
	}
}
