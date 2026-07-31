package v2alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func specWith(containers, initContainers []corev1.Container) *agcv2alpha1.RunnerTemplateSpec {
	return &agcv2alpha1.RunnerTemplateSpec{
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: containers, InitContainers: initContainers},
		},
	}
}

// captureSink is a minimal logr.LogSink that records Info/Error lines emitted
// during a test, so logRejection's audit trail can be asserted without a live
// logging backend. Mirrors the v1alpha1 webhook test helper of the same name.
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

func TestValidateReservedPodFields(t *testing.T) {
	priv := true
	proxyEnv := corev1.Container{Name: "runner", Env: []corev1.EnvVar{{Name: "HTTP_PROXY", Value: "x"}}}
	lowerProxyEnv := corev1.Container{Name: "runner", Env: []corev1.EnvVar{{Name: "no_proxy", Value: "x"}}}
	caEnv := corev1.Container{Name: "runner", Env: []corev1.EnvVar{{Name: "PROXY_CA_CERT_PATH", Value: "x"}}}
	privContainer := corev1.Container{Name: "runner", SecurityContext: &corev1.SecurityContext{Privileged: &priv}}
	clean := corev1.Container{Name: "runner"}

	tests := []struct {
		name            string
		spec            *agcv2alpha1.RunnerTemplateSpec
		rejectPriv      bool
		wantErr         bool
		wantErrContains string
	}{
		{"clean admitted", specWith([]corev1.Container{clean}, nil), true, false, ""},
		{"proxy env rejected", specWith([]corev1.Container{proxyEnv}, nil), true, true, "is reserved"},
		{"proxy env case-insensitive", specWith([]corev1.Container{lowerProxyEnv}, nil), true, true, "is reserved"},
		{"proxy ca path rejected", specWith([]corev1.Container{caEnv}, nil), true, true, "is reserved"},
		{"proxy env in init container rejected", specWith([]corev1.Container{clean}, []corev1.Container{proxyEnv}), true, true, "initContainers"},
		{"privileged rejected when flagged", specWith([]corev1.Container{privContainer}, nil), true, true, "privileged containers are not permitted"},
		{"privileged allowed when not flagged", specWith([]corev1.Container{privContainer}, nil), false, false, ""},
		{"privileged init container rejected when flagged", specWith([]corev1.Container{clean}, []corev1.Container{privContainer}), true, true, "privileged containers are not permitted"},
		// proxy env is rejected even when privileged is allowed (cluster-scoped path).
		{"proxy env rejected even when privileged allowed", specWith([]corev1.Container{proxyEnv}, nil), false, true, "is reserved"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReservedPodFields(tc.spec, tc.rejectPriv)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrContains)
		})
	}
}

func newRunnerTemplate(namespace, name string, containers []corev1.Container) *agcv2alpha1.RunnerTemplate {
	return &agcv2alpha1.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       *specWith(containers, nil),
	}
}

func newClusterRunnerTemplate(name string, containers []corev1.Container) *agcv2alpha1.ClusterRunnerTemplate {
	return &agcv2alpha1.ClusterRunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       *specWith(containers, nil),
	}
}

// rtWithPriorityClass returns a namespaced RunnerTemplate whose podTemplate names the
// given PriorityClass. An empty name leaves the field unset.
func rtWithPriorityClass(priorityClassName string) *agcv2alpha1.RunnerTemplate {
	rt := newRunnerTemplate("team-a", "rt", []corev1.Container{{Name: "runner"}})
	rt.Spec.PodTemplate.Spec.PriorityClassName = priorityClassName
	return rt
}

// TestRunnerTemplate_PodTemplatePriorityClassBypass is the Q289 regression test for
// the v2 surface. podTemplate is a full PodTemplateSpec the AGC copies verbatim into
// the worker pod, so an ungated priorityClassName let a tenant name
// system-cluster-critical (value 2000000000, PreemptLowerPriority — shipped in every
// cluster, not restricted to kube-system) and preempt other tenants' pods. The
// reserved-field CEL rules never covered it.
func TestRunnerTemplate_PodTemplatePriorityClassBypass(t *testing.T) {
	t.Run("system-cluster-critical rejected under the secure default", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{} // nil allowlist == secure default
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateCreate(ctx, rtWithPriorityClass("system-cluster-critical"))
		require.Error(t, err, "a nil/empty allowlist must reject every named PriorityClass")
		assert.Contains(t, err.Error(), "podTemplate.spec.priorityClassName")
		assert.Contains(t, err.Error(), "system-cluster-critical")
		assert.Contains(t, strings.Join(*lines, "\n"), "admission denied", "denials must be audited")
	})

	t.Run("off-allowlist class rejected and names the allowed set", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{PriorityClasses: allowlist.New([]string{"runner-standard"})}
		_, err := v.ValidateCreate(context.Background(), rtWithPriorityClass("tenant-escalated"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tenant-escalated")
		assert.Contains(t, err.Error(), "runner-standard")
	})

	t.Run("allowlisted class admitted", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{PriorityClasses: allowlist.New([]string{"runner-standard"})}
		_, err := v.ValidateCreate(context.Background(), rtWithPriorityClass("runner-standard"))
		require.NoError(t, err)
	})

	t.Run("unset is always admitted, even with an empty allowlist", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{}
		_, err := v.ValidateCreate(context.Background(), rtWithPriorityClass(""))
		require.NoError(t, err, "an unprioritized worker pod must stay admissible under the secure default")
	})

	t.Run("a ConfigMap-sourced dynamic entry is honoured", func(t *testing.T) {
		al := allowlist.New(nil)
		al.SetDynamic([]string{"runner-burst"})
		v := &RunnerTemplateCustomValidator{PriorityClasses: al}
		_, err := v.ValidateCreate(context.Background(), rtWithPriorityClass("runner-burst"))
		require.NoError(t, err, "the Q188 dynamic half of the allowlist must apply here too")
	})

	t.Run("update cannot smuggle it in", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{PriorityClasses: allowlist.New([]string{"runner-standard"})}
		_, err := v.ValidateUpdate(context.Background(), rtWithPriorityClass("runner-standard"), rtWithPriorityClass("system-cluster-critical"))
		require.Error(t, err)
	})

	t.Run("ClusterRunnerTemplate is platform-authored and stays exempt", func(t *testing.T) {
		crt := newClusterRunnerTemplate("golden", []corev1.Container{{Name: "runner"}})
		crt.Spec.PodTemplate.Spec.PriorityClassName = "system-cluster-critical"
		_, err := (&ClusterRunnerTemplateCustomValidator{}).ValidateCreate(context.Background(), crt)
		require.NoError(t, err, "a tenant cannot create a cluster-scoped object, so the allowlist would only bind the platform")
	})
}

// TestRunnerTemplate_DeletionOnlyUpdateExemption covers the Q518 exemption: a
// deletion-only write (deletionTimestamp set, spec unchanged) on a template
// naming a since-removed class is admitted so teardown cannot wedge (Q499);
// live objects and spec changes on deleting ones stay denied.
func TestRunnerTemplate_DeletionOnlyUpdateExemption(t *testing.T) {
	v := &RunnerTemplateCustomValidator{} // nil allowlist: every named class is off-allowlist
	now := metav1.Now()

	deleting := func(finalizers ...string) *agcv2alpha1.RunnerTemplate {
		rt := rtWithPriorityClass("removed-class")
		rt.DeletionTimestamp = &now
		rt.Finalizers = finalizers
		return rt
	}

	t.Run("finalizer removal on a deleting template is admitted", func(t *testing.T) {
		_, err := v.ValidateUpdate(context.Background(), deleting("example.com/cleanup"), deleting())
		require.NoError(t, err)
	})

	t.Run("the same write on a live template is still denied", func(t *testing.T) {
		old := rtWithPriorityClass("removed-class")
		old.Finalizers = []string{"example.com/cleanup"}
		_, err := v.ValidateUpdate(context.Background(), old, rtWithPriorityClass("removed-class"))
		require.Error(t, err, "live objects keep the stored-object re-validation")
	})

	t.Run("a spec change on a deleting template is still denied", func(t *testing.T) {
		changed := deleting()
		changed.Spec.PodTemplate.Spec.Containers = append(changed.Spec.PodTemplate.Spec.Containers, corev1.Container{Name: "extra"})
		_, err := v.ValidateUpdate(context.Background(), deleting("example.com/cleanup"), changed)
		require.Error(t, err, "the exemption must not admit spec changes on a deleting object")
	})
}

func TestRunnerTemplateCustomValidator_ValidateCreate(t *testing.T) {
	v := &RunnerTemplateCustomValidator{}
	clean := corev1.Container{Name: "runner"}

	t.Run("clean template admitted", func(t *testing.T) {
		_, err := v.ValidateCreate(context.Background(), newRunnerTemplate("team-a", "rt", []corev1.Container{clean}))
		require.NoError(t, err)
	})

	t.Run("privileged container rejected and audited", func(t *testing.T) {
		priv := true
		privContainer := corev1.Container{Name: "runner", SecurityContext: &corev1.SecurityContext{Privileged: &priv}}
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateCreate(ctx, newRunnerTemplate("team-a", "rt", []corev1.Container{privContainer}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "privileged containers are not permitted")

		joined := strings.Join(*lines, "\n")
		assert.Contains(t, joined, "admission denied")
		assert.Contains(t, joined, "create")
		assert.Contains(t, joined, "team-a")
		assert.Contains(t, joined, "rt")
	})
}

func TestRunnerTemplateCustomValidator_ValidateUpdate(t *testing.T) {
	v := &RunnerTemplateCustomValidator{}
	clean := corev1.Container{Name: "runner"}
	oldObj := newRunnerTemplate("team-a", "rt", []corev1.Container{clean})

	t.Run("clean update admitted", func(t *testing.T) {
		_, err := v.ValidateUpdate(context.Background(), oldObj, newRunnerTemplate("team-a", "rt", []corev1.Container{clean}))
		require.NoError(t, err)
	})

	t.Run("update introducing reserved env rejected and audited", func(t *testing.T) {
		proxyEnv := corev1.Container{Name: "runner", Env: []corev1.EnvVar{{Name: "HTTP_PROXY", Value: "x"}}}
		newObj := newRunnerTemplate("team-a", "rt", []corev1.Container{proxyEnv})
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateUpdate(ctx, oldObj, newObj)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is reserved")

		joined := strings.Join(*lines, "\n")
		assert.Contains(t, joined, "admission denied")
		assert.Contains(t, joined, "update")
	})
}

func TestRunnerTemplateCustomValidator_ValidateDelete(t *testing.T) {
	v := &RunnerTemplateCustomValidator{}
	priv := true
	privContainer := corev1.Container{Name: "runner", SecurityContext: &corev1.SecurityContext{Privileged: &priv}}
	_, err := v.ValidateDelete(context.Background(), newRunnerTemplate("team-a", "rt", []corev1.Container{privContainer}))
	require.NoError(t, err, "delete is a no-op regardless of the template's contents")
}

func TestClusterRunnerTemplateCustomValidator_ValidateCreate(t *testing.T) {
	v := &ClusterRunnerTemplateCustomValidator{}

	t.Run("privileged container admitted (platform-owned kind)", func(t *testing.T) {
		priv := true
		privContainer := corev1.Container{Name: "runner", SecurityContext: &corev1.SecurityContext{Privileged: &priv}}
		_, err := v.ValidateCreate(context.Background(), newClusterRunnerTemplate("dind", []corev1.Container{privContainer}))
		require.NoError(t, err)
	})

	t.Run("reserved proxy env still rejected and audited", func(t *testing.T) {
		proxyEnv := corev1.Container{Name: "runner", Env: []corev1.EnvVar{{Name: "HTTPS_PROXY", Value: "x"}}}
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateCreate(ctx, newClusterRunnerTemplate("dind", []corev1.Container{proxyEnv}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is reserved")

		joined := strings.Join(*lines, "\n")
		assert.Contains(t, joined, "admission denied")
		assert.Contains(t, joined, "create")
		assert.Contains(t, joined, "dind")
	})
}

func TestClusterRunnerTemplateCustomValidator_ValidateUpdate(t *testing.T) {
	v := &ClusterRunnerTemplateCustomValidator{}
	clean := corev1.Container{Name: "runner"}
	oldObj := newClusterRunnerTemplate("dind", []corev1.Container{clean})

	t.Run("clean update admitted", func(t *testing.T) {
		_, err := v.ValidateUpdate(context.Background(), oldObj, newClusterRunnerTemplate("dind", []corev1.Container{clean}))
		require.NoError(t, err)
	})

	t.Run("update introducing reserved env rejected and audited", func(t *testing.T) {
		caEnv := corev1.Container{Name: "runner", Env: []corev1.EnvVar{{Name: "PROXY_CA_CERT_PATH", Value: "x"}}}
		newObj := newClusterRunnerTemplate("dind", []corev1.Container{caEnv})
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateUpdate(ctx, oldObj, newObj)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is reserved")

		joined := strings.Join(*lines, "\n")
		assert.Contains(t, joined, "admission denied")
		assert.Contains(t, joined, "update")
	})
}

func TestClusterRunnerTemplateCustomValidator_ValidateDelete(t *testing.T) {
	v := &ClusterRunnerTemplateCustomValidator{}
	_, err := v.ValidateDelete(context.Background(), newClusterRunnerTemplate("dind", nil))
	require.NoError(t, err, "delete is a no-op")
}

// TestReapBlockingSidecarWarning asserts the Q249 admission warning is emitted for a
// regular sidecar, suppressed by a native sidecar or the opt-out annotation, and —
// critically — never blocks admission in any case.
func TestReapBlockingSidecarWarning(t *testing.T) {
	runner := corev1.Container{Name: "runner"}
	dind := corev1.Container{Name: "dind"}
	always := corev1.ContainerRestartPolicyAlways
	nativeDind := corev1.Container{Name: "dind", RestartPolicy: &always}

	withSidecar := func(annotations map[string]string, containers, initContainers []corev1.Container) *agcv2alpha1.RunnerTemplate {
		return &agcv2alpha1.RunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "rt", Namespace: "team-a", Annotations: annotations},
			Spec:       *specWith(containers, initContainers),
		}
	}

	t.Run("regular sidecar warns but is not blocked", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{}
		obj := withSidecar(nil, []corev1.Container{runner, dind}, nil)
		warnings, err := v.ValidateCreate(context.Background(), obj)
		require.NoError(t, err, "a reap-blocking sidecar must never block admission")
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "dind")
		assert.Contains(t, warnings[0], agcv2alpha1.SelfExitingSidecarsAnnotation)
	})

	t.Run("native sidecar does not warn", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{}
		obj := withSidecar(nil, []corev1.Container{runner}, []corev1.Container{nativeDind})
		warnings, err := v.ValidateCreate(context.Background(), obj)
		require.NoError(t, err)
		assert.Empty(t, warnings)
	})

	t.Run("opt-out annotation suppresses the warning", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{}
		obj := withSidecar(map[string]string{agcv2alpha1.SelfExitingSidecarsAnnotation: "dind"},
			[]corev1.Container{runner, dind}, nil)
		warnings, err := v.ValidateCreate(context.Background(), obj)
		require.NoError(t, err)
		assert.Empty(t, warnings)
	})

	t.Run("warning also emitted on update", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{}
		obj := withSidecar(nil, []corev1.Container{runner, dind}, nil)
		warnings, err := v.ValidateUpdate(context.Background(), obj, obj)
		require.NoError(t, err)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "dind")
	})

	t.Run("cluster template warns on a regular sidecar without blocking", func(t *testing.T) {
		v := &ClusterRunnerTemplateCustomValidator{}
		obj := &agcv2alpha1.ClusterRunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "dind-golden"},
			Spec:       *specWith([]corev1.Container{runner, dind}, nil),
		}
		warnings, err := v.ValidateCreate(context.Background(), obj)
		require.NoError(t, err)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "dind")
	})
}

// TestLogRejection asserts that logRejection both returns the original error
// unmodified and emits a single audit line naming the kind, operation,
// namespace, name, and reason — the trail every admission denial above
// depends on.
func TestLogRejection(t *testing.T) {
	ctx, lines := ctxWithCapture()
	origErr := assert.AnError

	got := logRejection(ctx, "RunnerTemplate", "create", "team-a", "rt", origErr)
	require.Equal(t, origErr, got, "logRejection must return the original error unchanged")

	joined := strings.Join(*lines, "\n")
	assert.Contains(t, joined, "RunnerTemplate admission denied")
	assert.Contains(t, joined, "create")
	assert.Contains(t, joined, "team-a")
	assert.Contains(t, joined, "rt")
	assert.Contains(t, joined, origErr.Error())
}
