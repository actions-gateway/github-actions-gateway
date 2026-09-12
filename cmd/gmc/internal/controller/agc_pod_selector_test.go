package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/api/apilabels"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// TestAGCPodSelectorIsVersionNeutral pins the fact the operator docs select on.
//
// The bare `app` label is per-gateway under v2 and fixed under v1, so
// `-l app=actions-gateway-controller` finds nothing on a v2 tenant. The
// recommended `app.kubernetes.io/name` label carries agcAppName under both, so it
// is the one selector an operator can run against a mixed fleet. Q1099 rewrote
// every operator-facing AGC pod selector onto it; this test is what keeps that
// rewrite true.
func TestAGCPodSelectorIsVersionNeutral(t *testing.T) {
	v1Pod := buildAGCDeployment(newTestAG("gateway", "team-a"), "agc:test", "http://proxy:8080", nil).Spec.Template.Labels
	v2AG := v2Gateway("gw", "team-a", "github-app", "shared")
	v2Pod := buildAGCDeploymentV2(v2AG, "agc:test", nil, gmcv2alpha1.SecurityProfileRestricted, nil).Spec.Template.Labels

	// The bare `app` label is what diverges, which is why the docs could not keep it.
	assert.Equal(t, agcAppName, v1Pod["app"])
	assert.Equal(t, agcNameV2(v2AG), v2Pod["app"])
	require.NotEqual(t, v1Pod["app"], v2Pod["app"],
		"if these ever converge, the version-neutral selector below is no longer load-bearing")

	// The recommended label does not, so one selector reaches both.
	assert.Equal(t, agcAppName, v1Pod[apilabels.Name])
	assert.Equal(t, agcAppName, v2Pod[apilabels.Name])
}
