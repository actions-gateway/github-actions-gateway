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

package v1alpha1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
)

func newAG(namespace string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: namespace},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "github-app"},
		},
	}
}

func TestWebhook_RejectsKubeSystem(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), newAG("kube-system"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-system")
}

func TestWebhook_RejectsKubePublic(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), newAG("kube-public"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-public")
}

func TestWebhook_RejectsGMCSystem(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), newAG("actions-gateway-system"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "actions-gateway-system")
}

func TestWebhook_AllowsTenantNamespace(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), newAG("team-a"))
	require.NoError(t, err)
}

func TestWebhook_UpdateNoOp(t *testing.T) {
	v := &ActionsGatewayCustomValidator{}
	_, err := v.ValidateUpdate(context.Background(), newAG("kube-system"), newAG("kube-system"))
	require.NoError(t, err)
}
