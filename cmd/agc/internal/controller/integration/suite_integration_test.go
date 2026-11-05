//go:build integration

package integration_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/broker/brokertest"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	testEnv    *envtest.Environment
	k8sClient  client.Client
	testScheme *runtime.Scheme
	ctx        context.Context
	cancel     context.CancelFunc
	brokerStub *brokertest.Server
)

func TestMain(m *testing.M) {
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	brokerStub = brokertest.New()
	defer brokerStub.Close()

	testScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = agcv2.AddToScheme(testScheme)

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			// The five v2 (actions-gateway.com) CRDs live in the neutral api module,
			// reached through testdata/ so the read stays inside this module root and
			// go keeps it as a test-cache input (Q902).
			"testdata/crd",
		},
		ErrorIfCRDPathMissing: true,
		Scheme:                testScheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic(err)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		panic(err)
	}

	exitCode := m.Run()
	_ = testEnv.Stop()
	cancel()
	os.Exit(exitCode)
}

// stubProvider always returns a hardcoded installation token.
type stubProvider struct{}

func (stubProvider) Token(_ context.Context) (string, error) { return "inst-token", nil }
func (stubProvider) TokenWithExpiry(_ context.Context) (*githubapp.InstallationToken, error) {
	return &githubapp.InstallationToken{
		Token:     "inst-token",
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

// sharedListenerMetrics returns the process-wide runnercore.Metrics, constructed
// once. runnercore.NewMetrics registers with the controller-runtime metrics
// registry, so it must be created a single time per test binary; counters
// accumulate across tests, so assertions read before/after deltas.
var sharedListenerMetrics = sync.OnceValue(runnercore.NewMetrics)
