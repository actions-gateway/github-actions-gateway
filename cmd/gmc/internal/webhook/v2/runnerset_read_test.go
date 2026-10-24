package v2

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// countingReader counts List calls per list type on top of a real reader.
type countingReader struct {
	client.Reader
	lists map[string]int
}

func (r *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists[fmt.Sprintf("%T", list)]++
	return r.Reader.List(ctx, list, opts...)
}

func TestRunnerSetWebhook_SharedReadListsEachKindOnce(t *testing.T) {
	// Q1075: the scale-set name guard and the agent-identity guard both run on a
	// ScaleSet set, and each kind they read is listed once per request between them.
	ctx := context.Background()
	want := map[string]int{
		"*v1alpha1.ActionsGatewayList": 1,
		"*v2alpha1.ActionsGatewayList": 1,
		"*v1alpha1.RunnerGroupList":    1,
		"*v2alpha1.RunnerSetList":      1,
	}
	self := scaleSetRS("self", "tenant", "gw", "linux")
	gw := v2Gateway("tenant", "gw", "https://github.com/acme", "")

	t.Run("create", func(t *testing.T) {
		r := &countingReader{Reader: fakeReader(t, gw), lists: map[string]int{}}
		v := &RunnerSetCustomValidator{reader: r}
		_, err := v.ValidateCreate(ctx, self)
		require.NoError(t, err)
		assert.Equal(t, want, r.lists)
	})

	t.Run("update", func(t *testing.T) {
		r := &countingReader{Reader: fakeReader(t, gw, self), lists: map[string]int{}}
		v := &RunnerSetCustomValidator{reader: r}
		_, err := v.ValidateUpdate(ctx, self, scaleSetRS("self", "tenant", "gw", "macos"))
		require.NoError(t, err)
		assert.Equal(t, want, r.lists)
	})
}
