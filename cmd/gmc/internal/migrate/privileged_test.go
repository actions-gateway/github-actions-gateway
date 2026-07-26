package migrate

import (
	"context"
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
	webhookv2alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v2alpha1"
)

// dindPodTemplate builds the representative Docker-in-Docker worker pod shape: a
// runner container pointed at a `docker:dind` NATIVE sidecar (a restartPolicy: Always
// init container), which is where the privileged flag sits. It mirrors the shape the
// dogfood e2e tenant actually runs (deploy/dogfood-e2e/overlays/dind/resources.yaml)
// and the one cmd/gmc/test/utils' DinD fixture applies, so the unit and e2e tiers
// exercise the same migration input.
func dindPodTemplate(runnerImage string) corev1.PodTemplateSpec {
	privileged := true
	always := corev1.ContainerRestartPolicyAlways
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name:            "dind",
				Image:           "docker:27-dind",
				RestartPolicy:   &always,
				Args:            []string{"--host=tcp://0.0.0.0:2375", "--tls=false"},
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
			}},
			Containers: []corev1.Container{{
				Name:  "runner",
				Image: runnerImage,
				Env:   []corev1.EnvVar{{Name: "DOCKER_HOST", Value: "tcp://localhost:2375"}},
			}},
		},
	}
}

// newDinDRunnerGroup builds a v1 RunnerGroup carrying the DinD worker shape.
func newDinDRunnerGroup(name, ns, runnerImage string, labels []string) agcv1alpha1.RunnerGroup {
	return agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agcv1alpha1.RunnerGroupSpec{
			RunnerLabels: labels,
			PodTemplate:  dindPodTemplate(runnerImage),
			WorkerImage:  runnerImage,
		},
	}
}

// newPrivilegedTenant assembles the v1 input for a privileged DinD tenant: the
// gateway opts into securityProfile: privileged and the namespace already holds the
// platform eligibility grant, which is the only configuration v1's own webhook admits
// a privileged worker container under.
func newPrivilegedTenant(ns string, groups ...agcv1alpha1.RunnerGroup) Input {
	gw := newGateway(ns, ns)
	gw.Spec.SecurityProfile = "privileged"
	return Input{
		Namespace: ns,
		NamespaceLabels: map[string]string{
			legacyTenantMarkerLabel:            legacyTenantMarkerValue,
			gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed,
		},
		Gateway:      gw,
		RunnerGroups: groups,
	}
}

// TestFanOut_PrivilegedShapeBecomesClusterTemplate is the Q414 headline: a v1 DinD
// tenant fans out to a CLUSTER-SCOPED ClusterRunnerTemplate, not a namespaced
// RunnerTemplate, and its RunnerSet's templateRef names that kind explicitly.
//
// Before this, FanOut emitted a namespaced RunnerTemplate carrying the privileged
// dind container — an object the v2 admission webhook refuses — so `--apply` failed
// after the EgressProxy was already created and left the namespace half-migrated.
func TestFanOut_PrivilegedShapeBecomesClusterTemplate(t *testing.T) {
	res, err := FanOut(newPrivilegedTenant("gag-dogfood-e2e",
		newDinDRunnerGroup("ci-e2e", "gag-dogfood-e2e", "runner:1", []string{"gag-ci-e2e"})))
	require.NoError(t, err)

	require.Len(t, res.ClusterTemplates, 1, "the privileged shape lands on the cluster-scoped kind")
	require.Empty(t, res.Templates, "and NOT on the namespaced kind, whose webhook would reject it")

	crt := res.ClusterTemplates[0]
	assert.Equal(t, "ClusterRunnerTemplate", crt.Kind)
	assert.Empty(t, crt.Namespace, "the cluster-scoped kind carries no namespace")
	assert.True(t, strings.HasPrefix(crt.Name, "crt-gag-dogfood-e2e-"),
		"cluster template names are namespace-qualified, got %q", crt.Name)
	assert.LessOrEqual(t, len(crt.Name), maxNameLen)
	assert.Equal(t, "gag-dogfood-e2e", crt.Labels[v2alpha1.MigratedFromNamespaceLabel],
		"provenance label records the namespace, since namespace deletion will not reclaim this object")

	require.Len(t, res.Sets, 1)
	ref := res.Sets[0].Spec.TemplateRef
	require.NotNil(t, ref)
	assert.Equal(t, crt.Name, ref.Name)
	assert.Equal(t, "ClusterRunnerTemplate", ref.Kind,
		"templateRef must name the kind explicitly; the default referent is the namespaced RunnerTemplate")

	// The operator must see the blast-radius change in the dry-run, before --apply.
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "CLUSTER-SCOPED")
	assert.Contains(t, res.Warnings[0], v2alpha1.MigratedFromNamespaceLabel)
}

// TestFanOut_EmittedTemplatesAreAdmissible is the decisive regression test: it runs
// the REAL v2 admission validators over everything FanOut emits, for both a
// privileged and an ordinary tenant. This is the invariant that actually broke —
// `gag-migrate` emitted an object the apiserver refuses — so the assertion is made
// against the admission rule itself rather than against a restatement of it.
func TestFanOut_EmittedTemplatesAreAdmissible(t *testing.T) {
	ctx := context.Background()
	// A nil PriorityClasses allowlist is the secure default: it forbids every NAMED
	// PriorityClass while permitting a pod that names none, which is what these
	// templates do.
	namespaced := &webhookv2alpha1.RunnerTemplateCustomValidator{}
	clusterScoped := &webhookv2alpha1.ClusterRunnerTemplateCustomValidator{}

	for _, tc := range []struct {
		name string
		in   Input
	}{
		{
			name: "privileged DinD tenant",
			in: newPrivilegedTenant("dind-tenant",
				newDinDRunnerGroup("dind-tenant-e2e", "dind-tenant", "runner:1", []string{"e2e"})),
		},
		{
			name: "ordinary tenant",
			in: Input{
				Namespace: "plain",
				Gateway:   newGateway("plain", "plain"),
				RunnerGroups: []agcv1alpha1.RunnerGroup{
					newRunnerGroup("plain-linux", "plain", "img:1", "worker:1", []string{"linux"}),
				},
			},
		},
		{
			name: "mixed tenant: one privileged group, one ordinary",
			in: newPrivilegedTenant("mixed",
				newDinDRunnerGroup("mixed-e2e", "mixed", "runner:1", []string{"e2e"}),
				newRunnerGroup("mixed-linux", "mixed", "img:1", "worker:1", []string{"linux"})),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := FanOut(tc.in)
			require.NoError(t, err)
			for _, tmpl := range res.Templates {
				_, err := namespaced.ValidateCreate(ctx, tmpl)
				assert.NoError(t, err, "emitted RunnerTemplate %q must be admissible", tmpl.Name)
			}
			for _, tmpl := range res.ClusterTemplates {
				_, err := clusterScoped.ValidateCreate(ctx, tmpl)
				assert.NoError(t, err, "emitted ClusterRunnerTemplate %q must be admissible", tmpl.Name)
			}
		})
	}
}

// TestFanOut_MixedTenantSplitsByPodShape proves the kind choice is per-group, not
// per-tenant: a tenant running both a privileged DinD group and an ordinary group
// gets one object of each kind, and each RunnerSet points at the right one.
func TestFanOut_MixedTenantSplitsByPodShape(t *testing.T) {
	res, err := FanOut(newPrivilegedTenant("mixed",
		newDinDRunnerGroup("mixed-e2e", "mixed", "runner:1", []string{"e2e"}),
		newRunnerGroup("mixed-linux", "mixed", "img:1", "worker:1", []string{"linux"})))
	require.NoError(t, err)

	require.Len(t, res.ClusterTemplates, 1)
	require.Len(t, res.Templates, 1)
	require.Len(t, res.Sets, 2)

	byName := map[string]*v2alpha1.RunnerSet{}
	for _, s := range res.Sets {
		byName[s.Name] = s
	}
	assert.Equal(t, "ClusterRunnerTemplate", byName["mixed-e2e"].Spec.TemplateRef.Kind)
	assert.Equal(t, res.ClusterTemplates[0].Name, byName["mixed-e2e"].Spec.TemplateRef.Name)
	assert.Empty(t, byName["mixed-linux"].Spec.TemplateRef.Kind,
		"an ordinary group keeps the default (namespaced) referent kind")
	assert.Equal(t, res.Templates[0].Name, byName["mixed-linux"].Spec.TemplateRef.Name)
}

// TestFanOut_PrivilegedReuseCollapsesWithinANamespace proves the reuse invariant
// (§H.17 #2) holds for the cluster-scoped kind too: K identical privileged groups in
// one namespace collapse to one object, and only one warning is raised.
func TestFanOut_PrivilegedReuseCollapsesWithinANamespace(t *testing.T) {
	res, err := FanOut(newPrivilegedTenant("t",
		newDinDRunnerGroup("t-a", "t", "runner:1", []string{"a"}),
		newDinDRunnerGroup("t-b", "t", "runner:1", []string{"b"}),
		newDinDRunnerGroup("t-c", "t", "runner:1", []string{"c"})))
	require.NoError(t, err)
	require.Len(t, res.ClusterTemplates, 1, "three identical privileged templates collapse to one")
	assert.Len(t, res.Sets, 3)
	assert.Len(t, res.Warnings, 1, "one warning per emitted cluster template, not one per group")
	for _, s := range res.Sets {
		assert.Equal(t, res.ClusterTemplates[0].Name, s.Spec.TemplateRef.Name)
	}
}

// TestFanOut_PrivilegedTemplatesAreNotSharedAcrossNamespaces is the reason cluster
// template names are namespace-qualified. Two tenants with a byte-identical
// privileged worker shape must get DISTINCT cluster-scoped objects: a bare content
// address would silently make the second tenant adopt the first's template, so an
// edit by either tenant's operator would change the other tenant's worker pods —
// a coupling v1's per-namespace inline podTemplate never had.
func TestFanOut_PrivilegedTemplatesAreNotSharedAcrossNamespaces(t *testing.T) {
	a, err := FanOut(newPrivilegedTenant("tenant-a",
		newDinDRunnerGroup("g", "tenant-a", "runner:1", []string{"e2e"})))
	require.NoError(t, err)
	b, err := FanOut(newPrivilegedTenant("tenant-b",
		newDinDRunnerGroup("g", "tenant-b", "runner:1", []string{"e2e"})))
	require.NoError(t, err)

	require.Len(t, a.ClusterTemplates, 1)
	require.Len(t, b.ClusterTemplates, 1)
	assert.NotEqual(t, a.ClusterTemplates[0].Name, b.ClusterTemplates[0].Name,
		"identical shapes in different namespaces must not share one cluster-scoped object")
	assert.Equal(t, a.ClusterTemplates[0].Spec, b.ClusterTemplates[0].Spec,
		"the shapes themselves are identical — only the names differ")
}

// TestClusterTemplateName_LongNamespaceStaysWithinTheCap proves a namespace long
// enough to push the joined name past the v2 52-char cap is truncated
// deterministically, reported as truncated so the operator is warned, and still
// distinct from another long namespace sharing the truncated prefix.
func TestClusterTemplateName_LongNamespaceStaysWithinTheCap(t *testing.T) {
	spec := v2alpha1.RunnerTemplateSpec{PodTemplate: dindPodTemplate("runner:1"), WorkerImage: "runner:1"}
	longA := strings.Repeat("a", 60) + "-one"
	longB := strings.Repeat("a", 60) + "-two"

	nameA, truncA, err := clusterTemplateName(longA, spec)
	require.NoError(t, err)
	nameB, truncB, err := clusterTemplateName(longB, spec)
	require.NoError(t, err)

	assert.True(t, truncA)
	assert.True(t, truncB)
	assert.LessOrEqual(t, len(nameA), maxNameLen)
	assert.LessOrEqual(t, len(nameB), maxNameLen)
	assert.NotEqual(t, nameA, nameB, "truncation must not collide two distinct namespaces")

	again, _, err := clusterTemplateName(longA, spec)
	require.NoError(t, err)
	assert.Equal(t, nameA, again, "deterministic across calls")

	// A truncated cluster-template name surfaces as an operator warning.
	res, err := FanOut(newPrivilegedTenant(longA, newDinDRunnerGroup("g", longA, "runner:1", []string{"e2e"})))
	require.NoError(t, err)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "exceeds the v2 52-char cap")
}

// TestGoldenDinDTenant snapshots the full rendered manifest for the representative
// DinD tenant — the migration output an operator reviews before --apply, including
// the cluster-scoped template and the warning block. Regenerate with -update after an
// intentional change and review the diff.
func TestGoldenDinDTenant(t *testing.T) {
	res, err := FanOut(newPrivilegedTenant("gag-dogfood-e2e",
		newDinDRunnerGroup("ci-e2e", "gag-dogfood-e2e", "runner:1", []string{"gag-ci-e2e"})))
	require.NoError(t, err)
	got, err := RenderManifests(res)
	require.NoError(t, err)

	goldenPath := filepath.Join("testdata", "dind-tenant.golden.yaml")
	if *updateGolden {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(goldenPath, []byte(got), 0o600))
		return
	}
	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file missing; run with -update")
	assert.Equal(t, string(want), got)
}
