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
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// runnerSetValidatorWith returns a validator whose reader is a fake client preloaded
// with the given sibling RunnerSets, so the ScaleSet label-uniqueness guard can be
// exercised without a live apiserver. Production wires mgr.GetAPIReader().
func runnerSetValidatorWith(t *testing.T, existing ...*agcv2alpha1.RunnerSet) *RunnerSetCustomValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agcv2alpha1.AddToScheme(scheme))
	objs := make([]client.Object, 0, len(existing))
	for _, rs := range existing {
		objs = append(objs, rs)
	}
	return &RunnerSetCustomValidator{
		reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
	}
}

// scaleSetRS builds a ScaleSet-protocol RunnerSet with a single runnerLabel bound to
// the named gateway.
func scaleSetRS(name, namespace, gateway, label string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:          agcv2alpha1.ObjectRef{Name: gateway},
			AcquisitionProtocol: agcv2alpha1.AcquisitionProtocolScaleSet,
			RunnerLabels:        []string{label},
		},
	}
}

// classicRS builds a Classic-protocol RunnerSet (default) with the given labels.
func classicRS(name, namespace, gateway string, labels ...string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:          agcv2alpha1.ObjectRef{Name: gateway},
			AcquisitionProtocol: agcv2alpha1.AcquisitionProtocolClassic,
			RunnerLabels:        labels,
		},
	}
}

func TestRunnerSetWebhook_ClassicIsNeverChecked(t *testing.T) {
	// Two Classic sets sharing a label under one gateway are fine: no scale-set
	// object exists, so there is no name collision at GitHub. Even with a colliding
	// ScaleSet sibling preloaded, a Classic create is admitted.
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), classicRS("newset", "tenant", "gw", "linux", "amd64"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_RejectsDuplicateScaleSetLabel(t *testing.T) {
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw", "linux"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "linux")
	assert.Contains(t, err.Error(), "existing")
}

func TestRunnerSetWebhook_AllowsDistinctScaleSetLabels(t *testing.T) {
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw", "windows"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_SameLabelDifferentGatewayIsAllowed(t *testing.T) {
	// Two gateways register their scale sets against different GitHub bindings, so
	// the same label under different gateways cannot collide.
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant", "gw-a", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw-b", "linux"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_SameLabelDifferentNamespaceIsAllowed(t *testing.T) {
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant-a", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant-b", "gw", "linux"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_DuplicateAgainstClassicSiblingIsAllowed(t *testing.T) {
	// A Classic sibling with the same label does not register a scale set, so it
	// cannot collide with a new ScaleSet set claiming that label.
	v := runnerSetValidatorWith(t, classicRS("existing", "tenant", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw", "linux"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_UpdateOntoCollidingLabelIsRejected(t *testing.T) {
	// The set already exists under a distinct label; an update that moves it onto a
	// sibling's label is rejected (acquisitionProtocol is immutable, but labels are not).
	sibling := scaleSetRS("sibling", "tenant", "gw", "linux")
	self := scaleSetRS("self", "tenant", "gw", "windows")
	v := runnerSetValidatorWith(t, sibling, self)

	moved := scaleSetRS("self", "tenant", "gw", "linux")
	_, err := v.ValidateUpdate(context.Background(), self, moved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sibling")
}

func TestRunnerSetWebhook_UpdateSelfNoOpIsAllowed(t *testing.T) {
	// An update that does not change the label must not self-collide with the set's
	// own persisted copy in the list.
	self := scaleSetRS("self", "tenant", "gw", "linux")
	v := runnerSetValidatorWith(t, self)
	_, err := v.ValidateUpdate(context.Background(), self, scaleSetRS("self", "tenant", "gw", "linux"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_NilReaderSkips(t *testing.T) {
	// The direct-construction path (no reader) is a no-op — production always wires one.
	v := &RunnerSetCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw", "linux"))
	require.NoError(t, err)
}
