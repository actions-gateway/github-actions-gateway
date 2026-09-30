//go:build integration

package integration_test

import (
	"testing"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/stretchr/testify/require"
)

// The v2 validators receive every write as its v2 view: the apiserver converts a
// v2alpha1 write through the v2beta1 hub before admission. A test that calls a
// validator directly with a v2alpha1 fixture converts it the same way.

func v2GatewayView(t *testing.T, in *agcv2alpha1.ActionsGateway) *v2.ActionsGateway {
	t.Helper()
	hub := &v2beta1.ActionsGateway{}
	require.NoError(t, in.ConvertTo(hub))
	out := &v2.ActionsGateway{}
	require.NoError(t, out.ConvertFrom(hub))
	return out
}

func v2RunnerSetView(t *testing.T, in *agcv2alpha1.RunnerSet) *v2.RunnerSet {
	t.Helper()
	hub := &v2beta1.RunnerSet{}
	require.NoError(t, in.ConvertTo(hub))
	out := &v2.RunnerSet{}
	require.NoError(t, out.ConvertFrom(hub))
	return out
}

func v2RunnerTemplateView(t *testing.T, in *agcv2alpha1.RunnerTemplate) *v2.RunnerTemplate {
	t.Helper()
	hub := &v2beta1.RunnerTemplate{}
	require.NoError(t, in.ConvertTo(hub))
	out := &v2.RunnerTemplate{}
	require.NoError(t, out.ConvertFrom(hub))
	return out
}
