/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v2alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
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
