//go:build e2e
// +build e2e

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_Tracing_CollectorOverTLS is the Q1281 gate: a proxied tenant's spans reach an
// in-cluster collector over TLS, through the two fields spec.tracing grew for it, with no
// NetworkPolicy applied by hand and no CA installed anywhere but the named ConfigMap.
//
// The collector is the real upstream image. Its certificate chains to a CA generated here,
// so nothing the AGC's image ships can validate it, and the first arm proves that: with the
// route open and no bundle, the AGC reports an unknown authority and the collector records
// no span. Reading that error also proves the peer rule, since a TLS failure means the
// connection got through the AGC's default-deny policy.
//
// The negative half runs on an enforcing CNI only. kindnet does not drop egress, so there a
// worker pod reaching the collector would say nothing about the policy.
var _ = Describe("E2E_Tracing_CollectorOverTLS", Ordered, func() {
	const (
		tenantNS    = "tenant-otel-tls"
		collectorNS = "otel-tls-collector"
		gwName      = "oteltls"
		agcDeploy   = gwName + "-agc"
		setName     = "set-oteltls"
		label       = "e2e-oteltls"
		proxyName   = "egress"
		secretName  = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.

		collectorHost = "otel-collector." + collectorNS + ".svc.cluster.local"
		endpoint      = "https://" + collectorHost + ":4317"
		rootSpan      = "Provisioner.ProvisionScaleSetWorker"
		refused       = "certificate signed by unknown authority"
	)

	var (
		pf    *exec.Cmd
		caPEM string
	)

	logsOf := func(ns, deploy string) string {
		out, _ := exec.Command("kubectl", "logs", "--tail=-1", "-n", ns, "deploy/"+deploy).CombinedOutput()
		return string(out)
	}
	rootSpans := func() int {
		return strings.Count(logsOf(collectorNS, "otel-collector"), "Name           : "+rootSpan)
	}
	// liveAGCLogs reads by pod, not deploy/: an earlier arm's AGC can still be terminating,
	// and its log holds the certificate failures that arm expects.
	liveAGCLogs := func() string {
		GinkgoHelper()
		out, err := exec.Command("kubectl", "get", "pods", "-n", tenantNS, "-l", "app="+agcDeploy, "-o",
			`go-template={{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}} {{end}}{{end}}`).Output()
		Expect(err).NotTo(HaveOccurred())
		pods := strings.Fields(string(out))
		Expect(pods).NotTo(BeEmpty())
		var all strings.Builder
		for _, pod := range pods {
			logs, err := exec.Command("kubectl", "logs", "--tail=-1", "-n", tenantNS, pod, "-c", "agc").CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "read %s: %s", pod, logs)
			Expect(string(logs)).To(ContainSubstring("OpenTelemetry tracing enabled"), "the read must be able to see this AGC's log")
			all.Write(logs)
		}
		return all.String()
	}
	workerPods := func() int {
		out, _ := exec.Command("kubectl", "get", "pods", "-n", tenantNS, "-l", "actions-gateway.com/runner-set="+setName, "-o", "name").Output()
		return len(strings.Fields(string(out)))
	}
	collectorCAManifest := func() string {
		return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: collector-ca
  namespace: %s
data:
  ca.crt: |
%s`, tenantNS, indent(caPEM, "    "))
	}
	gatewayCondition := func(condition string) string {
		out, _ := exec.Command("kubectl", "get", "actionsgateways.actions-gateway.com", gwName, "-n", tenantNS, "-o",
			fmt.Sprintf(`jsonpath={.status.conditions[?(@.type=="%s")].status}`, condition)).Output()
		return string(out)
	}
	// queueJobsUntil re-queues because a job taken by a pod a rollout is terminating dies
	// with it, and the root span only exists once a worker is provisioned.
	queueJobsUntil := func(done func() bool, description string) {
		GinkgoHelper()
		var next time.Time
		Eventually(func() bool {
			if time.Now().After(next) {
				scaleSetEnqueue(label)
				next = time.Now().Add(30 * time.Second)
			}
			return done()
		}, 4*time.Minute, 2*time.Second).Should(BeTrue(), description)
	}

	BeforeAll(func() {
		var certPEM, keyPEM string
		caPEM, certPEM, keyPEM = collectorCertificates(collectorHost)

		utils.CreateNamespace(collectorNS, nil)
		Expect(utils.ApplyManifest(tlsCollectorManifest(collectorNS,
			envOrDefault("E2E_OTEL_COLLECTOR_IMAGE", "otel/opentelemetry-collector:0.120.0"), certPEM, keyPEM))).To(Succeed())
		utils.WaitForDeploymentReady(collectorNS, "otel-collector", 4*time.Minute)

		utils.CreateNamespace(tenantNS, map[string]string{"actions-gateway.com/tenant": "managed"})
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)
		Expect(utils.ApplyManifest(collectorCAManifest())).To(Succeed())

		By("applying a proxied gateway that names the collector peer but no CA bundle")
		Expect(utils.ApplyManifestWithWebhookRetry(fmt.Sprintf(`apiVersion: actions-gateway.com/v2
kind: EgressProxy
metadata:
  name: %s
  namespace: %s
spec:
  minReplicas: 1
  maxReplicas: 1
---
%s`, proxyName, tenantNS, strings.NewReplacer(
			"name: ssacq\n", "name: "+gwName+"\n",
			"name: set-ssacq", "name: "+setName,
			`["e2e-ssacq"]`, `["`+label+`"]`,
			"/ssacqorg", "/oteltlsorg",
			"logLevel: debug", fmt.Sprintf(`logLevel: debug
  defaultProxyRef:
    name: %s
  tracing:
    endpoint: %s
    sampler: always_on
    networkPolicy:
      namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: %s
      podSelector:
        matchLabels:
          app: otel-collector`, proxyName, endpoint, collectorNS),
		).Replace(scaleSetAcquisitionManifest(tenantNS, secretName, workerImage))))).To(Succeed())
		utils.WaitForDeploymentReady(tenantNS, agcDeploy, 5*time.Minute)

		pf = startFakegithubPortForward(20390)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(tenantNS, agcDeploy, infraNamespace, fakegithubServiceName)
			_, _ = fmt.Fprintf(GinkgoWriter, "collector logs:\n%s\n", logsOf(collectorNS, "otel-collector"))
			utils.DumpCNIEnforcerState()
		}
	})

	AfterAll(func() {
		if pf != nil && pf.Process != nil {
			_ = pf.Process.Kill()
		}
		// In dependency order, waiting on each: the RunnerSet's finalizer is cleared by the
		// AGC, so a bare namespace delete can wedge in Terminating.
		_, _ = utils.Run(exec.Command("kubectl", "delete", "runnerset", setName,
			"-n", tenantNS, "--ignore-not-found", "--timeout=2m"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "actionsgateways.actions-gateway.com", gwName,
			"-n", tenantNS, "--ignore-not-found", "--timeout=2m"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
			"agc-clusterrunnertemplate-reader."+tenantNS+"."+gwName, "--ignore-not-found"))
		utils.DeleteNamespace(tenantNS)
		utils.DeleteNamespace(collectorNS)
	})

	It("E2E_Tracing_PrivateCACollectorIsUntrustedWithoutTheBundle: the peer opens the route and the AGC refuses the certificate", func() {
		Expect(agcEnvValue(tenantNS, agcDeploy, "HTTPS_PROXY")).NotTo(BeEmpty(), "this spec's tenant must be proxied")
		Expect(agcEnvValue(tenantNS, agcDeploy, "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE")).To(BeEmpty())

		queueJobsUntil(func() bool {
			return strings.Contains(logsOf(tenantNS, agcDeploy), refused)
		}, "the AGC must reach the collector and refuse its certificate")
		Expect(rootSpans()).To(BeZero(), "a span arrived over a connection the AGC should not have trusted")
	})

	It("E2E_Tracing_ProxiedTenantExportsOverTLSToPrivateCACollector: naming the bundle delivers the spans", func() {
		_, err := utils.Run(exec.Command("kubectl", "patch", "actionsgateways.actions-gateway.com", gwName, "-n", tenantNS,
			"--type=merge", "-p", `{"spec":{"tracing":{"caBundleRef":{"name":"collector-ca"}}}}`))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			return agcEnvValue(tenantNS, agcDeploy, "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE")
		}, 2*time.Minute, 2*time.Second).Should(Equal("/etc/actions-gateway/tracing-ca/ca.crt"))
		_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deploy/"+agcDeploy, "-n", tenantNS, "--timeout=4m"))
		Expect(err).NotTo(HaveOccurred())

		queueJobsUntil(func() bool { return rootSpans() > 0 }, "the provision path's root span must reach the collector")

		Expect(agcEnvValue(tenantNS, agcDeploy, "HTTPS_PROXY")).NotTo(BeEmpty(), "the tenant must still be proxied")
		Expect(liveAGCLogs()).NotTo(ContainSubstring("traces export"), "the rolled AGC must export without a failed attempt")
	})

	It("E2E_Tracing_WorkerPodCannotReachCollector: the peer opens the AGC's policy and not the worker's", func() {
		if !egressEnforcingCNI() {
			Skip("kindnet does not enforce egress drops; the Calico lane (e2e-calico.yml) runs this")
		}
		collector := "--insecure " + endpoint + "/"

		By("control: an unlabelled pod reaches the collector (destination is up)")
		logs := runGatedEgressProbe(tenantNS, "otel-control", false, collector, collector)
		Expect(logs).To(MatchRegexp(`GATE_RC=0(\s|$)`),
			"an unlabelled pod could not reach the collector, so the negative below would pass whatever the policy does; logs:\n%s", logs)

		By("negative: a workload-labelled pod reaches its proxy and not the collector")
		proxy := fmt.Sprintf("--insecure https://%s-proxy.%s.svc.cluster.local:8080/", proxyName, tenantNS)
		logs = runGatedEgressProbe(tenantNS, "otel-workload", true, proxy, collector)
		Expect(logs).To(MatchRegexp(`GATE_RC=0(\s|$)`),
			"the probe pod could not reach its own proxy, so its endpoint was never programmed and the drop below says nothing (Q1015); logs:\n%s", logs)
		Expect(logs).To(MatchRegexp(`CURL_RC=(7|28)(\s|$)`),
			"a worker pod reached the trace collector: spec.tracing.networkPolicy leaked past the AGC's own policy; logs:\n%s", logs)
	})

	It("E2E_Tracing_LostBundleDoesNotStopTheGateway: a replacement AGC starts without the ConfigMap and the gateway stays Ready", func() {
		_, err := utils.Run(exec.Command("kubectl", "delete", "configmap", "collector-ca", "-n", tenantNS))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "rollout", "restart", "deploy/"+agcDeploy, "-n", tenantNS))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deploy/"+agcDeploy, "-n", tenantNS, "--timeout=4m"))
		Expect(err).NotTo(HaveOccurred(), "a required mount would hold the replacement at ContainerCreating")

		Eventually(func() string { return gatewayCondition("TracingCABundleUnresolved") }, 3*time.Minute, 2*time.Second).
			Should(Equal("True"), "the lost bundle must report")
		Eventually(func() string { return gatewayCondition("Ready") }, 2*time.Minute, 2*time.Second).Should(Equal("True"))

		before, workers := rootSpans(), workerPods()
		queueJobsUntil(func() bool {
			return workerPods() > workers && strings.Contains(liveAGCLogs(), refused)
		}, "jobs must still run, and with no bundle the exporter must keep verifying against the system roots")
		Expect(rootSpans()).To(Equal(before), "no span may cross a connection the AGC could not verify")

		By("adding the ConfigMap back clears the condition, and only a restart makes the exporter read it")
		Expect(utils.ApplyManifest(collectorCAManifest())).To(Succeed())
		Eventually(func() string { return gatewayCondition("TracingCABundleUnresolved") }, 3*time.Minute, 2*time.Second).
			Should(Equal("False"), "the re-read must notice the ConfigMap")
		time.Sleep(90 * time.Second) // Past the kubelet's ConfigMap volume sync, so the file is there to be ignored.
		workers = workerPods()
		queueJobsUntil(func() bool { return workerPods() > workers }, "a job must run with the file back")
		// The batch processor exports every 5s with a 10s timeout, so this outlasts several.
		Consistently(rootSpans, 30*time.Second, 5*time.Second).Should(Equal(before),
			"the exporter read the file at startup, so the running AGC must not have picked it up")

		_, err = utils.Run(exec.Command("kubectl", "rollout", "restart", "deploy/"+agcDeploy, "-n", tenantNS))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deploy/"+agcDeploy, "-n", tenantNS, "--timeout=4m"))
		Expect(err).NotTo(HaveOccurred())
		queueJobsUntil(func() bool { return rootSpans() > before }, "the restarted AGC must deliver")
	})
})

// collectorCertificates returns a throwaway CA and a server certificate it signs for host.
func collectorCertificates(host string) (caPEM, certPEM, keyPEM string) {
	GinkgoHelper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e-collector-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	serverDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, caTemplate, &serverKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	Expect(err).NotTo(HaveOccurred())

	encode := func(kind string, der []byte) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
	}
	return encode("CERTIFICATE", caDER), encode("CERTIFICATE", serverDER), encode("EC PRIVATE KEY", keyDER)
}

// tlsCollectorManifest is a real OpenTelemetry Collector with a TLS OTLP/gRPC receiver and
// the debug exporter, whose detailed log is the only sink the spec needs to read.
func tlsCollectorManifest(ns, image, certPEM, keyPEM string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: otel-collector-tls
  namespace: %[1]s
data:
  tls.crt: %[3]s
  tls.key: %[4]s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: otel-collector
  namespace: %[1]s
data:
  config.yaml: |
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: 0.0.0.0:4317
            tls:
              cert_file: /etc/otel/tls/tls.crt
              key_file: /etc/otel/tls/tls.key
    exporters:
      debug:
        verbosity: detailed
    service:
      pipelines:
        traces:
          receivers: [otlp]
          exporters: [debug]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: otel-collector
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: otel-collector
  template:
    metadata:
      labels:
        app: otel-collector
    spec:
      containers:
      - name: collector
        image: %[2]s
        args: ["--config=/etc/otel/config.yaml"]
        ports:
        - containerPort: 4317
        volumeMounts:
        - name: config
          mountPath: /etc/otel
        - name: tls
          mountPath: /etc/otel/tls
      volumes:
      - name: config
        configMap:
          name: otel-collector
      - name: tls
        secret:
          secretName: otel-collector-tls
          defaultMode: 0444
---
apiVersion: v1
kind: Service
metadata:
  name: otel-collector
  namespace: %[1]s
spec:
  selector:
    app: otel-collector
  ports:
  - port: 4317
    targetPort: 4317
`, ns, image, base64.StdEncoding.EncodeToString([]byte(certPEM)), base64.StdEncoding.EncodeToString([]byte(keyPEM)))
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+prefix) + "\n"
}
