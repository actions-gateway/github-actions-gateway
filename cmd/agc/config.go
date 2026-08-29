package main

import (
	"fmt"
	"strconv"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/broker"
)

// agcConfig is the environment-variable surface run() consumes, snapshotted in
// one place so the config the GMC threads onto the AGC Deployment is visible
// and unit-testable rather than strewn through run() as scattered os.Getenv
// calls (Q367). It holds raw values only; parsing and validation that can error
// or has side effects stays at the point of use to preserve error ordering.
// The credential env vars (CREDENTIAL_TYPE, GITHUB_APP_*, VAULT_*) and OTEL_*
// are read by credentials.go and tracing.go respectively, not through here.
type agcConfig struct {
	// LogLevel (LOG_LEVEL, info|debug) is the per-tenant verbosity knob the GMC
	// threads from ActionsGateway.spec.logLevel.
	LogLevel string
	// AuditLogging (AGC_AUDIT_LOGGING, Off|WorkerAddresses) selects the
	// worker-address audit record the GMC threads from
	// ActionsGateway.spec.auditLogging. Off with the variable absent (Q986).
	AuditLogging string
	// PodNamespace (POD_NAMESPACE) scopes the controller cache to the AGC's own
	// tenant namespace.
	PodNamespace string
	// GatewayName (GATEWAY_NAME) scopes the RunnerSet informer to one gateway's
	// sets via a server-side field selector (§H.16 #1); empty leaves it unscoped.
	GatewayName string

	// Provisioner wiring (see provisioner.Provisioner). The GMC sets these on the
	// AGC Deployment; empty is the safe default for each.
	WorkerServiceAccount string // WORKER_SERVICE_ACCOUNT
	HTTPProxy            string // HTTP_PROXY
	HTTPSProxy           string // HTTPS_PROXY
	NoProxy              string // NO_PROXY
	ProxyTLSSecretName   string // PROXY_TLS_SECRET_NAME
	GitHubCAConfigMap    string // GITHUB_CA_CONFIGMAP_NAME
	SecurityProfile      string // SECURITY_PROFILE
	WorkerImage          string // WORKER_IMAGE (empty keeps the provisioner default)
	WrapperImage         string // WRAPPER_IMAGE (empty disables wrapper injection, Q235)
	WrapperDelivery      string // WRAPPER_DELIVERY (imagevolume|init; empty auto-detects)

	// WorkerUsageSampleInterval (WORKER_USAGE_SAMPLE_INTERVAL) tunes the worker
	// right-sizing sampler (Q359); parsed by workerUsageSampleInterval in run().
	WorkerUsageSampleInterval string

	// QuotaAdmission (AGC_QUOTA_ADMISSION, "false" opts out) controls the admission
	// gate's namespace-ResourceQuota rung (#784). On by default.
	QuotaAdmission string

	// Registrar selection (buildRegistrar). Stub URLs win over GITHUB_ORG_URL so an
	// explicitly-configured fakegithub stub takes precedence (testing).
	StubAuthURL         string // STUB_AUTH_URL
	StubBrokerURL       string // STUB_BROKER_URL
	GitHubOrgURL        string // GITHUB_ORG_URL
	GitHubRunnerGroupID string // GITHUB_RUNNER_GROUP_ID

	// Broker configuration (buildBrokerConfig).
	GitHubBrokerURL     string // GITHUB_BROKER_URL
	GitHubRunnerVersion string // GITHUB_RUNNER_VERSION
	GitHubRunnerOS      string // GITHUB_RUNNER_OS
	GitHubRunnerArch    string // GITHUB_RUNNER_ARCH
	GitHubUseVSTSFlow   string // GITHUB_USE_VSTS_FLOW ("true" selects the legacy VSTS flow)
	FanoutCompletion    string // AGC_FANOUT_COMPLETION ("false" opts out, Q260 Option A)
}

// loadConfig snapshots the AGC's full environment-variable surface via getenv
// (os.Getenv in production; a fake map in tests). It performs no parsing,
// validation, or side effects — see agcConfig for why a single up-front snapshot
// is behavior-identical to the scattered reads it replaces.
func loadConfig(getenv func(string) string) agcConfig {
	return agcConfig{
		LogLevel:                  getenv("LOG_LEVEL"),
		AuditLogging:              getenv("AGC_AUDIT_LOGGING"),
		PodNamespace:              getenv("POD_NAMESPACE"),
		GatewayName:               getenv("GATEWAY_NAME"),
		WorkerServiceAccount:      getenv("WORKER_SERVICE_ACCOUNT"),
		HTTPProxy:                 getenv("HTTP_PROXY"),
		HTTPSProxy:                getenv("HTTPS_PROXY"),
		NoProxy:                   getenv("NO_PROXY"),
		ProxyTLSSecretName:        getenv("PROXY_TLS_SECRET_NAME"),
		GitHubCAConfigMap:         getenv(githubCAConfigMapEnv),
		SecurityProfile:           getenv("SECURITY_PROFILE"),
		WorkerImage:               getenv("WORKER_IMAGE"),
		WrapperImage:              getenv("WRAPPER_IMAGE"),
		WrapperDelivery:           getenv("WRAPPER_DELIVERY"),
		WorkerUsageSampleInterval: getenv("WORKER_USAGE_SAMPLE_INTERVAL"),
		QuotaAdmission:            getenv("AGC_QUOTA_ADMISSION"),
		StubAuthURL:               getenv("STUB_AUTH_URL"),
		StubBrokerURL:             getenv("STUB_BROKER_URL"),
		GitHubOrgURL:              getenv("GITHUB_ORG_URL"),
		GitHubRunnerGroupID:       getenv("GITHUB_RUNNER_GROUP_ID"),
		GitHubBrokerURL:           getenv("GITHUB_BROKER_URL"),
		GitHubRunnerVersion:       getenv("GITHUB_RUNNER_VERSION"),
		GitHubRunnerOS:            getenv("GITHUB_RUNNER_OS"),
		GitHubRunnerArch:          getenv("GITHUB_RUNNER_ARCH"),
		GitHubUseVSTSFlow:         getenv("GITHUB_USE_VSTS_FLOW"),
		FanoutCompletion:          getenv("AGC_FANOUT_COMPLETION"),
	}
}

// buildRegistrar selects the agent registrar from config. An
// explicitly-configured fakegithub stub (both STUB_AUTH_URL and STUB_BROKER_URL
// set) wins even though a GMC-provisioned AGC always carries GITHUB_ORG_URL, so
// a stub-backed AGC stays on the stub until the stub env is unset. A
// GITHUB_ORG_URL install uses the GithubRegistrar; a malformed or non-positive
// GITHUB_RUNNER_GROUP_ID falls back to group 1 without error. Neither
// configured is a hard error.
func buildRegistrar(cfg agcConfig) (agentpool.Registrar, error) {
	switch {
	case cfg.StubAuthURL != "" && cfg.StubBrokerURL != "":
		return agentpool.NewStubRegistrarWithURLs(cfg.StubAuthURL, cfg.StubBrokerURL), nil
	case cfg.GitHubOrgURL != "":
		groupID := 1
		if raw := cfg.GitHubRunnerGroupID; raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				groupID = parsed
			}
		}
		return &agentpool.GithubRegistrar{
			OrgURL:  cfg.GitHubOrgURL,
			GroupID: groupID,
		}, nil
	default:
		return nil, fmt.Errorf("GITHUB_ORG_URL is required (for testing set both STUB_AUTH_URL and STUB_BROKER_URL)")
	}
}

// scaleSetStubBaseURL returns the fake-GitHub base the scale-set tier should
// bootstrap against, or "" for production (derive it from the gateway's githubURL).
//
// It keys off exactly the pair buildRegistrar's stub case does, so the two
// acquisition tiers cannot end up pointed at different backends: a stub-backed AGC
// is stub-backed on both, and unsetting the pair returns both to real GitHub. The
// scale-set tier needs its own signal because its endpoints are derived from
// spec.gitHubURL, which the CRD pattern and the webhook pin to https and so cannot
// name the plaintext stub.
func scaleSetStubBaseURL(cfg agcConfig) string {
	if cfg.StubAuthURL != "" && cfg.StubBrokerURL != "" {
		return cfg.StubBrokerURL
	}
	return ""
}

// buildBrokerConfig maps the broker-related env into controller.BrokerConfig. The
// long-poll HTTPClient (broker.NewHTTPClient) is the sanctioned no-read-timeout
// exception (Q108); UseV2Flow is on unless GITHUB_USE_VSTS_FLOW=="true", and
// FanoutCompletion is on unless AGC_FANOUT_COMPLETION=="false" (Q260 Option A).
func buildBrokerConfig(cfg agcConfig) controller.BrokerConfig {
	return controller.BrokerConfig{
		BrokerURL:     cfg.GitHubBrokerURL,
		RunnerVersion: cfg.GitHubRunnerVersion,
		RunnerOS:      cfg.GitHubRunnerOS,
		RunnerArch:    cfg.GitHubRunnerArch,
		UseV2Flow:     cfg.GitHubUseVSTSFlow != "true",
		// Tuned client bounds the GetMessage long-poll: a black-holed broker
		// connection is torn down a few seconds past the 50s hold rather than
		// blocking a listener for the multi-minute OS TCP timeout (Q108).
		HTTPClient: broker.NewHTTPClient(),
		// Q260 Option A: when a job is fanned out to sibling sessions, the winner
		// fans completejob out to every deduped sibling delivery on completion so
		// GitHub does not cancel the whole job at its ~15-minute unstarted-job
		// timeout. ON by default — the re-route #5 dogfood experiment (2026-07-04)
		// confirmed the run service's completion is per-delivery, not planID-scoped.
		// Opt out with AGC_FANOUT_COMPLETION=false. Operator runbook: the Q260
		// redelivery-accounting section in docs/operations/troubleshooting.md.
		FanoutCompletion: cfg.FanoutCompletion != "false",
	}
}
