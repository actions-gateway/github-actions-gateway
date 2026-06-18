package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// validatorWithNamespaces returns a validator whose API reader is a fake client
// preloaded with the given namespaces, so the privileged-eligibility gate
// (validatePrivilegedEligibility) can be exercised in unit tests without a live
// apiserver. Production wires mgr.GetAPIReader(); these tests wire a fake.
func validatorWithNamespaces(t *testing.T, namespaces ...*corev1.Namespace) *ActionsGatewayCustomValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, gmcv1alpha1.AddToScheme(scheme))
	objs := make([]client.Object, 0, len(namespaces))
	for _, ns := range namespaces {
		objs = append(objs, ns)
	}
	v := NewActionsGatewayCustomValidator("", nil)
	v.reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return v
}

// namespaceWithLabels builds a corev1.Namespace carrying the given labels.
func namespaceWithLabels(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// captureSink is a minimal logr.LogSink that records Info/Error lines emitted
// during a test, so the rejection-audit log can be asserted without a live
// logging backend.
type captureSink struct{ lines *[]string }

func (s captureSink) Init(logr.RuntimeInfo)          {}
func (s captureSink) Enabled(int) bool               { return true }
func (s captureSink) WithName(string) logr.LogSink   { return s }
func (s captureSink) WithValues(...any) logr.LogSink { return s }
func (s captureSink) Error(_ error, msg string, kv ...any) {
	*s.lines = append(*s.lines, fmt.Sprint(append([]any{msg}, kv...)...))
}
func (s captureSink) Info(_ int, msg string, kv ...any) {
	*s.lines = append(*s.lines, fmt.Sprint(append([]any{msg}, kv...)...))
}

// ctxWithCapture returns a context carrying a capturing logr logger plus the
// slice that logger appends to.
func ctxWithCapture() (context.Context, *[]string) {
	lines := &[]string{}
	return logf.IntoContext(context.Background(), logr.New(captureSink{lines: lines})), lines
}

func newAG(namespace string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: namespace},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "github-app"},
			GitHubURL:    "https://github.com/example-org",
		},
	}
}

func TestWebhook_RejectsKubeSystem(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateCreate(context.Background(), newAG("kube-system"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-system")
}

func TestWebhook_RejectsKubePublic(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateCreate(context.Background(), newAG("kube-public"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-public")
}

// TestWebhook_RejectsDefaultGMCNamespace covers the default install namespace.
// Even when the GMC has no POD_NAMESPACE env var (e.g. `make run` outside a
// pod, or a misconfigured Deployment that drops the downward-API mapping),
// `gmc-system` must still be reserved as a backstop.
func TestWebhook_RejectsDefaultGMCNamespace(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateCreate(context.Background(), newAG("gmc-system"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gmc-system")
}

// TestWebhook_RejectsCustomInstallNamespace covers a non-default install
// (e.g. an operator deployed to `actions-gateway-operator`). The downward
// API supplies the install namespace and the webhook must reject CRs in it.
func TestWebhook_RejectsCustomInstallNamespace(t *testing.T) {
	v := NewActionsGatewayCustomValidator("actions-gateway-operator", nil)
	_, err := v.ValidateCreate(context.Background(), newAG("actions-gateway-operator"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "actions-gateway-operator")
}

// A rejected create must leave a server-side audit line naming the operation,
// namespace, and reason — the GMC otherwise keeps no trail of reserved-namespace
// or privileged-container attempts (Q88, logging-audit Theme E).
func TestWebhook_RejectionIsAudited(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ctx, lines := ctxWithCapture()
	_, err := v.ValidateCreate(ctx, newAG("kube-system"))
	require.Error(t, err)

	joined := strings.Join(*lines, "\n")
	assert.Contains(t, joined, "admission denied")
	assert.Contains(t, joined, "kube-system")
	assert.Contains(t, joined, "create")
}

func TestWebhook_AllowsTenantNamespace(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateCreate(context.Background(), newAG("team-a"))
	require.NoError(t, err)
}

func TestWebhook_UpdateAllowsSafe(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateUpdate(context.Background(), newAG("team-a"), newAG("team-a"))
	require.NoError(t, err)
}

// ptr returns a pointer to v — helper for SecurityContext fields.
func ptr[T any](v T) *T { return &v }

func agWithPrivilegedContainer(privileged bool) *gmcv1alpha1.ActionsGateway {
	ag := newAG("team-a")
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{
		{
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "runner",
							Image: "runner:latest",
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr(privileged),
							},
						},
					},
				},
			},
		},
	}
	return ag
}

func agWithPrivilegedInitContainer() *gmcv1alpha1.ActionsGateway {
	ag := newAG("team-a")
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{
		{
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "runner:latest"}},
					InitContainers: []corev1.Container{
						{
							Name:  "init",
							Image: "busybox",
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr(true),
							},
						},
					},
				},
			},
		},
	}
	return ag
}

func TestWebhook_RejectsCrossNamespaceSecretRef(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.GitHubAppRef.Namespace = "other-namespace"
	_, err := v.ValidateCreate(context.Background(), ag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gitHubAppRef.namespace is not supported")
}

func TestWebhook_AllowsEmptySecretRefNamespace(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	// Namespace is zero value ("") — must be accepted.
	_, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
}

func TestWebhook_UpdateRejectsCrossNamespaceSecretRef(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	updated := newAG("team-a")
	updated.Spec.GitHubAppRef.Namespace = "other-namespace"
	_, err := v.ValidateUpdate(context.Background(), newAG("team-a"), updated)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gitHubAppRef.namespace is not supported")
}

func TestWebhook_RejectsMissingGitHubURL(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.GitHubURL = ""
	_, err := v.ValidateCreate(context.Background(), ag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gitHubURL is required")
}

func TestWebhook_RejectsNonHTTPSGitHubURL(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.GitHubURL = "http://github.com/example-org"
	_, err := v.ValidateCreate(context.Background(), ag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https")
}

func TestWebhook_RejectsGitHubURLWithoutOrgPath(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	// Host only, no organization/owner segment — nothing to register against.
	ag.Spec.GitHubURL = "https://github.com"
	_, err := v.ValidateCreate(context.Background(), ag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "organization")
}

func TestWebhook_AllowsRepoScopedGitHubURL(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.GitHubURL = "https://github.com/example-org/example-repo"
	_, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
}

func TestWebhook_AllowsGHESGitHubURL(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.GitHubURL = "https://ghes.example.com/example-org"
	_, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
}

func TestWebhook_UpdateRejectsInvalidGitHubURL(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	updated := newAG("team-a")
	updated.Spec.GitHubURL = "not-a-url"
	_, err := v.ValidateUpdate(context.Background(), newAG("team-a"), updated)
	require.Error(t, err)
}

func TestWebhook_RejectsPrivilegedContainer(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedContainer(true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privileged containers are not permitted")
}

func TestWebhook_AllowsNonPrivilegedContainer(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedContainer(false))
	require.NoError(t, err)
}

func TestWebhook_RejectsPrivilegedInitContainer(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedInitContainer())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privileged init containers are not permitted")
}

func TestWebhook_UpdateRejectsPrivilegedContainer(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateUpdate(context.Background(), newAG("team-a"), agWithPrivilegedContainer(true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privileged containers are not permitted")
}

// Under the explicit privileged securityProfile, the documented Kata/DinD
// privileged worker pattern must be admitted (Q127): the namespace PSA is
// stamped `privileged` to match, so the webhook no longer rejects it.
func TestWebhook_AllowsPrivilegedContainerUnderPrivilegedProfile(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := agWithPrivilegedContainer(true)
	ag.Spec.SecurityProfile = "privileged"
	_, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
}

// The privileged exemption is keyed strictly on the privileged profile; the
// restricted profile (a hardening) must still reject privileged containers.
func TestWebhook_RejectsPrivilegedContainerUnderRestrictedProfile(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := agWithPrivilegedContainer(true)
	ag.Spec.SecurityProfile = "restricted"
	_, err := v.ValidateCreate(context.Background(), ag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privileged containers are not permitted")
}

// The empty (default) profile maps to baseline and must keep rejecting
// privileged containers — secure by default, no silent opt-in.
func TestWebhook_RejectsPrivilegedContainerUnderDefaultProfile(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := agWithPrivilegedContainer(true)
	ag.Spec.SecurityProfile = "" // defaults to baseline
	_, err := v.ValidateCreate(context.Background(), ag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privileged containers are not permitted")
}

// --- Privileged-profile eligibility gate (Q133) ---------------------------
//
// securityProfile: privileged is eligible only in a namespace a platform admin
// has labelled actions-gateway.github.com/privileged-profile=allowed. The gate is
// fail-closed: absent the label (or a wrong value, or an unreadable namespace),
// privileged is rejected at create AND update. A tenant cannot self-grant it.

// agWithPrivilegedProfile returns an AG in the given namespace requesting
// securityProfile: privileged.
func agWithPrivilegedProfile(namespace string) *gmcv1alpha1.ActionsGateway {
	ag := newAG(namespace)
	ag.Spec.SecurityProfile = "privileged"
	return ag
}

func TestWebhook_RejectsPrivilegedProfileInUnlabeledNamespace(t *testing.T) {
	v := validatorWithNamespaces(t, namespaceWithLabels("team-a", nil))
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedProfile("team-a"))
	require.Error(t, err, "privileged is not eligible without the platform label")
	assert.Contains(t, err.Error(), gmcv1alpha1.PrivilegedProfileLabel)
	assert.Contains(t, err.Error(), "not eligible")
	assert.Contains(t, err.Error(), "not tenant-settable")
}

func TestWebhook_RejectsPrivilegedProfileWithWrongLabelValue(t *testing.T) {
	// A present-but-non-"true" value must not grant eligibility (fail closed).
	v := validatorWithNamespaces(t, namespaceWithLabels("team-a", map[string]string{
		gmcv1alpha1.PrivilegedProfileLabel: "yes",
	}))
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedProfile("team-a"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), gmcv1alpha1.PrivilegedProfileLabel)
}

func TestWebhook_RejectsPrivilegedProfileWhenNamespaceUnreadable(t *testing.T) {
	// The namespace object is absent from the reader: eligibility cannot be
	// confirmed, so the request is rejected (fail closed) rather than admitted.
	v := validatorWithNamespaces(t) // no namespaces preloaded
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedProfile("team-a"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot verify privileged eligibility")
}

func TestWebhook_AllowsPrivilegedProfileInLabeledNamespace(t *testing.T) {
	v := validatorWithNamespaces(t, namespaceWithLabels("team-a", map[string]string{
		gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed,
	}))
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedProfile("team-a"))
	require.NoError(t, err, "privileged is eligible once the platform applies the label")
}

// A non-privileged profile never consults the label — an unlabeled namespace
// must still admit baseline/restricted.
func TestWebhook_AllowsNonPrivilegedProfileWithoutLabel(t *testing.T) {
	v := validatorWithNamespaces(t, namespaceWithLabels("team-a", nil))
	for _, profile := range []string{"", "baseline", "restricted"} {
		_, err := v.ValidateCreate(context.Background(), agWithProfile(profile))
		require.NoErrorf(t, err, "profile %q must not require the privileged label", profile)
	}
}

// On update, raising to privileged in an unlabeled namespace is rejected even
// when the (separate) downgrade gate would otherwise be satisfied — the platform
// label is an independent, fail-closed requirement.
func TestWebhook_UpdateRejectsPrivilegedProfileInUnlabeledNamespace(t *testing.T) {
	v := validatorWithNamespaces(t, namespaceWithLabels("team-a", nil))
	newObj := agWithPrivilegedProfile("team-a")
	newObj.Annotations = map[string]string{gmcv1alpha1.AllowProfileDowngradeAnnotation: "true"}
	_, err := v.ValidateUpdate(context.Background(), agWithProfile("baseline"), newObj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), gmcv1alpha1.PrivilegedProfileLabel)
}

// On update, raising to privileged is admitted once the namespace is labelled
// (and the downgrade annotation is present, since anything→privileged is a
// downgrade in rank).
func TestWebhook_UpdateAllowsPrivilegedProfileInLabeledNamespace(t *testing.T) {
	v := validatorWithNamespaces(t, namespaceWithLabels("team-a", map[string]string{
		gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed,
	}))
	newObj := agWithPrivilegedProfile("team-a")
	newObj.Annotations = map[string]string{gmcv1alpha1.AllowProfileDowngradeAnnotation: "true"}
	_, err := v.ValidateUpdate(context.Background(), agWithProfile("baseline"), newObj)
	require.NoError(t, err)
}

// The rejection must leave a server-side audit line (logRejection), the same as
// the other admission denials.
func TestWebhook_PrivilegedEligibilityRejectionIsAudited(t *testing.T) {
	v := validatorWithNamespaces(t, namespaceWithLabels("team-a", nil))
	ctx, lines := ctxWithCapture()
	_, err := v.ValidateCreate(ctx, agWithPrivilegedProfile("team-a"))
	require.Error(t, err)
	joined := strings.Join(*lines, "\n")
	assert.Contains(t, joined, "admission denied")
	assert.Contains(t, joined, "team-a")
}

func TestWebhook_RejectsGitHubHostInNoProxyCIDRs(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	for _, entry := range []string{"github.com", ".github.com", "api.github.com", "githubusercontent.com", ".com"} {
		ag := newAG("team-a")
		ag.Spec.Proxy.NoProxyCIDRs = []string{entry}
		_, err := v.ValidateCreate(context.Background(), ag)
		require.Errorf(t, err, "entry %q should be rejected", entry)
		assert.Contains(t, err.Error(), "noProxyCIDRs[0]")
		assert.Contains(t, err.Error(), "around the per-tenant egress proxy")
	}
}

func TestWebhook_RejectsGitHubEnterpriseHostInNoProxyCIDRs(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.GitHubURL = "https://ghes.example.com/example-org"
	// An entry that NO_PROXY-matches the tenant's GHES host bypasses the proxy.
	ag.Spec.Proxy.NoProxyCIDRs = []string{"example.com"}
	_, err := v.ValidateCreate(context.Background(), ag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghes.example.com")
}

// noProxyCIDRs legitimately takes CIDRs, bare IPs, and non-GitHub domain
// suffixes (e.g. svc.cluster.local — the in-cluster pattern the project's own
// defaults and e2e rely on). None of these may be rejected.
func TestWebhook_AllowsNonGitHubNoProxyEntries(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.Proxy.NoProxyCIDRs = []string{
		"10.0.0.0/8", "203.0.113.5/32", "fd00::/8", // CIDRs
		"10.0.0.5",                       // bare IP
		"svc.cluster.local", "localhost", // cluster-internal domain suffixes
		"internal.example.com", // a non-GitHub internal domain
	}
	_, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
}

func TestWebhook_AllowsEmptyNoProxyCIDRs(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a") // NoProxyCIDRs nil
	_, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
}

func TestWebhook_UpdateRejectsGitHubHostInNoProxyCIDRs(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	updated := newAG("team-a")
	updated.Spec.Proxy.NoProxyCIDRs = []string{"10.0.0.0/8", "api.github.com"}
	_, err := v.ValidateUpdate(context.Background(), newAG("team-a"), updated)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "noProxyCIDRs[1]")
}

// agWithPriorityTier returns a tenant-namespace AG whose single RunnerGroup
// names the given PriorityClass in priorityTiers.
func agWithPriorityTier(priorityClassName string) *gmcv1alpha1.ActionsGateway {
	ag := newAG("team-a")
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{
		{
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:latest"}}},
			},
			PriorityTiers: []agcv1alpha1.PriorityTier{
				{PriorityClassName: priorityClassName, Threshold: 5},
			},
		},
	}
	return ag
}

func TestWebhook_RejectsDisallowedPriorityClass(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", []string{"runner-standard"})
	_, err := v.ValidateCreate(context.Background(), agWithPriorityTier("system-cluster-critical"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "system-cluster-critical", "error should name the disallowed class")
	assert.Contains(t, err.Error(), "runner-standard", "error should name the allowed set")
}

func TestWebhook_AllowsAllowlistedPriorityClass(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", []string{"runner-standard", "runner-opportunistic"})
	_, err := v.ValidateCreate(context.Background(), agWithPriorityTier("runner-opportunistic"))
	require.NoError(t, err)
}

func TestWebhook_EmptyAllowlistRejectsAnyPriorityClass(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateCreate(context.Background(), agWithPriorityTier("runner-standard"))
	require.Error(t, err, "an empty allowlist must reject every priorityTiers PriorityClass reference")
}

func TestWebhook_NoPriorityTiersIsAllowedWithEmptyAllowlist(t *testing.T) {
	// A gateway with RunnerGroups but no priorityTiers is unaffected by the
	// allowlist — the check only iterates priorityTiers entries.
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateCreate(context.Background(), agWithPrivilegedContainer(false))
	require.NoError(t, err)
}

func TestWebhook_UpdateRejectsDisallowedPriorityClass(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", []string{"runner-standard"})
	_, err := v.ValidateUpdate(context.Background(),
		agWithPriorityTier("runner-standard"), agWithPriorityTier("system-cluster-critical"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "system-cluster-critical")
}

// agWithProfile returns a tenant-namespace AG with the given securityProfile.
func agWithProfile(profile string) *gmcv1alpha1.ActionsGateway {
	ag := newAG("team-a")
	ag.Spec.SecurityProfile = profile
	return ag
}

func TestWebhook_UpdateAllowsProfileUpgrade(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	// baseline -> restricted is a hardening upgrade; always allowed.
	_, err := v.ValidateUpdate(context.Background(), agWithProfile("baseline"), agWithProfile("restricted"))
	require.NoError(t, err)
}

func TestWebhook_UpdateAllowsSameProfile(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateUpdate(context.Background(), agWithProfile("restricted"), agWithProfile("restricted"))
	require.NoError(t, err)
}

func TestWebhook_UpdateRejectsProfileDowngrade(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	// restricted -> baseline relaxes isolation; rejected without the opt-in annotation.
	_, err := v.ValidateUpdate(context.Background(), agWithProfile("restricted"), agWithProfile("baseline"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "downgrade")
	assert.Contains(t, err.Error(), gmcv1alpha1.AllowProfileDowngradeAnnotation)
}

// TestWebhook_UpdateRejectsDowngradeToPrivileged covers baseline -> privileged,
// which is a downgrade because privileged is the *least* restrictive profile.
func TestWebhook_UpdateRejectsDowngradeToPrivileged(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateUpdate(context.Background(), agWithProfile("baseline"), agWithProfile("privileged"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "downgrade")
}

func TestWebhook_UpdateAllowsProfileDowngradeWithAnnotation(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	newObj := agWithProfile("baseline")
	newObj.Annotations = map[string]string{gmcv1alpha1.AllowProfileDowngradeAnnotation: "true"}
	_, err := v.ValidateUpdate(context.Background(), agWithProfile("restricted"), newObj)
	require.NoError(t, err, "an explicit allow-downgrade annotation must permit the downgrade")
}

// TestWebhook_UpdateRejectsDowngradeWithWrongAnnotationValue ensures only the
// literal "true" opts in — a present-but-falsey value must not relax isolation.
func TestWebhook_UpdateRejectsDowngradeWithWrongAnnotationValue(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	newObj := agWithProfile("baseline")
	newObj.Annotations = map[string]string{gmcv1alpha1.AllowProfileDowngradeAnnotation: "yes"}
	_, err := v.ValidateUpdate(context.Background(), agWithProfile("restricted"), newObj)
	require.Error(t, err)
}

// TestWebhook_UpdateTreatsEmptyProfileAsBaseline ensures a manifest that drops
// securityProfile (so it re-defaults to baseline) is treated as a downgrade
// from restricted, not a no-op.
func TestWebhook_UpdateTreatsEmptyProfileAsBaseline(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateUpdate(context.Background(), agWithProfile("restricted"), agWithProfile(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "downgrade")
}

func TestWebhook_DeleteNoOp(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	_, err := v.ValidateDelete(context.Background(), newAG("team-a"))
	require.NoError(t, err)
}

func TestWebhook_WarnsMissingCPURequest(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.Proxy.Resources.Requests = corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("64Mi"),
	}
	warnings, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "cpu")
}

func TestWebhook_UpdateWarnsMissingCPURequest(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	updated := newAG("team-a")
	updated.Spec.Proxy.Resources.Requests = corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("64Mi"),
	}
	warnings, err := v.ValidateUpdate(context.Background(), newAG("team-a"), updated)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "cpu")
}

func TestWebhook_NoWarnWhenCPURequestPresent(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.Proxy.Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("50m"),
		corev1.ResourceMemory: resource.MustParse("64Mi"),
	}
	warnings, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestWebhook_NoWarnWhenResourcesUnset(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	warnings, err := v.ValidateCreate(context.Background(), newAG("team-a"))
	require.NoError(t, err)
	assert.Empty(t, warnings)
}
