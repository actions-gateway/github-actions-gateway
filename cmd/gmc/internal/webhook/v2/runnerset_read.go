package v2

import (
	"context"
	"fmt"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/agentidentity"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/scalesetscope"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runnerSetRead is the cluster-wide read the RunnerSet scale-set name guard (Q791) and
// agent-identity guard (Q1011) both derive their inventories from, taken at most once
// per admission request (Q1075). It reads through the validator's uncached reader;
// sharing it within one request trades away no freshness, since both guards would
// otherwise have read the same kinds moments apart. A read error is kept, so each
// guard fails closed with its own message.
type runnerSetRead struct {
	reader client.Reader

	done       bool
	err        error
	v1Gateways gmcv1alpha1.ActionsGatewayList
	v2Gateways agcv2alpha1.ActionsGatewayList
	groups     agcv1alpha1.RunnerGroupList
	sets       agcv2alpha1.RunnerSetList
}

func (r *runnerSetRead) load(ctx context.Context) error {
	if r.done {
		return r.err
	}
	r.done = true
	if err := r.reader.List(ctx, &r.v1Gateways); err != nil {
		r.err = fmt.Errorf("list v1alpha1 ActionsGateways: %w", err)
	} else if err := r.reader.List(ctx, &r.v2Gateways); err != nil {
		r.err = fmt.Errorf("list v2alpha1 ActionsGateways: %w", err)
	} else if err := r.reader.List(ctx, &r.groups); err != nil {
		r.err = fmt.Errorf("list RunnerGroups: %w", err)
	} else if err := r.reader.List(ctx, &r.sets); err != nil {
		r.err = fmt.Errorf("list RunnerSets: %w", err)
	}
	return r.err
}

func (r *runnerSetRead) scaleSets(ctx context.Context) (scalesetscope.Inventory, error) {
	if err := r.load(ctx); err != nil {
		return scalesetscope.Inventory{}, err
	}
	return scalesetscope.FromLists(r.v2Gateways.Items, r.sets.Items, nil), nil
}

func (r *runnerSetRead) agentIdentities(ctx context.Context) (agentidentity.Inventory, error) {
	if err := r.load(ctx); err != nil {
		return agentidentity.Inventory{}, err
	}
	return agentidentity.FromLists(r.v1Gateways.Items, r.v2Gateways.Items, r.groups.Items, r.sets.Items, nil), nil
}
