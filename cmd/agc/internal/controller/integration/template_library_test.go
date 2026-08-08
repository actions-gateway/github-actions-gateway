//go:build integration

package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// templateLibraryDir is the shipped runner template library, relative to this
// package. Same hop count as the suite's api/config/crd path.
const templateLibraryDir = "../../../../../deploy/templates"

// TestTemplateLibrary_Admitted applies every shipped library entry to a real
// apiserver (Q554).
//
// The library's admission rule is that only what CI exercises may ship, because
// a golden template is an implicit claim that it works. The dogfood e2e lane
// backs that claim for the two DinD entries by running real jobs on them, but it
// is on-demand and covers no other entry. This is the part that runs for every
// entry on every integration run, and it is the only thing that exercises `plain`
// at all.
//
// What a real apiserver settles that reading the YAML cannot: the CRD's
// structural schema, and the RunnerTemplateSpec CEL guardrails, which reject a
// template that sets a reserved pod field (serviceAccountName, hostPID,
// hostNetwork, hostIPC, or automountServiceAccountToken enabled) and a name over
// the 52-character templateRef budget. Those are author-time rejections by
// design, so a library entry that trips one is unappliable rather than silently
// rewritten.
func TestTemplateLibrary_Admitted(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(templateLibraryDir, "*", "template.yaml"))
	require.NoError(t, err)
	// An empty glob admits every entry vacuously, which reads exactly like a
	// pass. The library ships three entries; require the floor rather than the
	// exact count, so adding one does not fail here for the wrong reason.
	require.GreaterOrEqualf(t, len(paths), 3, "found %d templates under %s; the glob is not reaching the library", len(paths), templateLibraryDir)

	for _, path := range paths {
		entry := filepath.Base(filepath.Dir(path))
		t.Run(entry, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)

			var crt apiv2beta1.ClusterRunnerTemplate
			require.NoError(t, yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096).Decode(&crt))
			require.Equal(t, "ClusterRunnerTemplate", crt.Kind)
			require.Equal(t, entry, crt.Name, "the directory name is what templateRef points at")

			require.NoError(t, k8sClient.Create(ctx, &crt), "the apiserver rejected shipped library entry %q", entry)
			t.Cleanup(func() {
				_ = k8sClient.Delete(context.Background(), &crt)
			})

			// Round-trip it back: a create the apiserver accepted but pruned
			// fields out of would still pass the line above.
			var got apiv2beta1.ClusterRunnerTemplate
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(&crt), &got))
			require.Len(t, got.Spec.PodTemplate.Spec.Containers, len(crt.Spec.PodTemplate.Spec.Containers))
			require.Len(t, got.Spec.PodTemplate.Spec.InitContainers, len(crt.Spec.PodTemplate.Spec.InitContainers))
		})
	}
}
