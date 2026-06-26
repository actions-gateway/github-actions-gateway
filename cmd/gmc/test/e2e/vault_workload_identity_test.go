//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_VaultWorkloadIdentity proves the no-PEM delegation path (Q197/Q201) live on
// kind: a gateway with credentials.type=WorkloadIdentity holds NO GitHub App
// Secret, yet its AGC authenticates to the GitHub broker by having an in-cluster
// TEST Vault sign the App JWT via transit — the AGC proving its pod identity to
// Vault with a kubelet-projected ServiceAccount token.
//
// The decisive assertion is a LIVE broker session for the gateway's RunnerSet: the
// AGC can only open one after minting an installation token, which it can only mint
// by (a) logging in to Vault with its projected SA token and (b) getting Vault
// transit to sign the App JWT. No App private key exists anywhere in the cluster —
// the round-trip succeeds purely on the delegated signature.
//
// The Vault here is a dev-mode TEST instance (in-memory, root token, HTTP listener)
// with a throwaway transit key; no real Vault and no real credential are involved.
// fakegithub does not verify the JWT signature, so this exercises the no-PEM PATH
// (the AGC mints a token with no PEM), not GitHub's signature check.
var _ = Describe("E2E_VaultWorkloadIdentity", Ordered, Label("vault-workload-identity"), func() {
	const (
		tenantNS   = "tenant-vault-wi"
		gwName     = "wi"
		gwAGC      = "wi-agc"
		runnerSet  = "set-wi"
		vaultRole  = "agc-wi"
		transitKey = "agc"
	)
	var vaultImage string

	BeforeAll(func() {
		vaultImage = envOrDefault("E2E_VAULT_IMAGE", "hashicorp/vault:1.18.3")

		By("deploying an in-cluster TEST Vault (dev mode) in " + infraNamespace)
		setupTestVault(vaultImage)

		By("configuring Vault: transit RSA key + Kubernetes auth role bound to the AGC SA")
		configureTestVault(tenantNS, gwAGC, vaultRole, transitKey)

		By("creating the tenant namespace (NO GitHub App Secret — the whole point)")
		utils.CreateNamespace(tenantNS, map[string]string{
			"actions-gateway.com/tenant": "managed",
		})

		By("applying the workload-identity ActionsGateway + RunnerTemplate + RunnerSet")
		vaultAddr := fmt.Sprintf("http://vault.%s.svc.cluster.local:8200", infraNamespace)
		Expect(utils.ApplyManifest(vaultWorkloadIdentityManifest(
			tenantNS, gwName, runnerSet, vaultAddr, transitKey, vaultRole, agcImage))).To(Succeed())

		By("granting the AGC egress to fakegithub and to Vault (both in " + infraNamespace + ")")
		// kindnet does not enforce egress drops, but apply these so the spec is correct
		// under a policy-enforcing CNI too (the GMC does not yet auto-provision an
		// AGC→Vault egress rule — see docs/operations/tenant-onboarding.md / Q201).
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)
		applyVaultEgressNetworkPolicy(tenantNS, infraNamespace)

		By("waiting for the workload-identity AGC Deployment to become ready")
		utils.WaitForDeploymentReady(tenantNS, gwAGC, 4*time.Minute)

		By("starting persistent port-forward to the fakegithub control API")
		fakegithubLocalPort = fmt.Sprintf("%d", 19790+GinkgoParallelProcess())
		fakegithubPFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(fakegithubPFCmd.Start()).To(Succeed())
		Eventually(func() error {
			resp, err := http.Get("http://localhost:" + fakegithubLocalPort + "/control/sessions")
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(tenantNS, gwAGC, infraNamespace, fakegithubServiceName)
			if out, err := utils.Run(exec.Command("kubectl", "logs", "-n", infraNamespace, "deploy/vault", "--tail=50")); err == nil {
				AddReportEntry("vault logs", out)
			}
		}
	})

	AfterAll(func() {
		if fakegithubPFCmd != nil && fakegithubPFCmd.Process != nil {
			_ = fakegithubPFCmd.Process.Kill()
		}
		// Per-gateway ClusterRoleBinding is cluster-scoped (not namespace-GC'd).
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
			"agc-clusterrunnertemplate-reader."+tenantNS+"."+gwName, "--ignore-not-found"))
		utils.DeleteNamespace(tenantNS)
		teardownTestVault()
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_VaultWI_NoPEMSecret: the gateway provisions with no GitHub App Secret and a projected Vault token", func() {
		By("verifying no GitHub App credential Secret exists in the namespace")
		out, err := utils.Run(exec.Command("kubectl", "get", "secret",
			"-n", tenantNS, "-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`))
		Expect(err).NotTo(HaveOccurred())
		// Only metrics-TLS Secrets (created by the GMC) may exist; none holds an App key.
		Expect(out).NotTo(ContainSubstring("github-app"), "workload identity must hold no GitHub App Secret")

		By("verifying the AGC selects the workload-identity credential method")
		credType, err := utils.Run(exec.Command("kubectl", "get", "deployment", gwAGC,
			"-n", tenantNS,
			"-o", `jsonpath={range .spec.template.spec.containers[?(@.name=="agc")].env[?(@.name=="CREDENTIAL_TYPE")]}{.value}{end}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(credType).To(Equal("WorkloadIdentity"))

		By("verifying the AGC pod projects a Vault-audience ServiceAccount token (not a Secret mount)")
		aud, err := utils.Run(exec.Command("kubectl", "get", "deployment", gwAGC,
			"-n", tenantNS,
			"-o", `jsonpath={.spec.template.spec.volumes[*].projected.sources[*].serviceAccountToken.audience}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(aud).To(ContainSubstring("vault"), "AGC must project a Vault-audience SA token")
	})

	It("E2E_VaultWI_NoPEMRoundTrip: the AGC opens a broker session via a Vault-signed App JWT", func() {
		// A live session for the RunnerSet owner can only exist if the AGC minted an
		// installation token — which, with no PEM in the cluster, required a successful
		// Vault login + transit sign. This is the keystone no-PEM round-trip assertion.
		Eventually(func(g Gomega) {
			sessions := fakegithubActiveSessionsForOwner(g, runnerSet+"-")
			g.Expect(sessions).NotTo(BeEmpty(),
				"no live broker session for RunnerSet %s — the AGC could not mint a token via Vault (no-PEM round-trip failed)", runnerSet)
		}, 4*time.Minute, 2*time.Second).Should(Succeed())
	})
})

// setupTestVault deploys a dev-mode Vault (in-memory, root token, HTTP) plus its
// ServiceAccount bound to system:auth-delegator (so Vault can TokenReview the AGC's
// projected token) and a Service. Dev mode holds no persistent or real credentials.
func setupTestVault(image string) {
	Expect(utils.ApplyManifest(testVaultManifest(image, infraNamespace))).To(Succeed(), "deploy test Vault")
	utils.WaitForDeploymentReady(infraNamespace, "vault", 3*time.Minute)
}

func teardownTestVault() {
	_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding", "vault-e2e-auth-delegator", "--ignore-not-found"))
	for _, kind := range []string{"deployment", "service", "serviceaccount"} {
		_, _ = utils.Run(exec.Command("kubectl", "delete", kind, "vault", "-n", infraNamespace, "--ignore-not-found", "--wait=false"))
	}
}

// configureTestVault enables the transit engine with an RSA key, enables Kubernetes
// auth, and writes a policy + role binding the AGC ServiceAccount (in tenantNS) to
// the transit-sign capability. All run inside the Vault pod via the dev root token —
// no credential ever leaves the cluster or touches the test host.
func configureTestVault(tenantNS, agcSA, role, key string) {
	script := fmt.Sprintf(`set -e
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN=root
vault secrets enable transit || true
vault write -f transit/keys/%[1]s type=rsa-2048
vault auth enable kubernetes || true
vault write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc
echo 'path "transit/sign/%[1]s" { capabilities = ["update"] }' | vault policy write transit-sign -
vault write auth/kubernetes/role/%[2]s \
  bound_service_account_names=%[3]s \
  bound_service_account_namespaces=%[4]s \
  token_policies=transit-sign \
  audience=vault \
  ttl=20m
`, key, role, agcSA, tenantNS)

	Eventually(func(g Gomega) {
		_, err := utils.Run(exec.Command("kubectl", "exec", "-n", infraNamespace, "deploy/vault", "--", "sh", "-c", script))
		g.Expect(err).NotTo(HaveOccurred(), "configure test Vault")
	}, 2*time.Minute, 5*time.Second).Should(Succeed())
}

// applyVaultEgressNetworkPolicy permits the gateway's AGC (a workload-labeled pod)
// to reach the test Vault on 8200 in the infra namespace. Correctness under a
// policy-enforcing CNI; a no-op under kindnet (which does not drop egress).
func applyVaultEgressNetworkPolicy(ns, vaultNS string) {
	manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: e2e-vault-egress
  namespace: %s
spec:
  podSelector:
    matchLabels:
      actions-gateway/component: workload
  policyTypes: [Egress]
  egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: %s
      podSelector:
        matchLabels:
          app: vault
    ports:
    - port: 8200
      protocol: TCP
`, ns, vaultNS)
	Expect(utils.ApplyManifest(manifest)).To(Succeed(), "apply Vault egress NP in %s", ns)
}

// testVaultManifest renders a dev-mode Vault Deployment + Service + ServiceAccount,
// with a ClusterRoleBinding granting the Vault SA system:auth-delegator so Vault can
// review the AGC's projected token via the TokenReview API.
func testVaultManifest(image, ns string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: vault
  namespace: %[2]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: vault-e2e-auth-delegator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:auth-delegator
subjects:
- kind: ServiceAccount
  name: vault
  namespace: %[2]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vault
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: vault
  template:
    metadata:
      labels:
        app: vault
    spec:
      serviceAccountName: vault
      containers:
      - name: vault
        image: %[1]s
        imagePullPolicy: IfNotPresent
        args: ["server", "-dev", "-dev-listen-address=0.0.0.0:8200"]
        env:
        - name: VAULT_DEV_ROOT_TOKEN_ID
          value: root
        - name: VAULT_ADDR
          value: http://127.0.0.1:8200
        ports:
        - containerPort: 8200
          name: http
        securityContext:
          capabilities:
            add: ["IPC_LOCK"]
        readinessProbe:
          httpGet:
            path: /v1/sys/health?standbyok=true
            port: 8200
          initialDelaySeconds: 3
          periodSeconds: 3
---
apiVersion: v1
kind: Service
metadata:
  name: vault
  namespace: %[2]s
spec:
  selector:
    app: vault
  ports:
  - name: http
    port: 8200
    targetPort: 8200
`, image, ns)
}

// vaultWorkloadIdentityManifest renders the workload-identity object set: an
// ActionsGateway (no Secret, direct egress), a RunnerTemplate, and a RunnerSet bound
// to it so the AGC opens broker sessions (the no-PEM round-trip under test).
func vaultWorkloadIdentityManifest(ns, gw, runnerSet, vaultAddr, key, role, workerImage string) string {
	return fmt.Sprintf(`apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  githubURL: https://github.com/example-org
  credentials:
    type: WorkloadIdentity
    workloadIdentity:
      appId: 12345
      installationId: 67890
      signer:
        provider: Vault
        vault:
          address: %[4]s
          keyName: %[5]s
          auth:
            role: %[6]s
  logLevel: debug
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerTemplate
metadata:
  name: tmpl
  namespace: %[1]s
spec:
  workerImage: %[7]s
  podTemplate:
    spec:
      containers:
      - name: runner
        image: %[7]s
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata:
  name: %[3]s
  namespace: %[1]s
spec:
  gatewayRef:
    name: %[2]s
  templateRef:
    name: tmpl
  maxListeners: 2
  runnerLabels: ["e2e-wi"]
`, ns, gw, runnerSet, vaultAddr, key, role, workerImage)
}
