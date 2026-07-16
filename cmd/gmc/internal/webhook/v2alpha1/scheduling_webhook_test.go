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
	"strings"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateSchedulingPriorityClass(t *testing.T) {
	list := allowlist.New([]string{"gag-infra-critical"})

	cases := []struct {
		name    string
		s       *agcv2alpha1.PodScheduling
		list    *allowlist.PriorityClassAllowlist
		wantErr bool
	}{
		{"nil scheduling passes", nil, list, false},
		{"empty priorityClassName always passes", &agcv2alpha1.PodScheduling{}, list, false},
		{"allowlisted class admitted", &agcv2alpha1.PodScheduling{PriorityClassName: "gag-infra-critical"}, list, false},
		{"off-allowlist class rejected", &agcv2alpha1.PodScheduling{PriorityClassName: "system-cluster-critical"}, list, true},
		// The secure default: a nil allowlist forbids every named class but still
		// permits the unset case.
		{"nil allowlist forbids named class", &agcv2alpha1.PodScheduling{PriorityClassName: "anything"}, nil, true},
		{"nil allowlist permits empty name", &agcv2alpha1.PodScheduling{}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSchedulingPriorityClass(tc.s, tc.list)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "infra allowlist")
			assert.Contains(t, err.Error(), "--allowed-infra-priority-classes")
		})
	}
}

func newV2Gateway(ns, name, priorityClass string) *agcv2alpha1.ActionsGateway {
	ag := &agcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		// gitHubURL is required (CRD MinLength=1) and the webhook now validates it
		// structurally (Q323), so the helper carries a well-formed value.
		Spec: agcv2alpha1.ActionsGatewaySpec{GitHubURL: "https://github.com/example-org"},
	}
	if priorityClass != "" {
		ag.Spec.Scheduling = &agcv2alpha1.PodScheduling{PriorityClassName: priorityClass}
	}
	return ag
}

func TestActionsGatewayCustomValidator_ValidateCreate(t *testing.T) {
	v := &ActionsGatewayCustomValidator{InfraPriorityClasses: allowlist.New([]string{"gag-infra-critical"})}

	t.Run("allowlisted class admitted", func(t *testing.T) {
		_, err := v.ValidateCreate(context.Background(), newV2Gateway("team-a", "gw", "gag-infra-critical"))
		require.NoError(t, err)
	})

	t.Run("no scheduling admitted", func(t *testing.T) {
		_, err := v.ValidateCreate(context.Background(), newV2Gateway("team-a", "gw", ""))
		require.NoError(t, err)
	})

	t.Run("off-allowlist class rejected and audited", func(t *testing.T) {
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateCreate(ctx, newV2Gateway("team-a", "gw", "system-cluster-critical"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "infra allowlist")

		joined := strings.Join(*lines, "\n")
		assert.Contains(t, joined, "ActionsGateway admission denied")
		assert.Contains(t, joined, "create")
		assert.Contains(t, joined, "team-a")
	})
}

func TestActionsGatewayCustomValidator_ValidateUpdate(t *testing.T) {
	v := &ActionsGatewayCustomValidator{InfraPriorityClasses: allowlist.New([]string{"gag-infra-critical"})}
	oldObj := newV2Gateway("team-a", "gw", "")

	t.Run("editing to an off-allowlist class rejected", func(t *testing.T) {
		_, err := v.ValidateUpdate(context.Background(), oldObj, newV2Gateway("team-a", "gw", "system-cluster-critical"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "infra allowlist")
	})

	t.Run("editing to an allowlisted class admitted", func(t *testing.T) {
		_, err := v.ValidateUpdate(context.Background(), oldObj, newV2Gateway("team-a", "gw", "gag-infra-critical"))
		require.NoError(t, err)
	})
}

func TestActionsGatewayCustomValidator_ValidateDelete(t *testing.T) {
	v := &ActionsGatewayCustomValidator{InfraPriorityClasses: nil}
	_, err := v.ValidateDelete(context.Background(), newV2Gateway("team-a", "gw", "system-cluster-critical"))
	require.NoError(t, err, "delete is a no-op regardless of allowlist state")
}

// TestEgressProxyCustomValidator_InfraPriorityClass covers the Q284 gate on the
// EgressProxy path: the same shared helper, wired through the EgressProxy validator.
func TestEgressProxyCustomValidator_InfraPriorityClass(t *testing.T) {
	v := &EgressProxyCustomValidator{
		Allowlist:            allowlist.NewEgressDestination(nil, nil),
		InfraPriorityClasses: allowlist.New([]string{"gag-infra-critical"}),
	}

	epWithClass := func(class string) *agcv2alpha1.EgressProxy {
		return &agcv2alpha1.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "team-a"},
			Spec:       agcv2alpha1.EgressProxySpec{Scheduling: &agcv2alpha1.PodScheduling{PriorityClassName: class}},
		}
	}

	t.Run("allowlisted class admitted", func(t *testing.T) {
		_, err := v.ValidateCreate(context.Background(), epWithClass("gag-infra-critical"))
		require.NoError(t, err)
	})
	t.Run("off-allowlist class rejected", func(t *testing.T) {
		_, err := v.ValidateCreate(context.Background(), epWithClass("system-cluster-critical"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "infra allowlist")
	})
}
