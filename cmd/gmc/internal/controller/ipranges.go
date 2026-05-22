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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GitHubIPRangeFetcher fetches the current GitHub IP ranges.
// The default implementation calls https://api.github.com/meta.
// Tests inject a stub that returns a fixed set of CIDRs.
type GitHubIPRangeFetcher interface {
	FetchIPRanges(ctx context.Context) ([]net.IPNet, error)
}

// HTTPGitHubIPRangeFetcher is the production fetcher that calls api.github.com/meta.
type HTTPGitHubIPRangeFetcher struct {
	Client *http.Client
	APIURL string // override in tests; default "https://api.github.com"
}

type githubMetaResponse struct {
	Actions []string `json:"actions"`
}

// FetchIPRanges fetches GitHub Actions IP ranges from the GitHub meta API.
func (f *HTTPGitHubIPRangeFetcher) FetchIPRanges(ctx context.Context) ([]net.IPNet, error) {
	apiURL := f.APIURL
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	hc := f.Client
	if hc == nil {
		hc = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/meta", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch GitHub meta: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub meta returned %d", resp.StatusCode)
	}

	var meta githubMetaResponse
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	cidrs := make([]net.IPNet, 0, len(meta.Actions))
	for _, s := range meta.Actions {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			continue
		}
		cidrs = append(cidrs, *cidr)
	}
	return cidrs, nil
}

// IPRangeReconciler is a controller-runtime Runnable that periodically
// refreshes NetworkPolicy egress rules for all managed ActionsGateway CRs.
type IPRangeReconciler struct {
	client.Client
	Fetcher  GitHubIPRangeFetcher
	Interval time.Duration
	Log      *slog.Logger
}

// Start implements manager.Runnable. It runs until ctx is cancelled.
func (r *IPRangeReconciler) Start(ctx context.Context) error {
	interval := r.Interval
	if interval == 0 {
		interval = 24 * time.Hour
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}

	// Run once immediately on start, then on each tick.
	r.reconcileAll(ctx, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.reconcileAll(ctx, log)
		}
	}
}

// ReconcileNow triggers an immediate reconciliation of all ActionsGateway CRs.
// Intended for use in integration tests.
func (r *IPRangeReconciler) ReconcileNow(ctx context.Context) {
	r.reconcileAll(ctx, slog.Default())
}

func (r *IPRangeReconciler) reconcileAll(ctx context.Context, log *slog.Logger) {
	cidrs, err := r.Fetcher.FetchIPRanges(ctx)
	if err != nil {
		log.Error("failed to fetch GitHub IP ranges", "error", err)
		return
	}

	var agList gmcv1alpha1.ActionsGatewayList
	if err := r.List(ctx, &agList); err != nil {
		log.Error("failed to list ActionsGateways", "error", err)
		return
	}

	for i := range agList.Items {
		ag := &agList.Items[i]
		if ag.Spec.Proxy.ManagedNetworkPolicy != nil && !*ag.Spec.Proxy.ManagedNetworkPolicy {
			continue
		}
		if err := r.patchNetworkPolicy(ctx, ag, cidrs); err != nil {
			log.Error("failed to patch NetworkPolicy", "namespace", ag.Namespace, "name", ag.Name, "error", err)
		}
	}
}

func (r *IPRangeReconciler) patchNetworkPolicy(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, cidrs []net.IPNet) error {
	var np networkingv1.NetworkPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: "actions-gateway"}, &np); err != nil {
		return fmt.Errorf("get NetworkPolicy: %w", err)
	}

	desired := buildNetworkPolicy(ag, "", cidrs)
	np.Spec.Egress = desired.Spec.Egress
	np.Spec.Ingress = desired.Spec.Ingress

	return r.Update(ctx, &np)
}
