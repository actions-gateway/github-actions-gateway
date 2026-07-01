package migrate

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// updateGolden regenerates the golden manifest fixtures instead of asserting
// against them. Run `go test ./internal/migrate -run Golden -update` after an
// intentional output change, then review the diff.
var updateGolden = flag.Bool("update", false, "update golden manifest fixtures")

// podTemplate builds a minimal but non-trivial PodTemplateSpec for tests. label
// distinguishes two otherwise-identical templates when needed.
func podTemplate(containerImage string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app": "runner"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "runner", Image: containerImage},
			},
		},
	}
}

func newRunnerGroup(name, ns, image, workerImage string, labels []string) agcv1alpha1.RunnerGroup {
	return agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agcv1alpha1.RunnerGroupSpec{
			RunnerLabels: labels,
			PodTemplate:  podTemplate(image),
			WorkerImage:  workerImage,
		},
	}
}

func newGateway(name, ns string) *gmcv1alpha1.ActionsGateway {
	min := int32(2)
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "github-app"},
			GitHubURL:    "https://github.com/" + name,
			Proxy:        gmcv1alpha1.ProxyConfig{MinReplicas: &min},
			LogLevel:     "info",
		},
	}
}

// TestFanOut_RepresentativeTenant exercises the headline scenario: a tenant with
// multiple groups, a shared template and a distinct template, a proxy, and a
// non-default securityProfile (the acceptance scenario from the task).
func TestFanOut_RepresentativeTenant(t *testing.T) {
	gw := newGateway("team-a", "team-a")
	gw.Spec.SecurityProfile = "restricted"

	in := Input{
		Namespace:       "team-a",
		NamespaceLabels: map[string]string{legacyTenantMarkerLabel: legacyTenantMarkerValue},
		Gateway:         gw,
		RunnerGroups: []agcv1alpha1.RunnerGroup{
			newRunnerGroup("team-a-linux", "team-a", "img:1", "worker:1", []string{"linux"}),
			// Identical (podTemplate, workerImage) to team-a-linux → must reuse.
			newRunnerGroup("team-a-linux2", "team-a", "img:1", "worker:1", []string{"linux2"}),
			// Distinct podTemplate → distinct template.
			newRunnerGroup("team-a-gpu", "team-a", "img:gpu", "worker:gpu", []string{"gpu"}),
		},
	}

	res, err := FanOut(in)
	require.NoError(t, err)

	// One gateway, one proxy.
	require.NotNil(t, res.Gateway)
	require.NotNil(t, res.Proxy)
	assert.Equal(t, "team-a", res.Gateway.Name)
	assert.Equal(t, "team-a-egress", res.Proxy.Name)

	// No silent direct egress: defaultProxyRef wired to the emitted proxy.
	require.NotNil(t, res.Gateway.Spec.DefaultProxyRef)
	assert.Equal(t, "team-a-egress", res.Gateway.Spec.DefaultProxyRef.Name)

	// securityProfile relocated to the v2 namespace label.
	require.NotNil(t, res.NamespacePatch)
	assert.Equal(t, "restricted", res.NamespacePatch.Labels[v2alpha1.SecurityProfileLabel])
	// Q147 tenant marker aligned (additively).
	assert.Equal(t, v2alpha1.TenantNamespaceMarkerValue, res.NamespacePatch.Labels[v2alpha1.TenantNamespaceMarkerLabel])

	// Reuse: 3 groups, 2 distinct templates, 3 sets.
	assert.Len(t, res.Templates, 2, "two distinct templates after reuse collapse")
	assert.Len(t, res.Sets, 3, "one RunnerSet per group")

	// Every set references the gateway and an existing template, and leaves proxyRef
	// unset (inherits defaultProxyRef → proxied).
	tmplNames := map[string]bool{}
	for _, tm := range res.Templates {
		tmplNames[tm.Name] = true
	}
	sharedTemplate := ""
	for _, s := range res.Sets {
		assert.Equal(t, "team-a", s.Spec.GatewayRef.Name)
		assert.True(t, tmplNames[s.Spec.TemplateRef.Name], "templateRef %q resolves to an emitted template", s.Spec.TemplateRef.Name)
		assert.Nil(t, s.Spec.ProxyRef, "proxyRef unset so the set inherits defaultProxyRef")
		if s.Name == "team-a-linux" {
			sharedTemplate = s.Spec.TemplateRef.Name
		}
	}
	// The two identical-template sets share one template name.
	for _, s := range res.Sets {
		if s.Name == "team-a-linux2" {
			assert.Equal(t, sharedTemplate, s.Spec.TemplateRef.Name, "identical templates collapse to one")
		}
		if s.Name == "team-a-gpu" {
			assert.NotEqual(t, sharedTemplate, s.Spec.TemplateRef.Name, "distinct template stays separate")
		}
	}
}

// TestFanOut_ReuseCollapse proves K identical templates collapse to ONE
// RunnerTemplate (§H.17 invariant 2 — the object-size justification).
func TestFanOut_ReuseCollapse(t *testing.T) {
	gw := newGateway("t", "t")
	var groups []agcv1alpha1.RunnerGroup
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		groups = append(groups, newRunnerGroup("t-"+name, "t", "same:img", "same:worker", []string{name}))
	}
	res, err := FanOut(Input{Namespace: "t", Gateway: gw, RunnerGroups: groups})
	require.NoError(t, err)
	assert.Len(t, res.Templates, 1, "five identical templates collapse to one")
	assert.Len(t, res.Sets, 5)
	for _, s := range res.Sets {
		assert.Equal(t, res.Templates[0].Name, s.Spec.TemplateRef.Name)
	}
}

// TestFanOut_WorkerImageDistinguishes proves workerImage participates in template
// equality: same podTemplate + different workerImage ⇒ two templates, so a group's
// runner image is never silently changed by a collapse.
func TestFanOut_WorkerImageDistinguishes(t *testing.T) {
	gw := newGateway("t", "t")
	groups := []agcv1alpha1.RunnerGroup{
		newRunnerGroup("t-a", "t", "img:same", "worker:1", []string{"a"}),
		newRunnerGroup("t-b", "t", "img:same", "worker:2", []string{"b"}),
	}
	res, err := FanOut(Input{Namespace: "t", Gateway: gw, RunnerGroups: groups})
	require.NoError(t, err)
	assert.Len(t, res.Templates, 2, "different workerImage ⇒ distinct templates")
}

// TestFanOut_MaxListenersPinned proves the v1 concurrency ceiling is preserved:
// an unset v1 maxListeners pins to 1 (not v2's default of 10), a set value copies.
func TestFanOut_MaxListenersPinned(t *testing.T) {
	gw := newGateway("t", "t")
	g1 := newRunnerGroup("t-unset", "t", "i", "w", []string{"a"}) // MaxListeners unset (0)
	g2 := newRunnerGroup("t-set", "t", "i2", "w2", []string{"b"})
	g2.Spec.MaxListeners = 5
	res, err := FanOut(Input{Namespace: "t", Gateway: gw, RunnerGroups: []agcv1alpha1.RunnerGroup{g1, g2}})
	require.NoError(t, err)
	byName := map[string]*v2alpha1.RunnerSet{}
	for _, s := range res.Sets {
		byName[s.Name] = s
	}
	assert.Equal(t, int32(1), byName["t-unset"].Spec.MaxListeners, "unset v1 maxListeners preserved as 1, not raised to v2's 10")
	assert.Equal(t, int32(5), byName["t-set"].Spec.MaxListeners)
}

// TestFanOut_SecurityProfileRelocation covers baseline/restricted/privileged and
// the privileged-grant carry-forward + warning.
func TestFanOut_SecurityProfileRelocation(t *testing.T) {
	t.Run("baseline default set explicitly", func(t *testing.T) {
		gw := newGateway("t", "t") // securityProfile unset → baseline
		res, err := FanOut(Input{Namespace: "t", Gateway: gw})
		require.NoError(t, err)
		assert.Equal(t, "baseline", res.NamespacePatch.Labels[v2alpha1.SecurityProfileLabel])
	})

	t.Run("privileged with grant carries the eligibility label", func(t *testing.T) {
		gw := newGateway("t", "t")
		gw.Spec.SecurityProfile = "privileged"
		res, err := FanOut(Input{
			Namespace:       "t",
			NamespaceLabels: map[string]string{gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed},
			Gateway:         gw,
		})
		require.NoError(t, err)
		assert.Equal(t, "privileged", res.NamespacePatch.Labels[v2alpha1.SecurityProfileLabel])
		assert.Equal(t, v2alpha1.PrivilegedProfileAllowed, res.NamespacePatch.Labels[v2alpha1.PrivilegedProfileLabel])
		assert.Empty(t, res.Warnings)
	})

	t.Run("privileged without grant warns and never invents the grant", func(t *testing.T) {
		gw := newGateway("t", "t")
		gw.Spec.SecurityProfile = "privileged"
		res, err := FanOut(Input{Namespace: "t", Gateway: gw})
		require.NoError(t, err)
		assert.Equal(t, "privileged", res.NamespacePatch.Labels[v2alpha1.SecurityProfileLabel])
		_, granted := res.NamespacePatch.Labels[v2alpha1.PrivilegedProfileLabel]
		assert.False(t, granted, "the tool never invents an eligibility grant")
		require.NotEmpty(t, res.Warnings)
	})
}

// TestFanOut_DowngradeAnnotationAlignment proves the Q147 downgrade opt-in is
// aligned additively when present, and absent when the v1 annotation is absent.
func TestFanOut_DowngradeAnnotationAlignment(t *testing.T) {
	gw := newGateway("t", "t")
	res, err := FanOut(Input{
		Namespace:            "t",
		NamespaceAnnotations: map[string]string{gmcv1alpha1.AllowProfileDowngradeAnnotation: "true"},
		Gateway:              gw,
	})
	require.NoError(t, err)
	assert.Equal(t, v2alpha1.AllowProfileDowngradeAllowed,
		res.NamespacePatch.Annotations[v2alpha1.AllowProfileDowngradeAnnotation])

	res2, err := FanOut(Input{Namespace: "t", Gateway: gw})
	require.NoError(t, err)
	assert.Nil(t, res2.NamespacePatch.Annotations)
}

// TestFanOut_StandaloneVsInline proves the precedence decision: an inline bootstrap
// entry already materialized as a standalone CR is migrated once (standalone wins),
// and an inline entry with no standalone CR is synthesized.
func TestFanOut_StandaloneVsInline(t *testing.T) {
	gw := newGateway("gw", "ns")
	// Inline entry whose derived name collides with the standalone CR below.
	inlineSpec := agcv1alpha1.RunnerGroupSpec{
		RunnerLabels: []string{"linux"},
		PodTemplate:  podTemplate("inline-image"),
		WorkerImage:  "inline-worker",
	}
	gw.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{
		inlineSpec,
		// A second inline entry with no standalone CR — must be synthesized.
		{RunnerLabels: []string{"win"}, PodTemplate: podTemplate("win-image"), WorkerImage: "win-worker"},
	}
	standaloneName := runnerGroupName("gw", inlineSpec, 0)
	standalone := newRunnerGroup(standaloneName, "ns", "standalone-image", "standalone-worker", []string{"linux"})

	res, err := FanOut(Input{
		Namespace:    "ns",
		Gateway:      gw,
		RunnerGroups: []agcv1alpha1.RunnerGroup{standalone},
	})
	require.NoError(t, err)
	// Two logical groups: the collided one (standalone wins) + the synthesized "win".
	assert.Len(t, res.Sets, 2)

	bySet := map[string]*v2alpha1.RunnerSet{}
	for _, s := range res.Sets {
		bySet[s.Name] = s
	}
	require.Contains(t, bySet, standaloneName)
	// Standalone won: its template is the standalone image, not the inline image.
	standaloneTmplName := bySet[standaloneName].Spec.TemplateRef.Name
	var standaloneTmpl *v2alpha1.RunnerTemplate
	for _, tm := range res.Templates {
		if tm.Name == standaloneTmplName {
			standaloneTmpl = tm
		}
	}
	require.NotNil(t, standaloneTmpl)
	assert.Equal(t, "standalone-image", standaloneTmpl.Spec.PodTemplate.Spec.Containers[0].Image)
	assert.Equal(t, "standalone-worker", standaloneTmpl.Spec.WorkerImage)
}

// TestFanOut_NameCap proves an over-long v1 name is truncated under the 52-char cap
// with a warning, so the emitted manifest is admissible under the v2 CRD CEL.
func TestFanOut_NameCap(t *testing.T) {
	longName := "this-is-a-very-long-gateway-name-that-exceeds-the-fifty-two-character-cap"
	gw := newGateway(longName, "ns")
	res, err := FanOut(Input{Namespace: "ns", Gateway: gw})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(res.Gateway.Name), maxNameLen)
	assert.LessOrEqual(t, len(res.Proxy.Name), maxNameLen)
	require.NotEmpty(t, res.Warnings)
}

// TestFanOut_NoGateway rejects a structurally unmigratable input.
func TestFanOut_NoGateway(t *testing.T) {
	_, err := FanOut(Input{Namespace: "ns"})
	require.Error(t, err)
}

// TestFanOut_Deterministic proves the same input yields byte-identical rendered
// output across runs (the golden-test contract).
func TestFanOut_Deterministic(t *testing.T) {
	build := func() string {
		gw := newGateway("team-a", "team-a")
		gw.Spec.SecurityProfile = "restricted"
		res, err := FanOut(Input{
			Namespace:       "team-a",
			NamespaceLabels: map[string]string{legacyTenantMarkerLabel: legacyTenantMarkerValue},
			Gateway:         gw,
			RunnerGroups: []agcv1alpha1.RunnerGroup{
				newRunnerGroup("team-a-linux", "team-a", "img:1", "worker:1", []string{"linux"}),
				newRunnerGroup("team-a-gpu", "team-a", "img:gpu", "worker:gpu", []string{"gpu"}),
			},
		})
		require.NoError(t, err)
		out, err := RenderManifests(res)
		require.NoError(t, err)
		return out
	}
	assert.Equal(t, build(), build())
}

// TestMostRestrictiveProfile covers the defensive multi-gateway merge: the
// strictest posture wins and an empty input is the baseline default.
func TestMostRestrictiveProfile(t *testing.T) {
	assert.Equal(t, "baseline", MostRestrictiveProfile())
	assert.Equal(t, "baseline", MostRestrictiveProfile("", "baseline"))
	assert.Equal(t, "restricted", MostRestrictiveProfile("baseline", "restricted"))
	assert.Equal(t, "restricted", MostRestrictiveProfile("restricted", "privileged"), "restricted outranks privileged")
	// privileged is the LEAST restrictive level (rank 0), so baseline wins over it.
	assert.Equal(t, "baseline", MostRestrictiveProfile("privileged", "baseline"), "baseline outranks privileged")
}

// TestGoldenRepresentativeTenant snapshots the full rendered manifest for the
// representative multi-group tenant. Regenerate with -update after an intentional
// change and review the diff.
func TestGoldenRepresentativeTenant(t *testing.T) {
	gw := newGateway("team-a", "team-a")
	gw.Spec.SecurityProfile = "restricted"
	min := int32(2)
	max := int32(8)
	cpu := int32(70)
	gw.Spec.Proxy = gmcv1alpha1.ProxyConfig{
		MinReplicas:                    &min,
		MaxReplicas:                    &max,
		TargetCPUUtilizationPercentage: &cpu,
		NoProxyCIDRs:                   []string{"10.0.0.0/8"},
	}

	maxWorkers := int32(20)
	gpu := newRunnerGroup("team-a-gpu", "team-a", "img:gpu", "worker:gpu", []string{"gpu", "large"})
	gpu.Spec.MaxWorkers = &maxWorkers
	gpu.Spec.PriorityTiers = []agcv1alpha1.PriorityTier{{PriorityClassName: "high", Threshold: 20}}

	in := Input{
		Namespace: "team-a",
		NamespaceLabels: map[string]string{
			legacyTenantMarkerLabel: legacyTenantMarkerValue,
		},
		NamespaceAnnotations: map[string]string{
			gmcv1alpha1.AllowProfileDowngradeAnnotation: "true",
		},
		Gateway: gw,
		RunnerGroups: []agcv1alpha1.RunnerGroup{
			newRunnerGroup("team-a-linux", "team-a", "img:1", "worker:1", []string{"linux"}),
			newRunnerGroup("team-a-linux2", "team-a", "img:1", "worker:1", []string{"linux2"}),
			gpu,
		},
	}

	res, err := FanOut(in)
	require.NoError(t, err)
	got, err := RenderManifests(res)
	require.NoError(t, err)

	goldenPath := filepath.Join("testdata", "representative-tenant.golden.yaml")
	if *updateGolden {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(goldenPath, []byte(got), 0o600))
		return
	}
	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file missing; run with -update")
	assert.Equal(t, string(want), got)
}

// TestLabelSafe exercises every sanitization branch: it must lowercase, replace
// any non-[a-z0-9-] byte with '-', trim leading/trailing '-', cap the segment at
// 40 bytes, fall back to "label" when nothing survives, and always append a
// 7-hex hash so distinct inputs stay distinct.
func TestLabelSafe(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantSeg string // segment before the "-<hash>" suffix
	}{
		{"already safe", "linux", "linux"},
		{"uppercased", "Linux", "linux"},
		{"non-alnum to dash", "a/b c", "a-b-c"},
		{"trims edge dashes", "-abc-", "abc"},
		{"caps at 40 bytes", strings.Repeat("a", 50), strings.Repeat("a", 40)},
		{"empty after sanitize falls back", "///", "label"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := labelSafe(tt.in)
			assert.Regexp(t, "^"+tt.wantSeg+"-[0-9a-f]{7}$", got)
		})
	}

	// The hash suffix keeps otherwise-colliding sanitized segments distinct.
	assert.NotEqual(t, labelSafe("a/b"), labelSafe("a-b"),
		"different raw labels that sanitize identically must differ by hash")
}

// TestRunnerGroupName covers both derivation paths: a content-derived name from
// the first runner label (via labelSafe) and the index-based fallback when no
// labels are set.
func TestRunnerGroupName(t *testing.T) {
	withLabel := runnerGroupName("gw", agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{"Linux"}}, 0)
	assert.Regexp(t, "^gw-linux-[0-9a-f]{7}$", withLabel)

	noLabel := runnerGroupName("gw", agcv1alpha1.RunnerGroupSpec{}, 3)
	assert.Equal(t, "gw-3", noLabel)
}
