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
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func specWith(containers, initContainers []corev1.Container) *agcv2alpha1.RunnerTemplateSpec {
	return &agcv2alpha1.RunnerTemplateSpec{
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: containers, InitContainers: initContainers},
		},
	}
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
