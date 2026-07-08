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
	"net"
	"strings"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mustCIDR parses s as a CIDR, failing the test on error. Test helper only.
func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	require.NoError(t, err)
	return n
}

func newEgressProxy(namespace, name string, fqdns, cidrs []string) *agcv2alpha1.EgressProxy {
	return &agcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.EgressProxySpec{
			DestinationFQDNs: fqdns,
			DestinationCIDRs: cidrs,
		},
	}
}

func TestValidateEgressDestinations(t *testing.T) {
	list := allowlist.NewEgressDestination(
		[]string{"golang.org"},
		[]*net.IPNet{mustCIDR(t, "10.0.0.0/8")},
	)

	tests := []struct {
		name            string
		spec            *agcv2alpha1.EgressProxySpec
		list            *allowlist.EgressDestinationAllowlist
		wantErr         bool
		wantErrContains string
	}{
		{
			name: "no extra destinations admitted",
			spec: &agcv2alpha1.EgressProxySpec{},
			list: list,
		},
		{
			name: "allowlisted FQDN admitted",
			spec: &agcv2alpha1.EgressProxySpec{DestinationFQDNs: []string{"proxy.golang.org"}},
			list: list,
		},
		{
			name:            "off-allowlist FQDN rejected",
			spec:            &agcv2alpha1.EgressProxySpec{DestinationFQDNs: []string{"evil.example.com"}},
			list:            list,
			wantErr:         true,
			wantErrContains: "destinationFQDNs",
		},
		{
			name: "allowlisted CIDR admitted",
			spec: &agcv2alpha1.EgressProxySpec{DestinationCIDRs: []string{"10.1.0.0/16"}},
			list: list,
		},
		{
			name:            "off-allowlist CIDR rejected",
			spec:            &agcv2alpha1.EgressProxySpec{DestinationCIDRs: []string{"192.168.0.0/16"}},
			list:            list,
			wantErr:         true,
			wantErrContains: "destinationCIDRs",
		},
		{
			name:            "malformed CIDR rejected as defense in depth",
			spec:            &agcv2alpha1.EgressProxySpec{DestinationCIDRs: []string{"not-a-cidr"}},
			list:            list,
			wantErr:         true,
			wantErrContains: "not a valid CIDR",
		},
		{
			name:            "nil allowlist denies everything (secure default)",
			spec:            &agcv2alpha1.EgressProxySpec{DestinationFQDNs: []string{"golang.org"}},
			list:            nil,
			wantErr:         true,
			wantErrContains: "destinationFQDNs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEgressDestinations(tc.spec, tc.list)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrContains)
		})
	}
}

func TestEgressProxyCustomValidator_ValidateCreate(t *testing.T) {
	list := allowlist.NewEgressDestination([]string{"golang.org"}, nil)
	v := &EgressProxyCustomValidator{Allowlist: list}

	t.Run("valid destination admitted", func(t *testing.T) {
		_, err := v.ValidateCreate(context.Background(), newEgressProxy("team-a", "ep", []string{"golang.org"}, nil))
		require.NoError(t, err)
	})

	t.Run("invalid destination rejected and audited", func(t *testing.T) {
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateCreate(ctx, newEgressProxy("team-a", "ep", []string{"evil.example.com"}, nil))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not permitted")

		joined := strings.Join(*lines, "\n")
		assert.Contains(t, joined, "admission denied")
		assert.Contains(t, joined, "create")
		assert.Contains(t, joined, "team-a")
		assert.Contains(t, joined, "ep")
	})
}

func TestEgressProxyCustomValidator_ValidateUpdate(t *testing.T) {
	list := allowlist.NewEgressDestination([]string{"golang.org"}, nil)
	v := &EgressProxyCustomValidator{Allowlist: list}
	oldObj := newEgressProxy("team-a", "ep", nil, nil)

	t.Run("widening to a valid destination admitted", func(t *testing.T) {
		newObj := newEgressProxy("team-a", "ep", []string{"golang.org"}, nil)
		_, err := v.ValidateUpdate(context.Background(), oldObj, newObj)
		require.NoError(t, err)
	})

	t.Run("widening to an invalid destination rejected and audited", func(t *testing.T) {
		newObj := newEgressProxy("team-a", "ep", []string{"evil.example.com"}, nil)
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateUpdate(ctx, oldObj, newObj)
		require.Error(t, err)

		joined := strings.Join(*lines, "\n")
		assert.Contains(t, joined, "admission denied")
		assert.Contains(t, joined, "update")
	})
}

func TestEgressProxyCustomValidator_ValidateDelete(t *testing.T) {
	v := &EgressProxyCustomValidator{Allowlist: allowlist.NewEgressDestination(nil, nil)}
	_, err := v.ValidateDelete(context.Background(), newEgressProxy("team-a", "ep", []string{"anything-goes.example.com"}, nil))
	require.NoError(t, err, "delete is a no-op regardless of allowlist state")
}

// epWithMode builds a minimal EgressProxy carrying only an egressPolicyMode, for the
// Q245 intent/backend admission cases.
func epWithMode(mode agcv2alpha1.EgressPolicyMode) *agcv2alpha1.EgressProxy {
	return &agcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "team-a"},
		Spec:       agcv2alpha1.EgressProxySpec{EgressPolicyMode: mode},
	}
}

// TestEgressProxyCustomValidator_FQDNBackend covers the Q245 intent/backend split at
// admission: FQDN intent is rejected when the cluster declares no backend (fail-closed
// and loud), admitted when a backend is configured, and CIDR/deprecated modes are never
// gated by the backend.
func TestEgressProxyCustomValidator_FQDNBackend(t *testing.T) {
	list := allowlist.NewEgressDestination(nil, nil)

	cases := []struct {
		name    string
		mode    agcv2alpha1.EgressPolicyMode
		backend controller.FQDNBackend
		wantErr bool
	}{
		{"FQDN + none rejected", agcv2alpha1.EgressPolicyModeFQDN, controller.FQDNBackendNone, true},
		{"FQDN + empty (zero value) rejected", agcv2alpha1.EgressPolicyModeFQDN, "", true},
		{"FQDN + cilium admitted", agcv2alpha1.EgressPolicyModeFQDN, controller.FQDNBackendCilium, false},
		{"FQDN + gke admitted", agcv2alpha1.EgressPolicyModeFQDN, controller.FQDNBackendGKE, false},
		{"CIDR + none admitted", agcv2alpha1.EgressPolicyModeCIDR, controller.FQDNBackendNone, false},
		{"deprecated Cilium + none admitted", agcv2alpha1.EgressPolicyModeCiliumFQDN, controller.FQDNBackendNone, false},
		{"deprecated Calico + none admitted", agcv2alpha1.EgressPolicyModeCalicoFQDN, controller.FQDNBackendNone, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &EgressProxyCustomValidator{Allowlist: list, FQDNBackend: tc.backend}
			_, err := v.ValidateCreate(context.Background(), epWithMode(tc.mode))
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "--fqdn-policy-backend")
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestEgressProxyCustomValidator_DeprecationWarnings asserts the deprecated CNI-specific
// modes are admitted with a non-blocking warning, while FQDN/CIDR emit none.
func TestEgressProxyCustomValidator_DeprecationWarnings(t *testing.T) {
	v := &EgressProxyCustomValidator{
		Allowlist:   allowlist.NewEgressDestination(nil, nil),
		FQDNBackend: controller.FQDNBackendCilium,
	}

	cases := []struct {
		mode      agcv2alpha1.EgressPolicyMode
		wantWarn  bool
		wantToken string
	}{
		{agcv2alpha1.EgressPolicyModeCiliumFQDN, true, "CiliumFQDN is deprecated"},
		{agcv2alpha1.EgressPolicyModeCalicoFQDN, true, "CalicoFQDN is deprecated"},
		{agcv2alpha1.EgressPolicyModeFQDN, false, ""},
		{agcv2alpha1.EgressPolicyModeCIDR, false, ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			warns, err := v.ValidateCreate(context.Background(), epWithMode(tc.mode))
			require.NoError(t, err)
			if !tc.wantWarn {
				assert.Empty(t, warns)
				return
			}
			require.Len(t, warns, 1)
			assert.Contains(t, warns[0], tc.wantToken)
		})
	}
}
