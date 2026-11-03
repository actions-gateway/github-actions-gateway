package main

import (
	"testing"

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

// TestScaleSetStubBaseURL pins the stub selection to both stub variables. The
// negative cases are the load-bearing ones: a production AGC carries neither stub
// var, and a half-configured AGC must fall through to real GitHub.
func TestScaleSetStubBaseURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  agcConfig
		want string
	}{
		{
			name: "both stub URLs select the stub",
			cfg: agcConfig{
				StubAuthURL:   "http://stub/token",
				StubBrokerURL: "http://stub",
			},
			want: "http://stub",
		},
		{
			name: "only the auth URL falls through to GitHub",
			cfg:  agcConfig{StubAuthURL: "http://stub/token"},
			want: "",
		},
		{
			name: "only the broker URL falls through to GitHub",
			cfg:  agcConfig{StubBrokerURL: "http://stub"},
			want: "",
		},
		{
			name: "production",
			cfg:  agcConfig{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scaleSetStubBaseURL(tc.cfg); got != tc.want {
				t.Errorf("scaleSetStubBaseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildScheme verifies the AGC client scheme registers the core and v2alpha1
// kinds it wires reconcilers for.
func TestBuildScheme(t *testing.T) {
	scheme, err := buildScheme()
	if err != nil {
		t.Fatalf("buildScheme returned error: %v", err)
	}
	// A representative kind from each registered group must resolve.
	if !scheme.Recognizes(corev1.SchemeGroupVersion.WithKind("Pod")) {
		t.Error("scheme must recognize core/v1 Pod")
	}
	if !scheme.Recognizes(agcv2alpha1.GroupVersion.WithKind("RunnerSet")) {
		t.Error("scheme must recognize actions-gateway.com/v2alpha1 RunnerSet")
	}
}
