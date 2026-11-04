package main

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

	// Fake-GitHub stub selection (scaleSetStubBaseURL). Testing only.
	StubAuthURL   string // STUB_AUTH_URL
	StubBrokerURL string // STUB_BROKER_URL
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
	}
}

// scaleSetStubBaseURL returns the fake-GitHub base the scale-set tier should
// bootstrap against, or "" for production (derive it from the gateway's githubURL).
//
// Both STUB_AUTH_URL and STUB_BROKER_URL must be set; unsetting either returns the
// AGC to real GitHub. The stub needs its own signal because the scale-set endpoints
// are derived from spec.gitHubURL, which the CRD pattern and the webhook pin to https
// and so cannot name the plaintext stub.
func scaleSetStubBaseURL(cfg agcConfig) string {
	if cfg.StubAuthURL != "" && cfg.StubBrokerURL != "" {
		return cfg.StubBrokerURL
	}
	return ""
}
