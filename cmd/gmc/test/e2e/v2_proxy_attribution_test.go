//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_V2_ProxyAttribution proves proxy failure attribution (Q1184) live: a pool in
// one namespace gives each of two consumer namespaces its own CONNECT port, and the
// proxy learns the consumer from that port alone. So the per-port ingress rule is
// the whole boundary between one tenant's failure records and another's, and the
// first spec measures it holding on an enforcing CNI. The second follows one
// failed CONNECT from a worker-labelled pod to the gateway's attributed counter,
// which only the mTLS read, the cursor, and the address-window join together move.
var _ = Describe("E2E_V2_ProxyAttribution", Ordered, func() {
	const (
		poolNS     = "v2-attr-pool"
		alphaNS    = "v2-attr-alpha"
		betaNS     = "v2-attr-beta"
		poolName   = "pool"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
		alphaAGC   = "alpha-agc"
		betaAGC    = "beta-agc"
		runnerSet  = "set-attr"
		// Unresolvable, so the proxy answers the CONNECT with a 502 and records a
		// dial failure. A denial would need the pool opted in to an allowlist, which
		// takes an extra destination the GMC's platform allowlist has to admit.
		failTarget = "https://unresolvable.invalid/"
	)
	var (
		shareCM = "proxy-share-" + poolNS + "-" + poolName
		attrCM  = "proxy-attribution-" + poolNS + "-" + poolName
		// proxyHost is the pool's Service host, read from the share projection.
		proxyHost           string
		alphaPort, betaPort string
	)

	projected := func(cm, ns, key string) string {
		var v string
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "configmap", cm, "-n", ns,
				"-o", "jsonpath={.data."+key+"}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(BeEmpty(), "%s/%s carries no %s yet", ns, cm, key)
			v = out
		}, 4*time.Minute, 5*time.Second).Should(Succeed())
		return v
	}

	BeforeAll(func() {
		tenant := map[string]string{"actions-gateway.com/tenant": "managed"}
		utils.CreateNamespace(poolNS, tenant)
		utils.CreateNamespace(betaNS, tenant)
		// The AGC admits metrics scrapes only from a namespace labelled metrics=enabled.
		utils.CreateNamespace(alphaNS, map[string]string{"actions-gateway.com/tenant": "managed", "metrics": "enabled"})
		utils.CreateGitHubAppSecret(alphaNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.CreateGitHubAppSecret(betaNS, secretName, 12345, 67890, testRSAKeyPEM)

		By("applying a pool attributing for two consumer namespaces, and an opted-in gateway in each")
		Expect(utils.ApplyManifestWithWebhookRetry(v2ProxyAttributionManifest(
			poolNS, poolName, alphaNS, betaNS, secretName, agcImage, runnerSet))).To(Succeed())
		utils.ApplyFakegithubEgressNetworkPolicy(alphaNS)
		utils.ApplyFakegithubEgressNetworkPolicy(betaNS)

		utils.WaitForDeploymentReady(poolNS, poolName+"-proxy", 4*time.Minute)
		utils.WaitForDeploymentReady(alphaNS, alphaAGC, 4*time.Minute)
		utils.WaitForDeploymentReady(betaNS, betaAGC, 4*time.Minute)

		// A port is projected only once the pool has rolled out with it, so waiting
		// for both means no later roll replaces a replica holding a spec's record.
		alphaPort = projected(attrCM, alphaNS, "proxy-port")
		betaPort = projected(attrCM, betaNS, "proxy-port")
		Expect(alphaPort).NotTo(Equal(betaPort))
		proxyHost = projected(shareCM, alphaNS, "proxy-host")
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpEgressProxyDiagnostics(poolNS, poolName+"-proxy")
			out, _ := utils.Run(exec.Command("kubectl", "logs", "-n", alphaNS, "deployment/"+alphaAGC, "--tail=200"))
			_, _ = fmt.Fprintf(GinkgoWriter, "--- %s/%s logs ---\n%s\n", alphaNS, alphaAGC, out)
			// The pool's side of a port that never arrived: its conditions and Events,
			// and every GMC replica's lines about it, since the leader may be either.
			out, _ = utils.Run(exec.Command("kubectl", "get", "egressproxy", poolName, "-n", poolNS, "-o", "jsonpath={.status}"))
			_, _ = fmt.Fprintf(GinkgoWriter, "--- %s/%s status ---\n%s\n", poolNS, poolName, out)
			out, _ = utils.Run(exec.Command("kubectl", "get", "events", "-n", poolNS, "--sort-by=.lastTimestamp"))
			_, _ = fmt.Fprintf(GinkgoWriter, "--- events in %s ---\n%s\n", poolNS, out)
			out, _ = utils.Run(exec.Command("kubectl", "logs", "-n", gmcNamespace, "-l", "control-plane=controller-manager",
				"--prefix", "--tail=-1"))
			var gmcLines []string
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "v2-attr-") {
					gmcLines = append(gmcLines, line)
				}
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "--- GMC log lines naming v2-attr-* ---\n%s\n", strings.Join(gmcLines, "\n"))
		}
	})

	AfterAll(func() {
		for _, gw := range []struct{ ns, name string }{{alphaNS, "alpha"}, {betaNS, "beta"}} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
				"agc-clusterrunnertemplate-reader."+gw.ns+"."+gw.name, "--ignore-not-found"))
		}
		for _, ns := range []string{alphaNS, betaNS, poolNS} {
			utils.DeleteNamespace(ns)
		}
	})

	It("E2E_V2_ProxyAttribution_WorkerRefusedOnAnotherConsumersPort: a worker gets no answer on a port attributed to another namespace", func() {
		if !egressEnforcingCNI() {
			Skip("kindnet's enforcer does not drop NetworkPolicy negatives; this runs on the Calico lane")
		}
		// The gate polls the pod's own port until the proxy answers, so the negative
		// taken afterwards in the SAME pod cannot pass on an endpoint the CNI has not
		// programmed yet (see runGatedEgressProbe). The workload NetworkPolicy admits
		// the whole attribution range, so a drop here is the pool's ingress rule.
		script := fmt.Sprintf(`      set -u
      n=0
      code=000
      while [ "$n" -lt 30 ]; do
        n=$((n+1))
        code=$(curl --silent --output /dev/null --connect-timeout 3 --max-time 5 \
          --proxy https://%[1]s:%[2]s --proxy-cacert /proxy-ca/ca.crt \
          --write-out '%%{http_connect}' %[4]s) || true
        if [ "$code" != "000" ]; then break; fi
        sleep 2
      done
      echo "GATE_CONNECT=${code} GATE_ATTEMPTS=${n}"
      rc=0
      code=$(curl --silent --show-error --output /dev/null --connect-timeout 10 --max-time 20 \
        --proxy https://%[1]s:%[3]s --proxy-cacert /proxy-ca/ca.crt \
        --write-out '%%{http_connect}' %[4]s) || rc=$?
      echo "PROXY_CONNECT=${code}"
      echo "CURL_RC=${rc}"
      echo DONE
`, proxyHost, alphaPort, betaPort, failTarget)
		logs := runAttributionProbe(alphaNS, "attr-cross-port", map[string]string{
			"actions-gateway/component": "workload",
		}, "proxy-ca", proxyCAVolume(shareCM), script)

		Expect(logs).To(MatchRegexp(`GATE_CONNECT=[1-9]`),
			"the pod's own port %s never answered, so the negative below proves nothing; logs:\n%s", alphaPort, logs)
		Expect(logs).To(MatchRegexp(`CURL_RC=(7|28)\b`),
			"a worker in %s reached the port attributed to %s; logs:\n%s", alphaNS, betaNS, logs)
		Expect(logs).To(ContainSubstring("PROXY_CONNECT=000"), "logs:\n%s", logs)
	})

	It("E2E_V2_ProxyAttribution_FailureAttributedToTheWorker: a failed CONNECT reaches the gateway's attributed counter", func() {
		// The RunnerSet label is what makes the AGC count this pod as its own worker.
		// The pod stays Running afterwards so its address window is still open when
		// the record is read; the sleep first keeps the record clear of the window's
		// opening edge.
		script := fmt.Sprintf(`      set -u
      sleep 5
      n=0
      code=000
      while [ "$n" -lt 30 ]; do
        n=$((n+1))
        code=$(curl --silent --output /dev/null --connect-timeout 3 --max-time 5 \
          --proxy https://%[1]s:%[2]s --proxy-cacert /proxy-ca/ca.crt \
          --write-out '%%{http_connect}' %[3]s) || true
        if [ "$code" != "000" ]; then break; fi
        sleep 2
      done
      echo "PROXY_CONNECT=${code} ATTEMPTS=${n}"
      echo DONE
      sleep 900
`, proxyHost, alphaPort, failTarget)
		logs := runAttributionProbe(alphaNS, "attr-worker", map[string]string{
			"actions-gateway/component":      "workload",
			"actions-gateway.com/runner-set": runnerSet,
		}, "proxy-ca", proxyCAVolume(shareCM), script)
		Expect(logs).To(ContainSubstring("PROXY_CONNECT=502"),
			"the proxy did not fail the CONNECT on %s's port, so there is no record to attribute; logs:\n%s", alphaNS, logs)

		// The scrape runs in the cluster so the gateway's metrics client key never
		// leaves it. It polls past the AGC's 30-second read interval.
		scrape := fmt.Sprintf(`      set -u
      n=0
      line=""
      while [ "$n" -lt 40 ]; do
        n=$((n+1))
        line=$(curl --silent --cacert /scrape/ca.crt --cert /scrape/tls.crt --key /scrape/tls.key \
          https://%[1]s.%[2]s.svc:8443/metrics \
          | grep '^actions_gateway_proxy_failure_records_total{' \
          | grep 'outcome="dial-failed"' | grep 'result="attributed"') || true
        if [ -n "$line" ]; then break; fi
        sleep 5
      done
      echo "ATTRIBUTED=${line:-none}"
      curl --silent --cacert /scrape/ca.crt --cert /scrape/tls.crt --key /scrape/tls.key \
        https://%[1]s.%[2]s.svc:8443/metrics | grep '^actions_gateway_proxy_failure_' || true
      echo DONE
`, alphaAGC, alphaNS)
		out := runAttributionProbe(alphaNS, "attr-scrape", map[string]string{"e2e-probe": "control"},
			"scrape", fmt.Sprintf(`    secret:
      secretName: %s-metrics-client
      defaultMode: 0444`, alphaAGC), scrape)
		Expect(out).To(MatchRegexp(`ATTRIBUTED=actions_gateway_proxy_failure_records_total\{[^}]*\} [1-9]`),
			"the gateway never attributed the failed CONNECT to its worker; scrape:\n%s", out)
	})
})

// proxyCAVolume is a volume source holding the pool's CA from the share projection.
func proxyCAVolume(shareCM string) string {
	return fmt.Sprintf(`    configMap:
      name: %s
      items:
      - key: ca.crt
        path: ca.crt`, shareCM)
}

// runAttributionProbe applies a curl pod with the given labels and one volume, from
// source and mounted at /<volName>, and returns its logs once they carry DONE. Unlike
// runProbePod it does not wait for the pod to end, so a script may keep the pod
// Running after it reports.
func runAttributionProbe(ns, name string, labels map[string]string, volName, source, script string) string {
	var labelLines strings.Builder
	for k, v := range labels {
		fmt.Fprintf(&labelLines, "\n    %s: %q", k, v)
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:%s
spec:
  restartPolicy: Never
  volumes:
  - name: %s
%s
  containers:
  - name: curl
    image: %s
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c"]
    volumeMounts:
    - name: %s
      mountPath: /%s
      readOnly: true
    args:
    - |
%s`, name, ns, labelLines.String(), volName, source, curlImage, volName, volName, script)

	ExpectWithOffset(1, utils.ApplyManifest(manifest)).To(Succeed(), "apply probe pod %s/%s", ns, name)
	DeferCleanup(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", name, "-n", ns, "--ignore-not-found", "--wait=false"))
	})

	By("waiting for probe pod " + name + " to report")
	var logs string
	EventuallyWithOffset(1, func(g Gomega) {
		out, err := utils.Run(exec.Command("kubectl", "logs", name, "-n", ns))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(ContainSubstring("DONE"))
		logs = out
	}, 6*time.Minute, 5*time.Second).Should(Succeed())
	return logs
}

// v2ProxyAttributionManifest is a pool in poolNS shared with, and attributing for,
// two consumer namespaces. Each holds a gateway opted in to PerJob whose
// defaultProxyRef names the pool, and a RunnerSet resolving to it, since the AGC
// reads the pools its RunnerSets resolve to. The cross-namespace defaultProxyRef
// also carries the AGC's own egress through the pool (Q1209).
func v2ProxyAttributionManifest(poolNS, pool, alphaNS, betaNS, secretName, image, runnerSet string) string {
	consumer := func(ns, gateway, org, set string) string {
		return fmt.Sprintf(`---
apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  githubURL: https://github.com/%[3]s
  credentials:
    type: GitHubApp
    githubApp:
      name: %[5]s
  defaultProxyRef:
    name: %[7]s
    namespace: %[8]s
  proxyFailureAttribution: PerJob
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerTemplate
metadata:
  name: tmpl
  namespace: %[1]s
spec:
  workerImage: %[6]s
  podTemplate:
    spec:
      containers:
      - name: runner
        image: %[6]s
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata:
  name: %[4]s
  namespace: %[1]s
spec:
  gatewayRef:
    name: %[2]s
  templateRef:
    name: tmpl
  maxListeners: 1
  acquisitionProtocol: Classic   # see v2MultiGatewayManifest (Q264 P5)
  runnerLabels: ["e2e-attr"]
`, ns, gateway, org, set, secretName, image, pool, poolNS)
	}
	return fmt.Sprintf(`apiVersion: actions-gateway.com/v2alpha1
kind: EgressProxy
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  # One replica: the port spec's gate proves only the endpoint it reached was
  # programmed, and a record lives in one replica's memory until it is read.
  minReplicas: 1
  maxReplicas: 1
  failureAttribution: PerConsumerPort
  sharing:
    allowedNamespaces: [%[3]s, %[4]s]
`, poolNS, pool, alphaNS, betaNS) +
		consumer(alphaNS, "alpha", "example-org-attr-alpha", runnerSet) +
		consumer(betaNS, "beta", "example-org-attr-beta", runnerSet+"-beta")
}
