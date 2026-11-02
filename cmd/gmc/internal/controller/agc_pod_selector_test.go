package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/api/apilabels"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// TestAGCPodSelectorIsStable pins the fact the operator docs select on.
//
// The bare `app` label is per-gateway, so `-l app=actions-gateway-controller` finds
// nothing. The recommended `app.kubernetes.io/name` label carries agcAppName on every
// gateway, so it is the one selector an operator can run across a fleet. Q1099
// rewrote every operator-facing AGC pod selector onto it; this test is what keeps
// that rewrite true.
func TestAGCPodSelectorIsStable(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	pod := buildAGCDeploymentV2(ag, "agc:test", nil, gmcv2alpha1.SecurityProfileRestricted, nil).Spec.Template.Labels

	// The bare `app` label is per-gateway, which is why the docs could not keep it.
	require.Equal(t, agcNameV2(ag), pod["app"])
	require.NotEqual(t, agcAppName, pod["app"],
		"if these ever converge, the recommended selector below is no longer load-bearing")

	assert.Equal(t, agcAppName, pod[apilabels.Name])
}
