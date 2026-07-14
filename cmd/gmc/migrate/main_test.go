package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// fakeClient builds a fake controller-runtime client preloaded with the given v1
// objects, with the v1+v2 scheme registered. The fake client does not run admission
// (CEL/webhooks) — that path is covered by the envtest integration suite; these unit
// tests exercise the CLI flow (read → fan-out → print/apply) and its coverage.
func fakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objs...).Build()
}

// failingReader fails the test if read from — proves the --assume-yes path never
// touches stdin.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("confirmApply read stdin under --assume-yes; it must not")
	return 0, nil
}

func v1Namespace(name string, labels, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations}}
}

func v1Gateway(name, ns, profile string) *gmcv1alpha1.ActionsGateway {
	min := int32(2)
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef:    gmcv1alpha1.SecretReference{Name: "github-app"},
			GitHubURL:       "https://github.com/example-org",
			SecurityProfile: profile,
			Proxy:           gmcv1alpha1.ProxyConfig{MinReplicas: &min},
		},
	}
}

func v1RunnerGroup(name, ns, image string, labels []string) *agcv1alpha1.RunnerGroup {
	return &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agcv1alpha1.RunnerGroupSpec{
			RunnerLabels: labels,
			PodTemplate:  corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: image}}}},
			WorkerImage:  "worker:" + image,
		},
	}
}

func TestParseOptions_RequiresTarget(t *testing.T) {
	var stderr bytes.Buffer
	_, err := parseOptions(nil, &stderr)
	require.Error(t, err)
	assert.Contains(t, stderr.String(), "Usage:")
}

func TestParseOptions_NamespaceAndApply(t *testing.T) {
	opts, err := parseOptions([]string{"--namespace", "team-a", "--apply"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "team-a", opts.namespace)
	assert.True(t, opts.apply)
	assert.Empty(t, opts.kubeContext)
	assert.False(t, opts.assumeYes)
}

// TestParseOptions_ContextAndAssumeYes covers the wrong-cluster guard flags: an
// explicit --context pins the target, and --assume-yes / -y arm the automation escape.
func TestParseOptions_ContextAndAssumeYes(t *testing.T) {
	opts, err := parseOptions([]string{"--namespace", "team-a", "--apply", "--context", "kind-gag", "--assume-yes"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "kind-gag", opts.kubeContext)
	assert.True(t, opts.assumeYes)

	opts, err = parseOptions([]string{"-n", "team-a", "-y"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.True(t, opts.assumeYes, "-y is shorthand for --assume-yes")
}

// TestParseOptions_AssumeYesFromEnv covers the ASSUME_YES=1 escape (parity with
// scripts/lib/common.sh) even when the flag is absent.
func TestParseOptions_AssumeYesFromEnv(t *testing.T) {
	t.Setenv("ASSUME_YES", "1")
	opts, err := parseOptions([]string{"--namespace", "team-a", "--apply"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.True(t, opts.assumeYes)

	t.Setenv("ASSUME_YES", "0")
	opts, err = parseOptions([]string{"--namespace", "team-a", "--apply"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.False(t, opts.assumeYes, "only ASSUME_YES=1 arms the escape")
}

// TestApplyScope renders the confirmation scope: a single namespace vs the loud
// cluster-wide banner for --all-namespaces.
func TestApplyScope(t *testing.T) {
	assert.Contains(t, applyScope(options{namespace: "team-a"}), `"team-a"`)
	assert.Contains(t, applyScope(options{allNamespaces: true}), "cluster-wide")
}

// TestConfirmApply_AssumeYes skips the prompt and never touches stdin.
func TestConfirmApply_AssumeYes(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmApply("kind-gag", "namespace \"team-a\"", true, failingReader{t}, &out)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Contains(t, out.String(), "skipping confirmation")
	// The target and scope are always echoed before any write, assume-yes or not.
	assert.Contains(t, out.String(), "kind-gag")
}

// TestConfirmApply_Prompt covers the interactive replies: only y/Y/yes proceeds;
// everything else (and EOF on a piped stdin) declines without writing.
func TestConfirmApply_Prompt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  bool
	}{
		{"yes-short", "y\n", true},
		{"yes-upper", "Y\n", true},
		{"yes-word", "yes\n", true},
		{"no", "n\n", false},
		{"empty", "\n", false},
		{"eof", "", false},
		{"garbage", "maybe\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			ok, err := confirmApply("prod-cluster", "ALL namespaces (cluster-wide)", false, strings.NewReader(tc.input), &out)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
			// The resolved target + scope are echoed so a wrong cluster is visible.
			assert.Contains(t, out.String(), "prod-cluster")
			assert.Contains(t, out.String(), "cluster-wide")
			if !tc.want {
				assert.Contains(t, out.String(), "Aborted")
			}
		})
	}
}

// TestMigrateAll_DryRun prints the v2 manifests for a tenant and applies nothing.
func TestMigrateAll_DryRun(t *testing.T) {
	c := fakeClient(
		v1Namespace("team-a", map[string]string{"actions-gateway.github.com/tenant": "true"}, nil),
		v1Gateway("team-a", "team-a", "restricted"),
		v1RunnerGroup("team-a-linux", "team-a", "img:1", []string{"linux"}),
		v1RunnerGroup("team-a-linux2", "team-a", "img:1", []string{"linux2"}), // shared template
		v1RunnerGroup("team-a-gpu", "team-a", "img:gpu", []string{"gpu"}),
	)
	var stdout, stderr bytes.Buffer
	require.NoError(t, migrateAll(context.Background(), c, options{namespace: "team-a"}, &stdout, &stderr))

	out := stdout.String()
	assert.Contains(t, out, "kind: ActionsGateway")
	assert.Contains(t, out, "kind: EgressProxy")
	assert.Contains(t, out, "kind: RunnerSet")
	assert.Contains(t, out, "security-profile=restricted")

	// The dry-run next-step hint points the operator at --apply, on stderr so the
	// stdout manifest stays pipeable.
	assert.Contains(t, stderr.String(), "Dry-run complete")
	assert.NotContains(t, out, "Dry-run complete", "the hint must not contaminate the manifest payload")

	// Dry-run must not create anything in the cluster.
	var v2gw v2alpha1.ActionsGateway
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "team-a"}, &v2gw)
	assert.True(t, apierrors.IsNotFound(err), "dry-run must not create v2 objects")
}

// TestMigrateAll_Apply creates the v2 object set and patches the namespace.
func TestMigrateAll_Apply(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(
		v1Namespace("team-a", map[string]string{"actions-gateway.github.com/tenant": "true"}, nil),
		v1Gateway("team-a", "team-a", "restricted"),
		v1RunnerGroup("team-a-linux", "team-a", "img:1", []string{"linux"}),
		v1RunnerGroup("team-a-gpu", "team-a", "img:gpu", []string{"gpu"}),
	)
	var stdout, stderr bytes.Buffer
	require.NoError(t, migrateAll(ctx, c, options{namespace: "team-a", apply: true}, &stdout, &stderr))

	// The apply next-step hint points the operator at validate-then-decommission.
	assert.Contains(t, stderr.String(), "Migration applied")

	// v2 objects created.
	var v2gw v2alpha1.ActionsGateway
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "team-a"}, &v2gw))
	require.NotNil(t, v2gw.Spec.DefaultProxyRef)
	assert.Equal(t, "team-a-egress", v2gw.Spec.DefaultProxyRef.Name)

	var proxy v2alpha1.EgressProxy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "team-a-egress"}, &proxy))

	var sets v2alpha1.RunnerSetList
	require.NoError(t, c.List(ctx, &sets, client.InNamespace("team-a")))
	assert.Len(t, sets.Items, 2)

	// Namespace patched additively: v2 marker + relocated profile added; v1 marker kept.
	var ns corev1.Namespace
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "team-a"}, &ns))
	assert.Equal(t, "managed", ns.Labels[v2alpha1.TenantNamespaceMarkerLabel])
	assert.Equal(t, "restricted", ns.Labels[v2alpha1.SecurityProfileLabel])
	assert.Equal(t, "true", ns.Labels["actions-gateway.github.com/tenant"], "v1 marker kept for coexistence")
}

// TestMigrateAll_ApplyIdempotent re-runs apply and tolerates already-existing objects.
func TestMigrateAll_ApplyIdempotent(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(
		v1Namespace("team-a", nil, nil),
		v1Gateway("team-a", "team-a", ""),
		v1RunnerGroup("team-a-linux", "team-a", "img:1", []string{"linux"}),
	)
	opts := options{namespace: "team-a", apply: true}
	require.NoError(t, migrateAll(ctx, c, opts, &bytes.Buffer{}, &bytes.Buffer{}))
	var stderr bytes.Buffer
	require.NoError(t, migrateAll(ctx, c, opts, &bytes.Buffer{}, &stderr), "a second apply is idempotent")
	assert.Contains(t, stderr.String(), "exists, skipped")
}

// TestMigrateAll_AllNamespaces discovers every namespace holding a v1 gateway.
func TestMigrateAll_AllNamespaces(t *testing.T) {
	c := fakeClient(
		v1Namespace("a", nil, nil), v1Gateway("a", "a", ""),
		v1RunnerGroup("a-x", "a", "i", []string{"x"}),
		v1Namespace("b", nil, nil), v1Gateway("b", "b", ""),
		v1RunnerGroup("b-y", "b", "j", []string{"y"}),
	)
	var stdout bytes.Buffer
	require.NoError(t, migrateAll(context.Background(), c, options{allNamespaces: true}, &stdout, &bytes.Buffer{}))
	out := stdout.String()
	assert.Equal(t, 2, strings.Count(out, "kind: ActionsGateway"))
}

// TestMigrateAll_NoGateway skips a namespace with no v1 gateway.
func TestMigrateAll_NoGateway(t *testing.T) {
	c := fakeClient(v1Namespace("empty", nil, nil))
	var stderr bytes.Buffer
	require.NoError(t, migrateAll(context.Background(), c, options{namespace: "empty"}, &bytes.Buffer{}, &stderr))
	assert.Contains(t, stderr.String(), "no v1 ActionsGateway")
	// Nothing migrated ⇒ no next-step hint.
	assert.NotContains(t, stderr.String(), "Dry-run complete")
}

// TestMigrateAll_OutputDir writes a per-namespace manifest file.
func TestMigrateAll_OutputDir(t *testing.T) {
	c := fakeClient(
		v1Namespace("team-a", nil, nil), v1Gateway("team-a", "team-a", ""),
		v1RunnerGroup("team-a-linux", "team-a", "img:1", []string{"linux"}),
	)
	dir := t.TempDir()
	var stdout bytes.Buffer
	require.NoError(t, migrateAll(context.Background(), c, options{namespace: "team-a", outputDir: dir}, &stdout, &bytes.Buffer{}))
	assert.Contains(t, stdout.String(), "wrote ")
}

// TestGroupsForGateway covers the multi-gateway assignment branch (owner label →
// owning gateway; unowned → lexically-first gateway).
func TestGroupsForGateway(t *testing.T) {
	gwA := v1Gateway("a", "ns", "")
	gwB := v1Gateway("b", "ns", "")
	all := &gmcv1alpha1.ActionsGatewayList{Items: []gmcv1alpha1.ActionsGateway{*gwA, *gwB}}

	owned := *v1RunnerGroup("owned-by-b", "ns", "i", []string{"x"})
	owned.Labels = map[string]string{"actions-gateway/owner-name": "b"}
	unowned := *v1RunnerGroup("unowned", "ns", "j", []string{"y"})
	groups := []agcv1alpha1.RunnerGroup{owned, unowned}

	// b owns "owned-by-b"; a (lexically first) gets the unowned group.
	bGroups := groupsForGateway(gwB, all, groups)
	require.Len(t, bGroups, 1)
	assert.Equal(t, "owned-by-b", bGroups[0].Name)

	aGroups := groupsForGateway(gwA, all, groups)
	require.Len(t, aGroups, 1)
	assert.Equal(t, "unowned", aGroups[0].Name)
}
