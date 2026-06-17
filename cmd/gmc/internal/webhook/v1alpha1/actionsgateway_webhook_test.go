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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

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

func TestWebhook_RejectsHostnameInNoProxyCIDRs(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.Proxy.NoProxyCIDRs = []string{"github.com"}
	_, err := v.ValidateCreate(context.Background(), ag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "noProxyCIDRs")
	assert.Contains(t, err.Error(), "not a valid CIDR")
}

func TestWebhook_RejectsBareIPInNoProxyCIDRs(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	// A bare IP (no prefix length) is not a CIDR; require explicit /32.
	ag.Spec.Proxy.NoProxyCIDRs = []string{"10.0.0.5"}
	_, err := v.ValidateCreate(context.Background(), ag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "noProxyCIDRs[0]")
}

func TestWebhook_AllowsValidNoProxyCIDRs(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a")
	ag.Spec.Proxy.NoProxyCIDRs = []string{"10.0.0.0/8", "203.0.113.5/32", "fd00::/8"}
	_, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
}

func TestWebhook_AllowsEmptyNoProxyCIDRs(t *testing.T) {
	v := NewActionsGatewayCustomValidator("", nil)
	ag := newAG("team-a") // NoProxyCIDRs nil
	_, err := v.ValidateCreate(context.Background(), ag)
	require.NoError(t, err)
}

func TestWebhook_UpdateRejectsHostnameInNoProxyCIDRs(t *testing.T) {
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
