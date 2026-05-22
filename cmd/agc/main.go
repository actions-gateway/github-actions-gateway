// Command agc is the Actions Gateway Controller (AGC).
// It reconciles RunnerGroup CRDs into adaptive listener goroutine pools that
// long-poll the GitHub Actions broker for incoming workflow jobs.
//
// Required environment variables:
//
//	GITHUB_APP_ID              - GitHub App numeric ID
//	GITHUB_APP_PRIVATE_KEY     - Path to PEM file, or PEM literal
//	GITHUB_APP_INSTALLATION_ID - Installation ID for the target org/repo
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	"github.com/karlkfi/github-actions-gateway/agc/internal/agentpool"
	"github.com/karlkfi/github-actions-gateway/agc/internal/controller"
	"github.com/karlkfi/github-actions-gateway/agc/internal/listener"
	"github.com/karlkfi/github-actions-gateway/agc/internal/provisioner"
	"github.com/karlkfi/github-actions-gateway/agc/internal/token"
	"github.com/karlkfi/github-actions-gateway/githubapp"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("agc")

	if err := run(); err != nil {
		log.Error(err, "startup failed")
		os.Exit(1)
	}
}

func run() error {
	// ── 1. Read credentials ──────────────────────────────────────────────────
	appID, err := strconv.ParseInt(mustEnv("GITHUB_APP_ID"), 10, 64)
	if err != nil {
		return fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installID, err := strconv.ParseInt(mustEnv("GITHUB_APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		return fmt.Errorf("parse GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	pemBytes, err := loadPEM(mustEnv("GITHUB_APP_PRIVATE_KEY"))
	if err != nil {
		return fmt.Errorf("load GITHUB_APP_PRIVATE_KEY: %w", err)
	}

	// ── 2. Build token provider and manager ─────────────────────────────────
	creds := githubapp.Credentials{
		AppID:          appID,
		PrivateKeyPEM:  pemBytes,
		InstallationID: installID,
	}
	rawProvider, err := githubapp.NewInstallationTokenProvider(creds, nil)
	if err != nil {
		return fmt.Errorf("create token provider: %w", err)
	}
	expProvider, ok := rawProvider.(githubapp.ExpiringTokenProvider)
	if !ok {
		return fmt.Errorf("token provider does not implement ExpiringTokenProvider")
	}
	tokenMgr := token.NewManager(expProvider, nil)

	// ── 3. Build Prometheus metrics ──────────────────────────────────────────
	m := listener.NewMetrics()

	// ── 4. Build scheme ──────────────────────────────────────────────────────
	scheme := runtime.NewScheme()
	// clientgoscheme already includes corev1; no need to add it separately.
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	// ── 5. Start the controller-runtime manager ──────────────────────────────
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	// ── 6. Start token manager ───────────────────────────────────────────────
	ctx := ctrl.SetupSignalHandler()
	namespace := os.Getenv("POD_NAMESPACE")
	tokenMgr.Namespace = namespace
	tokenMgr.Metrics = m
	tokenMgr.Start(ctx)

	// Wait for the first token before starting the reconciler.
	if _, err := tokenMgr.Token(context.Background()); err != nil {
		return fmt.Errorf("initial token fetch: %w", err)
	}

	// ── 7. Register reconciler ───────────────────────────────────────────────
	prov := provisioner.NewProvisioner(mgr.GetClient(), m, nil)
	prov.WorkerSA = os.Getenv("WORKER_SERVICE_ACCOUNT")
	prov.HTTPProxy = os.Getenv("HTTP_PROXY")
	prov.HTTPSProxy = os.Getenv("HTTPS_PROXY")
	prov.NoProxy = os.Getenv("NO_PROXY")
	if img := os.Getenv("WORKER_IMAGE"); img != "" {
		prov.DefaultWorkerImage = img
	}
	prov.TokenFunc = tokenMgr.Token

	// STUB_AUTH_URL / STUB_BROKER_URL redirect the StubRegistrar to a local
	// fake server. These are no-ops when unset (defaults remain).
	stubAuthURL := os.Getenv("STUB_AUTH_URL")
	stubBrokerURL := os.Getenv("STUB_BROKER_URL")
	var registrar agentpool.Registrar
	if stubAuthURL != "" || stubBrokerURL != "" {
		if stubAuthURL == "" {
			stubAuthURL = "https://stub.example.com/token"
		}
		if stubBrokerURL == "" {
			stubBrokerURL = "https://stub.example.com/broker"
		}
		registrar = agentpool.NewStubRegistrarWithURLs(stubAuthURL, stubBrokerURL)
	} else {
		registrar = agentpool.NewStubRegistrar()
	}

	r := &controller.RunnerGroupReconciler{
		Client:       mgr.GetClient(),
		TokenManager: tokenMgr,
		Registrar:    registrar,
		Metrics:      m,
		Provisioner:  prov,
		BrokerConfig: controller.BrokerConfig{
			BrokerURL:     os.Getenv("GITHUB_BROKER_URL"),
			RunnerVersion: os.Getenv("GITHUB_RUNNER_VERSION"),
			RunnerOS:      os.Getenv("GITHUB_RUNNER_OS"),
			RunnerArch:    os.Getenv("GITHUB_RUNNER_ARCH"),
			UseV2Flow:     os.Getenv("GITHUB_USE_V2_FLOW") == "true",
		},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup reconciler: %w", err)
	}

	ctrl.Log.Info("starting AGC manager")
	return mgr.Start(ctx)
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "required environment variable %s is not set\n", name)
		os.Exit(1)
	}
	return v
}

func loadPEM(value string) ([]byte, error) {
	const pemHeader = "-----"
	if len(value) >= len(pemHeader) && value[:len(pemHeader)] == pemHeader {
		return []byte(value), nil
	}
	return os.ReadFile(value)
}
