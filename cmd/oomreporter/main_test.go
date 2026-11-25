package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

var noLog = slog.New(slog.DiscardHandler)

func env(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }

// tried runs args with no cluster to reach, and returns what stopped it.
func tried(vars map[string]string, args ...string) error {
	return run(context.Background(), args, env(vars), func() (*rest.Config, error) { return &rest.Config{Host: "http://127.0.0.1:1"}, nil }, noLog)
}

func TestRun_RefusesWhatItCannotRunWith(t *testing.T) {
	const agents = "-agents=app=agent"
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no mode":                          {nil, "want a mode"},
		"another mode":                     {[]string{"sidecar"}, `not "sidecar"`},
		"an argument":                      {[]string{"agent", "node-a"}, `unexpected argument "node-a"`},
		"another flag":                     {[]string{"agent", "-agents=a=b"}, "not defined"},
		"an agent with no node":            {[]string{"agent", "-namespace=oom"}, "want -node"},
		"an agent with no namespace":       {[]string{"agent", "-node=node-a"}, "want -node and -namespace"},
		"an agent that never polls":        {[]string{"agent", "-node=node-a", "-namespace=oom", "-poll=0s"}, "-poll above 0"},
		"a controller with no namespace":   {[]string{"controller", agents, "-all-namespaces"}, "want -namespace"},
		"a controller with no agents":      {[]string{"controller", "-namespace=oom", "-all-namespaces"}, "want -agents"},
		"a controller with a range of one": {[]string{"controller", "-namespace=oom", "-agents=app in (a,b)", "-all-namespaces"}, "want -agents"},
		"a controller with a range too":    {[]string{"controller", "-namespace=oom", "-agents=app=a,b in (c)", "-all-namespaces"}, "want -agents"},
		"a controller with half a label":   {[]string{"controller", "-namespace=oom", "-agents=app=a=b", "-all-namespaces"}, "-agents:"},
		"a controller with nowhere":        {[]string{"controller", "-namespace=oom", agents}, "want one of -namespaces"},
		"a controller with only commas":    {[]string{"controller", "-namespace=oom", agents, "-namespaces= ,"}, "want one of -namespaces"},
		"a controller with everywhere too": {[]string{"controller", "-namespace=oom", agents, "-namespaces=a", "-all-namespaces"}, "want one of -namespaces"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, tried(nil, tc.args...), tc.want)
		})
	}
}

func TestRun_HelpIsNoFailure(t *testing.T) {
	require.NoError(t, tried(nil, "agent", "-h"))
}

func TestRun_TakesItsPlaceFromTheEnvironment(t *testing.T) {
	vars := map[string]string{"NODE_NAME": "node-a", "POD_NAMESPACE": "oom"}
	err := tried(vars, "agent", "-cgroup-mount="+t.TempDir())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "want -node")
	require.ErrorContains(t, tried(vars, "controller", "-agents=app=agent"), "want one of -namespaces")
}

func TestRun_NeedsAClusterItCanReach(t *testing.T) {
	for _, args := range [][]string{{"agent", "-node=node-a", "-namespace=oom"}, {"controller", "-namespace=oom", "-agents=app=agent", "-all-namespaces"}} {
		bad := func() (*rest.Config, error) { return &rest.Config{Host: "http://[::1"}, nil }
		require.ErrorContains(t, run(t.Context(), args, env(nil), bad, noLog), "host must be a URL")
	}
}

func TestRun_NeedsACluster(t *testing.T) {
	none := errors.New("no cluster")
	err := run(t.Context(), []string{"agent"}, env(nil), func() (*rest.Config, error) { return nil, none }, noLog)
	require.ErrorIs(t, err, none)
}
