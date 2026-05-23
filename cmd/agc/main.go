// Command agc is the Actions Gateway Controller (AGC).
// It reconciles RunnerGroup CRDs into adaptive listener goroutine pools that
// long-poll the GitHub Actions broker for incoming workflow jobs.
//
// GitHub App credentials are read from files under /etc/actions-gateway/github-app/
// (projected from a Kubernetes Secret by the GMC). Keys:
//
//	appId          - GitHub App numeric ID
//	installationId - Installation ID for the target org/repo
//	privateKey     - RSA private key in PEM format
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/cache"
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

const credsDir = "/etc/actions-gateway/github-app"

func run() error {
	// ── 1. Read credentials from mounted Secret files ────────────────────────
	appIDBytes, err := os.ReadFile(filepath.Join(credsDir, "appId"))
	if err != nil {
		return fmt.Errorf("read appId: %w", err)
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(string(appIDBytes)), 10, 64)
	if err != nil {
		return fmt.Errorf("parse appId: %w", err)
	}

	installIDBytes, err := os.ReadFile(filepath.Join(credsDir, "installationId"))
	if err != nil {
		return fmt.Errorf("read installationId: %w", err)
	}
	installID, err := strconv.ParseInt(strings.TrimSpace(string(installIDBytes)), 10, 64)
	if err != nil {
		return fmt.Errorf("parse installationId: %w", err)
	}

	pemBytes, err := os.ReadFile(filepath.Join(credsDir, "privateKey"))
	if err != nil {
		return fmt.Errorf("read privateKey: %w", err)
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
	// Restrict the cache to POD_NAMESPACE so the AGC only watches resources in
	// its own tenant namespace. A cluster-scoped cache would require a
	// ClusterRole; a namespace-scoped cache works with the Role+RoleBinding
	// that GMC creates per tenant.
	namespace := os.Getenv("POD_NAMESPACE")
	cacheOpts := cache.Options{}
	if namespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{namespace: {}}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache:  cacheOpts,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	// ── 6. Start token manager ───────────────────────────────────────────────
	ctx := ctrl.SetupSignalHandler()
	tokenMgr.Namespace = namespace
	tokenMgr.Metrics = m
	tokenMgr.Start(ctx)

	// Wait for the first token before starting the reconciler.
	// Use a short-lived context so a GitHub outage at startup fails fast
	// rather than blocking indefinitely.
	tokenCtx, tokenCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer tokenCancel()
	if _, err := tokenMgr.Token(tokenCtx); err != nil {
		return fmt.Errorf("initial token fetch: %w", err)
	}

	// ── 7. Register reconciler ───────────────────────────────────────────────
	httpClient := &http.Client{Timeout: 60 * time.Second}
	prov := provisioner.NewProvisioner(mgr.GetClient(), m, nil)
	prov.WorkerSA = os.Getenv("WORKER_SERVICE_ACCOUNT")
	prov.HTTPProxy = os.Getenv("HTTP_PROXY")
	prov.HTTPSProxy = os.Getenv("HTTPS_PROXY")
	prov.NoProxy = os.Getenv("NO_PROXY")
	prov.HTTPClient = httpClient
	if img := os.Getenv("WORKER_IMAGE"); img != "" {
		prov.DefaultWorkerImage = img
	}
	prov.TokenFunc = tokenMgr.Token

	// Choose registrar:
	//   GITHUB_ORG_URL set                  → GithubRegistrar (production)
	//   STUB_AUTH_URL / STUB_BROKER_URL set  → StubRegistrar with those URLs (testing)
	//   neither                              → error: GITHUB_ORG_URL is required
	var registrar agentpool.Registrar
	if orgURL := os.Getenv("GITHUB_ORG_URL"); orgURL != "" {
		groupID := 1
		if raw := os.Getenv("GITHUB_RUNNER_GROUP_ID"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				groupID = parsed
			}
		}
		registrar = &agentpool.GithubRegistrar{
			OrgURL:  orgURL,
			GroupID: groupID,
		}
	} else {
		stubAuthURL := os.Getenv("STUB_AUTH_URL")
		stubBrokerURL := os.Getenv("STUB_BROKER_URL")
		if stubAuthURL == "" || stubBrokerURL == "" {
			return fmt.Errorf("GITHUB_ORG_URL is required (for testing set both STUB_AUTH_URL and STUB_BROKER_URL)")
		}
		registrar = agentpool.NewStubRegistrarWithURLs(stubAuthURL, stubBrokerURL)
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
			UseV2Flow:     os.Getenv("GITHUB_USE_VSTS_FLOW") != "true",
		},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup reconciler: %w", err)
	}

	ctrl.Log.Info("starting AGC manager")
	return mgr.Start(ctx)
}

