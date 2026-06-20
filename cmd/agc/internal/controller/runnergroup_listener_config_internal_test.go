package controller

import (
	"net/http"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func listenerConfigTestRunnerGroup() *v1alpha1.RunnerGroup {
	return &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
	}
}

// TestNewListenerConfig_ReusesConfiguredHTTPClient asserts the broker's
// already-configured HTTP client is threaded into the listener Config so the
// per-session OAuth token fetch reuses its connection pool / keep-alives instead
// of building a fresh client (and a fresh connection) per fetch — Q153.
func TestNewListenerConfig_ReusesConfiguredHTTPClient(t *testing.T) {
	shared := &http.Client{}
	r := &RunnerGroupReconciler{}
	brokerCfg := BrokerConfig{HTTPClient: shared}
	pool := agentpool.NewPool(nil, "ns", "g", "", nil, agentpool.NewStubRegistrar(), "")
	agent := &agentpool.Agent{Index: 0}

	cfg := r.newListenerConfig(listenerConfigTestRunnerGroup(), pool, brokerCfg, nil, agent)

	// Same pointer — the OAuth fetch path receives the shared client, not a copy
	// or a freshly constructed one.
	require.Same(t, shared, cfg.HTTPClient,
		"OAuth token fetch must reuse the configured broker HTTP client")
	require.Same(t, shared, cfg.Broker.HTTPClient,
		"broker client must reuse the configured HTTP client")
}

// TestNewListenerConfig_NilHTTPClientPreservesFallback asserts that when the
// BrokerConfig leaves HTTPClient unset (e.g. tests), the listener Config also
// leaves it nil so FetchRunnerOAuthToken falls back to its bounded
// httpx.NewClient() — the genuinely-unconfigured case is preserved.
func TestNewListenerConfig_NilHTTPClientPreservesFallback(t *testing.T) {
	r := &RunnerGroupReconciler{}
	pool := agentpool.NewPool(nil, "ns", "g", "", nil, agentpool.NewStubRegistrar(), "")
	agent := &agentpool.Agent{Index: 0}

	cfg := r.newListenerConfig(listenerConfigTestRunnerGroup(), pool, BrokerConfig{}, nil, agent)

	require.Nil(t, cfg.HTTPClient,
		"unset broker HTTP client must stay nil so the OAuth fetch uses its bounded fallback")
}
