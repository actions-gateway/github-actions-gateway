//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const values = `
namespaces: [` + tenant + `]
image:
  digest: ` + digest + `
`

// render is the chart as helm renders it with values, in the order it installs it in.
func render(ctx context.Context, values string) ([]unstructured.Unstructured, error) {
	// Read by this process, so that the test cache sees a change of it.
	dir, err := os.MkdirTemp("", "chart-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.CopyFS(dir, os.DirFS("testdata/chart")); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "test.yaml"), []byte(values), 0o600); err != nil {
		return nil, err
	}
	helm := exec.CommandContext(ctx, "helm", "template", release, ".", "--namespace", reporter, "--values", "test.yaml")
	helm.Dir = dir
	out, err := helm.Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("helm template: %w: %s", err, exit.Stderr)
		}
		return nil, fmt.Errorf("helm template: %w", err)
	}
	return decode(out)
}

func decode(data []byte) (objects []unstructured.Unstructured, err error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var u unstructured.Unstructured
		if err := dec.Decode(&u); errors.Is(err, io.EOF) {
			return objects, nil
		} else if err != nil {
			return nil, err
		}
		if len(u.Object) > 0 {
			objects = append(objects, u)
		}
	}
}

// held is what a test holds objects to, by kind and name: all but their labels, and the rules of the
// policies, which the API server's answers are.
func held(objects []unstructured.Unstructured) map[string]map[string]any {
	by := map[string]map[string]any{}
	for _, u := range objects {
		u.SetLabels(nil)
		if u.GetKind() == "ValidatingAdmissionPolicy" {
			unstructured.RemoveNestedField(u.Object, "spec", "variables")
			unstructured.RemoveNestedField(u.Object, "spec", "validations")
		}
		by[strings.Join([]string{u.GetKind(), u.GetNamespace(), u.GetName()}, " ")] = u.Object
	}
	return by
}

func rendered(t *testing.T, values string) map[string]map[string]any {
	t.Helper()
	objects, err := render(t.Context(), values)
	require.NoError(t, err)
	by := held(objects)
	require.Len(t, by, len(objects))
	return by
}

// Every grant, and what each pod may do to its node and the network. A change of the chart is made
// in the file too, by hand.
func TestChart_GrantsAndRunsThisAndNoMore(t *testing.T) {
	data, err := os.ReadFile("testdata/chart.yaml")
	require.NoError(t, err)
	want, err := decode(data)
	require.NoError(t, err)
	assert.Equal(t, held(want), rendered(t, values))
}

func without(by map[string]map[string]any, in string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for k, v := range by {
		if !strings.Contains(k, in) {
			out[k] = v
		}
	}
	return out
}

func TestChart_AllNamespacesAreAClusterRoleInPlaceOfEach(t *testing.T) {
	const controller, pods = "Deployment " + reporter + " " + controllerAccount, reporter + "." + release + "-pods"
	each := rendered(t, values)
	all := rendered(t, strings.Replace(values, "namespaces: ["+tenant+"]", "allNamespaces: true", 1))
	assert.Equal(t, without(without(each, pods), "Deployment"), without(without(all, pods), "Deployment"))

	assert.Equal(t, each["Role "+tenant+" "+pods]["rules"], all["ClusterRole  "+pods]["rules"])
	binding := all["ClusterRoleBinding  "+pods]
	assert.Equal(t, each["RoleBinding "+tenant+" "+pods]["subjects"], binding["subjects"])
	assert.Equal(t, map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": pods}, binding["roleRef"])
	assert.Len(t, all, len(each))

	args := func(by map[string]map[string]any) []any {
		containers, _, err := unstructured.NestedSlice(by[controller], "spec", "template", "spec", "containers")
		require.NoError(t, err)
		args, _, err := unstructured.NestedSlice(containers[0].(map[string]any), "args")
		require.NoError(t, err)
		return args
	}
	assert.Equal(t, append(args(each)[:len(args(each))-1], "-all-namespaces"), args(all))
}

func TestChart_TaintsRequireNoLabelAndChangeNothingElse(t *testing.T) {
	const controller = "Deployment " + reporter + " " + controllerAccount
	want := rendered(t, values)
	unstructured.RemoveNestedField(want[controller], "spec", "template", "spec", "affinity")
	assert.Equal(t, want, rendered(t, values+"controller:\n  placement: Taints\n"))
}

func TestChart_EgressIsNarrowedToTheAddressesGiven(t *testing.T) {
	const policy = "NetworkPolicy " + reporter + " " + release + "-api-server-ports"
	want := rendered(t, values)
	require.NoError(t, unstructured.SetNestedSlice(want[policy], []any{map[string]any{
		"ports": []any{map[string]any{"port": int64(6443), "protocol": "TCP"}},
		"to":    []any{map[string]any{"ipBlock": map[string]any{"cidr": "10.0.0.1/32"}}},
	}}, "spec", "egress"))
	assert.Equal(t, want, rendered(t, values+"networkPolicy:\n  apiServerPorts: [6443]\n  apiServerCIDRs: [10.0.0.1/32]\n"))
}

// Two releases of one name in two namespaces may name one tenant, and each keeps its policies.
func TestChart_SharesNoNameWithAReleaseInAnotherNamespace(t *testing.T) {
	for k := range rendered(t, values) {
		kind, rest, _ := strings.Cut(k, " ")
		ns, name, _ := strings.Cut(rest, " ")
		if ns != reporter {
			assert.True(t, strings.HasPrefix(name, reporter+"."), "%s %s", kind, name)
		}
	}
}

func TestChart_RefusesToBeInATenantsNamespace(t *testing.T) {
	_, err := render(t.Context(), strings.Replace(values, tenant, reporter, 1))
	require.ErrorContains(t, err, "namespaces holds the release's own")
}

// The option adds one container to the agent's pod, and moves the agent's token into its own
// container, so that the log reader has none.
func TestChart_TheKernelLogAddsAContainerWithOneCapabilityAndNoToken(t *testing.T) {
	const agent = "DaemonSet " + reporter + " " + release + "-agent"
	off := rendered(t, values)
	on := rendered(t, values+"kernelLog:\n  enabled: true\n")
	assert.Equal(t, without(off, "DaemonSet"), without(on, "DaemonSet"))

	pod, _, err := unstructured.NestedMap(on[agent], "spec", "template", "spec")
	require.NoError(t, err)
	assert.Equal(t, false, pod["automountServiceAccountToken"])
	assert.NotContains(t, pod, "shareProcessNamespace")
	assert.NotContains(t, pod, "hostUsers")
	containers := pod["containers"].([]any)
	require.Len(t, containers, 2)
	agentContainer, reader := containers[0].(map[string]any), containers[1].(map[string]any)
	assert.Equal(t, "log-reader", reader["name"])
	assert.Equal(t, map[string]any{
		"runAsNonRoot": false, "runAsUser": int64(0), "runAsGroup": int64(0),
		"readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false,
		"capabilities": map[string]any{"drop": []any{"ALL"}, "add": []any{"SYSLOG"}},
	}, reader["securityContext"])
	assert.NotContains(t, reader, "env")
	assert.Equal(t, []any{map[string]any{"name": "log-socket", "mountPath": "/run/oom-reporter"}}, reader["volumeMounts"])

	mounts := map[string]bool{}
	for _, m := range agentContainer["volumeMounts"].([]any) {
		mounts[m.(map[string]any)["name"].(string)] = true
	}
	assert.Equal(t, map[string]bool{"cgroup": true, "token": true, "log-socket": true}, mounts)
	assert.Contains(t, agentContainer["args"], "-log-socket=/run/oom-reporter/log.sock")

	volumes := map[string]map[string]any{}
	for _, v := range pod["volumes"].([]any) {
		volumes[v.(map[string]any)["name"].(string)] = v.(map[string]any)
	}
	assert.NotContains(t, volumes["log-socket"], "hostPath")
	assert.Contains(t, volumes["token"], "projected")
}
