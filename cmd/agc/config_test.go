package main

import (
	"testing"

	v1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
)

// fakeGetenv returns a getenv func backed by a map, so config resolution is
// tested without mutating the process environment.
func fakeGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestLoadConfig verifies loadConfig snapshots every environment variable of the
// AGC config surface into the matching struct field. This is the single place
// F8/Q367 pulled the 24 scattered os.Getenv reads into; the test guards the
// mapping so a mis-wired field is caught here rather than in production wiring.
func TestLoadConfig(t *testing.T) {
	env := map[string]string{
		"LOG_LEVEL":                    "debug",
		"POD_NAMESPACE":                "tenant-a",
		"GATEWAY_NAME":                 "gw-1",
		"WORKER_SERVICE_ACCOUNT":       "worker-sa",
		"HTTP_PROXY":                   "http://proxy:8080",
		"HTTPS_PROXY":                  "http://proxy:8080",
		"NO_PROXY":                     "10.0.0.0/8",
		"PROXY_TLS_SECRET_NAME":        "proxy-ca",
		"SECURITY_PROFILE":             "restricted",
		"WORKER_IMAGE":                 "ghcr.io/org/runner:v1",
		"WRAPPER_IMAGE":                "ghcr.io/org/wrapper:v1",
		"WRAPPER_DELIVERY":             "imagevolume",
		"WORKER_USAGE_SAMPLE_INTERVAL": "30s",
		"STUB_AUTH_URL":                "http://stub/auth",
		"STUB_BROKER_URL":              "http://stub/broker",
		"GITHUB_ORG_URL":               "https://github.com/org",
		"GITHUB_RUNNER_GROUP_ID":       "7",
		"GITHUB_BROKER_URL":            "https://broker.github.com",
		"GITHUB_RUNNER_VERSION":        "2.320.0",
		"GITHUB_RUNNER_OS":             "linux",
		"GITHUB_RUNNER_ARCH":           "x64",
		"GITHUB_USE_VSTS_FLOW":         "true",
		"AGC_FANOUT_COMPLETION":        "false",
	}
	cfg := loadConfig(fakeGetenv(env))

	checks := map[string]struct{ got, want string }{
		"LogLevel":                  {cfg.LogLevel, "debug"},
		"PodNamespace":              {cfg.PodNamespace, "tenant-a"},
		"GatewayName":               {cfg.GatewayName, "gw-1"},
		"WorkerServiceAccount":      {cfg.WorkerServiceAccount, "worker-sa"},
		"HTTPProxy":                 {cfg.HTTPProxy, "http://proxy:8080"},
		"HTTPSProxy":                {cfg.HTTPSProxy, "http://proxy:8080"},
		"NoProxy":                   {cfg.NoProxy, "10.0.0.0/8"},
		"ProxyTLSSecretName":        {cfg.ProxyTLSSecretName, "proxy-ca"},
		"SecurityProfile":           {cfg.SecurityProfile, "restricted"},
		"WorkerImage":               {cfg.WorkerImage, "ghcr.io/org/runner:v1"},
		"WrapperImage":              {cfg.WrapperImage, "ghcr.io/org/wrapper:v1"},
		"WrapperDelivery":           {cfg.WrapperDelivery, "imagevolume"},
		"WorkerUsageSampleInterval": {cfg.WorkerUsageSampleInterval, "30s"},
		"StubAuthURL":               {cfg.StubAuthURL, "http://stub/auth"},
		"StubBrokerURL":             {cfg.StubBrokerURL, "http://stub/broker"},
		"GitHubOrgURL":              {cfg.GitHubOrgURL, "https://github.com/org"},
		"GitHubRunnerGroupID":       {cfg.GitHubRunnerGroupID, "7"},
		"GitHubBrokerURL":           {cfg.GitHubBrokerURL, "https://broker.github.com"},
		"GitHubRunnerVersion":       {cfg.GitHubRunnerVersion, "2.320.0"},
		"GitHubRunnerOS":            {cfg.GitHubRunnerOS, "linux"},
		"GitHubRunnerArch":          {cfg.GitHubRunnerArch, "x64"},
		"GitHubUseVSTSFlow":         {cfg.GitHubUseVSTSFlow, "true"},
		"FanoutCompletion":          {cfg.FanoutCompletion, "false"},
	}
	for field, c := range checks {
		if c.got != c.want {
			t.Errorf("loadConfig().%s = %q, want %q", field, c.got, c.want)
		}
	}

	// An empty environment yields the zero value for every field (no defaults are
	// applied at snapshot time; defaulting happens downstream).
	empty := loadConfig(fakeGetenv(nil))
	if (empty != agcConfig{}) {
		t.Errorf("loadConfig(empty) = %+v, want zero-valued agcConfig", empty)
	}
}

// TestBuildRegistrar covers the registrar selection precedence: stub-first, then
// GitHub, then the GITHUB_ORG_URL-required error, plus the runner-group-ID
// fallback rules.
func TestBuildRegistrar(t *testing.T) {
	t.Run("stub URLs win even with GITHUB_ORG_URL set", func(t *testing.T) {
		reg, err := buildRegistrar(agcConfig{
			StubAuthURL:   "http://stub/auth",
			StubBrokerURL: "http://stub/broker",
			GitHubOrgURL:  "https://github.com/org",
		})
		if err != nil {
			t.Fatalf("buildRegistrar returned error: %v", err)
		}
		if _, ok := reg.(*agentpool.StubRegistrar); !ok {
			t.Errorf("expected *agentpool.StubRegistrar, got %T", reg)
		}
	})

	t.Run("only one stub URL falls through to GitHub", func(t *testing.T) {
		reg, err := buildRegistrar(agcConfig{
			StubAuthURL:  "http://stub/auth", // STUB_BROKER_URL missing
			GitHubOrgURL: "https://github.com/org",
		})
		if err != nil {
			t.Fatalf("buildRegistrar returned error: %v", err)
		}
		gh, ok := reg.(*agentpool.GithubRegistrar)
		if !ok {
			t.Fatalf("expected *agentpool.GithubRegistrar, got %T", reg)
		}
		if gh.GroupID != 1 {
			t.Errorf("default GroupID = %d, want 1", gh.GroupID)
		}
	})

	t.Run("GitHub with explicit group ID", func(t *testing.T) {
		reg, err := buildRegistrar(agcConfig{
			GitHubOrgURL:        "https://github.com/org",
			GitHubRunnerGroupID: "42",
		})
		if err != nil {
			t.Fatalf("buildRegistrar returned error: %v", err)
		}
		gh := reg.(*agentpool.GithubRegistrar)
		if gh.GroupID != 42 {
			t.Errorf("GroupID = %d, want 42", gh.GroupID)
		}
		if gh.OrgURL != "https://github.com/org" {
			t.Errorf("OrgURL = %q, want the configured org URL", gh.OrgURL)
		}
	})

	for _, raw := range []string{"0", "-3", "not-an-int", ""} {
		t.Run("invalid/non-positive group ID "+raw+" falls back to 1", func(t *testing.T) {
			reg, err := buildRegistrar(agcConfig{
				GitHubOrgURL:        "https://github.com/org",
				GitHubRunnerGroupID: raw,
			})
			if err != nil {
				t.Fatalf("buildRegistrar returned error: %v", err)
			}
			if gh := reg.(*agentpool.GithubRegistrar); gh.GroupID != 1 {
				t.Errorf("GroupID = %d, want 1 (fallback)", gh.GroupID)
			}
		})
	}

	t.Run("neither configured is an error", func(t *testing.T) {
		if _, err := buildRegistrar(agcConfig{}); err == nil {
			t.Error("buildRegistrar with no registrar config must return an error")
		}
	})
}

// TestBuildBrokerConfig verifies the env → BrokerConfig mapping and the two
// default-on booleans that flip only on an exact opt-out string.
func TestBuildBrokerConfig(t *testing.T) {
	t.Run("defaults: v2 flow and fan-out on", func(t *testing.T) {
		bc := buildBrokerConfig(agcConfig{})
		if !bc.UseV2Flow {
			t.Error("UseV2Flow must default to true (only GITHUB_USE_VSTS_FLOW=true disables it)")
		}
		if !bc.FanoutCompletion {
			t.Error("FanoutCompletion must default to true (only AGC_FANOUT_COMPLETION=false disables it)")
		}
		if bc.HTTPClient == nil {
			t.Error("HTTPClient must be set to the broker long-poll client")
		}
	})

	t.Run("opt-outs flip the booleans", func(t *testing.T) {
		bc := buildBrokerConfig(agcConfig{
			GitHubUseVSTSFlow: "true",
			FanoutCompletion:  "false",
		})
		if bc.UseV2Flow {
			t.Error("GITHUB_USE_VSTS_FLOW=true must set UseV2Flow=false")
		}
		if bc.FanoutCompletion {
			t.Error("AGC_FANOUT_COMPLETION=false must set FanoutCompletion=false")
		}
	})

	t.Run("passthrough fields", func(t *testing.T) {
		bc := buildBrokerConfig(agcConfig{
			GitHubBrokerURL:     "https://broker",
			GitHubRunnerVersion: "2.320.0",
			GitHubRunnerOS:      "linux",
			GitHubRunnerArch:    "x64",
		})
		if bc.BrokerURL != "https://broker" || bc.RunnerVersion != "2.320.0" ||
			bc.RunnerOS != "linux" || bc.RunnerArch != "x64" {
			t.Errorf("broker passthrough mismatch: %+v", bc)
		}
	})
}

// TestBuildScheme verifies the AGC client scheme registers the core, v1alpha1,
// and v2alpha1 kinds it wires reconcilers for.
func TestBuildScheme(t *testing.T) {
	scheme, err := buildScheme()
	if err != nil {
		t.Fatalf("buildScheme returned error: %v", err)
	}
	// A representative kind from each registered group must resolve.
	if !scheme.Recognizes(corev1.SchemeGroupVersion.WithKind("Pod")) {
		t.Error("scheme must recognize core/v1 Pod")
	}
	if !scheme.Recognizes(v1alpha1.GroupVersion.WithKind("RunnerGroup")) {
		t.Error("scheme must recognize agc v1alpha1 RunnerGroup")
	}
	if !scheme.Recognizes(agcv2alpha1.GroupVersion.WithKind("RunnerSet")) {
		t.Error("scheme must recognize actions-gateway.com/v2alpha1 RunnerSet")
	}
}
