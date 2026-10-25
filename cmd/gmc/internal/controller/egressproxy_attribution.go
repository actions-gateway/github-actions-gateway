package controller

// Proxy failure attribution, the pool side (Q1184; design 05-security.md "Proxy
// failure attribution"). A pool with spec.failureAttribution PerConsumerPort gives
// each opted-in consumer namespace its own CONNECT port, and the proxy records the
// failures arriving on it for that namespace's AGC to read over a dedicated mTLS
// listener. With no attributed namespace the pool's children are exactly what they
// were without the feature.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	failureAttributionPerConsumerPort = "PerConsumerPort"
	proxyFailureAttributionPerJob     = "PerJob"

	// attributionPortBase is the first attributed CONNECT port. The range holds the
	// pool's own namespace plus the 32 the CRD lets allowedNamespaces list while
	// attribution is on.
	attributionPortBase     = 8100
	maxAttributedNamespaces = 33

	// attributionReadPort is the proxy's mTLS listener the AGCs read failures from.
	attributionReadPort = 8444

	attributionTLSVolumeName = "attribution-tls"
	attributionTLSMountPath  = "/etc/actions-gateway/attribution-tls"

	// "<ep>-attribution-tls" is the server bundle mounted into the pool;
	// "<ep>-attribution-clients" holds one client cert per attributed namespace
	// under "<ns>.crt"/"<ns>.key", and is never mounted into the pool.
	egressProxyAttributionTLSSuffix     = "-attribution-tls"
	egressProxyAttributionClientsSuffix = "-attribution-clients"
	// "<ep>-proxy-attr" is the headless Service whose DNS records name each replica.
	egressProxyAttributionServiceSuffix = "-proxy-attr"

	// annAttributionCA carries a hash of the attribution CA on the pool's pod
	// template, so a regenerated bundle rolls the pool: the proxy reads its client
	// CA only at start.
	annAttributionCA = "actions-gateway.com/attribution-ca-sha256"

	attributionClientCertSuffix = ".crt"
	attributionClientKeySuffix  = ".key"
)

func egressProxyAttributes(ep *gmcv2alpha1.EgressProxy) bool {
	return ep.Spec.FailureAttribution == failureAttributionPerConsumerPort
}

func egressProxyAttributionTLSSecretName(ep *gmcv2alpha1.EgressProxy) string {
	return ep.Name + egressProxyAttributionTLSSuffix
}

func egressProxyAttributionClientsSecretName(ep *gmcv2alpha1.EgressProxy) string {
	return ep.Name + egressProxyAttributionClientsSuffix
}

func egressProxyAttributionServiceName(ep *gmcv2alpha1.EgressProxy) string {
	return ep.Name + egressProxyAttributionServiceSuffix
}

// attributedNamespacesFor returns the namespaces this pool gives a port: those
// that reference it, that it admits (its own, or one sharing grants), and where
// any ActionsGateway has set proxyFailureAttribution PerJob. Sorted, and cut to
// maxAttributedNamespaces, which admission already bounds.
func attributedNamespacesFor(ctx context.Context, c client.Client, ep *gmcv2alpha1.EgressProxy) ([]string, error) {
	if !egressProxyAttributes(ep) {
		return nil, nil
	}
	var gateways gmcv2alpha1.ActionsGatewayList
	if err := c.List(ctx, &gateways); err != nil {
		return nil, fmt.Errorf("list ActionsGateways: %w", err)
	}
	var runnerSets gmcv2alpha1.RunnerSetList
	if err := c.List(ctx, &runnerSets); err != nil {
		return nil, fmt.Errorf("list RunnerSets: %w", err)
	}

	optedIn := map[string]bool{}
	for i := range gateways.Items {
		if gateways.Items[i].Spec.ProxyFailureAttribution == proxyFailureAttributionPerJob {
			optedIn[gateways.Items[i].Namespace] = true
		}
	}
	attributed := map[string]bool{}
	reference := func(ref *gmcv2alpha1.ProxyObjectRef, referrerNS string) {
		if ref == nil || ref.Name != ep.Name || !optedIn[referrerNS] {
			return
		}
		target := ref.Namespace
		if target == "" {
			target = referrerNS
		}
		if target != ep.Namespace {
			return
		}
		if referrerNS != ep.Namespace && !proxyShareGranted(ep, referrerNS) {
			return
		}
		attributed[referrerNS] = true
	}
	for i := range gateways.Items {
		reference(gateways.Items[i].Spec.DefaultProxyRef, gateways.Items[i].Namespace)
	}
	for i := range runnerSets.Items {
		reference(runnerSets.Items[i].Spec.ProxyRef, runnerSets.Items[i].Namespace)
	}

	namespaces := slices.Sorted(maps.Keys(attributed))
	if len(namespaces) > maxAttributedNamespaces {
		namespaces = namespaces[:maxAttributedNamespaces]
	}
	return namespaces, nil
}

// allocateAttributionPorts maps each namespace to a port, keeping any port it
// already holds in current and giving a new namespace the lowest free one, so a
// grant change never moves another namespace.
func allocateAttributionPorts(namespaces []string, current map[string]int) map[string]int {
	if len(namespaces) == 0 {
		return nil
	}
	ports := make(map[string]int, len(namespaces))
	used := map[int]bool{}
	for _, ns := range namespaces {
		if p, ok := current[ns]; ok && p >= attributionPortBase && p < attributionPortBase+maxAttributedNamespaces && !used[p] {
			ports[ns] = p
			used[p] = true
		}
	}
	next := attributionPortBase
	for _, ns := range namespaces {
		if _, ok := ports[ns]; ok {
			continue
		}
		for used[next] {
			next++
		}
		ports[ns] = next
		used[next] = true
	}
	return ports
}

// liveAttributionPorts reads the port map back from the pool Deployment's
// PROXY_ATTRIBUTION_PORTS, which is the record allocation keeps stable against.
// An absent Deployment or an unparseable entry yields no entry.
func liveAttributionPorts(dep *appsv1.Deployment) map[string]int {
	ports := map[string]int{}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != proxyContainerName {
			continue
		}
		for _, e := range c.Env {
			if e.Name != "PROXY_ATTRIBUTION_PORTS" {
				continue
			}
			for _, entry := range strings.Split(e.Value, ",") {
				ns, port, ok := strings.Cut(entry, "=")
				if !ok {
					continue
				}
				if p, err := strconv.Atoi(port); err == nil {
					ports[ns] = p
				}
			}
		}
	}
	return ports
}

// attributionPortsEnv renders the port map as the proxy's namespace=port list,
// sorted by namespace so an unchanged map never changes the pod template.
func attributionPortsEnv(ports map[string]int) string {
	entries := make([]string, 0, len(ports))
	for _, ns := range slices.Sorted(maps.Keys(ports)) {
		entries = append(entries, fmt.Sprintf("%s=%d", ns, ports[ns]))
	}
	return strings.Join(entries, ",")
}

// applyAttributionToDeployment adds the attributed listeners, the read listener
// and its mTLS bundle to the pool Deployment built by buildEgressProxyDeployment.
// No ports leaves it untouched.
func applyAttributionToDeployment(dep *appsv1.Deployment, ep *gmcv2alpha1.EgressProxy, ports map[string]int, caPEM []byte) {
	if len(ports) == 0 {
		return
	}
	spec := &dep.Spec.Template.Spec
	tlsMode := int32(0o440)
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: attributionTLSVolumeName,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  egressProxyAttributionTLSSecretName(ep),
			DefaultMode: &tlsMode,
		}},
	})
	c := &spec.Containers[0]
	c.Ports = append(c.Ports, corev1.ContainerPort{Name: "attribution", ContainerPort: attributionReadPort, Protocol: corev1.ProtocolTCP})
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "PROXY_ATTRIBUTION_PORTS", Value: attributionPortsEnv(ports)},
		corev1.EnvVar{Name: "PROXY_ATTRIBUTION_PORT", Value: strconv.Itoa(attributionReadPort)},
		corev1.EnvVar{Name: "PROXY_ATTRIBUTION_TLS_CERT_FILE", Value: attributionTLSMountPath + "/" + corev1.TLSCertKey},
		corev1.EnvVar{Name: "PROXY_ATTRIBUTION_TLS_KEY_FILE", Value: attributionTLSMountPath + "/" + corev1.TLSPrivateKeyKey},
		corev1.EnvVar{Name: "PROXY_ATTRIBUTION_CLIENT_CA_FILE", Value: attributionTLSMountPath + "/" + metricsCACertKey},
	)
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: attributionTLSVolumeName, MountPath: attributionTLSMountPath, ReadOnly: true})

	sum := sha256.Sum256(caPEM)
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[annAttributionCA] = hex.EncodeToString(sum[:])
}

// applyAttributionToService adds one "attr-<port>" port per attributed namespace
// to the pool Service, each targeting the same container port by number.
func applyAttributionToService(svc *corev1.Service, ports map[string]int) {
	for _, p := range slices.Sorted(maps.Values(ports)) {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       fmt.Sprintf("attr-%d", p),
			Port:       int32(p),
			TargetPort: intstr.FromInt32(int32(p)),
			Protocol:   corev1.ProtocolTCP,
		})
	}
}

// applyAttributionToNetworkPolicy admits each attributed namespace's workload
// pods on its own port only, and its AGC pods on the read port. Each peer carries
// the namespace and pod selectors together so they AND (see the sharing rules in
// buildEgressProxyNetworkPolicy). Rules naming the container port by number keep
// the grant list out of the pod template. Like the sharing rules, another
// namespace's are emitted only while the GMC manages the policy.
func applyAttributionToNetworkPolicy(np *networkingv1.NetworkPolicy, ep *gmcv2alpha1.EgressProxy, ports map[string]int) {
	managed := ep.Spec.ManagedNetworkPolicy == nil || *ep.Spec.ManagedNetworkPolicy
	for _, ns := range slices.Sorted(maps.Keys(ports)) {
		if ns != ep.Namespace && !managed {
			continue
		}
		np.Spec.Ingress = append(np.Spec.Ingress,
			networkingv1.NetworkPolicyIngressRule{
				From:  []networkingv1.NetworkPolicyPeer{attributionPeer(ep, ns, map[string]string{labelComponent: componentWorkload})},
				Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(attributionPortBase)), EndPort: ptr(int32(attributionPortBase + maxAttributedNamespaces - 1))}},
			},
			networkingv1.NetworkPolicyIngressRule{
				From: []networkingv1.NetworkPolicyPeer{attributionPeer(ep, ns, map[string]string{
					labelComponent:           componentWorkload,
					"app.kubernetes.io/name": agcAppName,
				})},
				Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(attributionReadPort))}},
			},
		)
	}
}

// attributionPeer selects pods matching podLabels in ns. The pool's own namespace
// takes a bare pod selector, as the same-namespace workload rule does.
func attributionPeer(ep *gmcv2alpha1.EgressProxy, ns string, podLabels map[string]string) networkingv1.NetworkPolicyPeer {
	peer := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: podLabels}}
	if ns != ep.Namespace {
		peer.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: ns}}
	}
	return peer
}

// buildEgressProxyAttributionService builds the headless Service on the read port
// whose DNS records resolve to every ready replica, since each holds its own buffer.
func buildEgressProxyAttributionService(ep *gmcv2alpha1.EgressProxy) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyAttributionServiceName(ep), Namespace: ep.Namespace, Labels: egressProxyLabels(ep)},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  egressProxyPodSelector(ep),
			Ports: []corev1.ServicePort{{
				Name:       "attribution",
				Port:       attributionReadPort,
				TargetPort: intstr.FromInt32(attributionReadPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// attributionCertBundle is one attribution PKI: a CA, the read listener's server
// leaf, and a client leaf per attributed namespace whose CN is that namespace.
type attributionCertBundle struct {
	caPEM         []byte
	serverCertPEM []byte
	serverKeyPEM  []byte
	clients       map[string][2][]byte // namespace → {cert, key}
}

// generateAttributionCerts builds a bundle for the "<ep>-proxy" Service names. The
// CA key is not persisted: the bundle regenerates whole whenever the namespace set
// changes or nears expiry.
func generateAttributionCerts(namespace, svcName string, consumers []string) (*attributionCertBundle, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	caSerial, err := randSerial()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: fmt.Sprintf("actions-gateway-attribution-ca.%s.%s", svcName, namespace)},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	b := &attributionCertBundle{caPEM: encodeCertPEM(caDER), clients: make(map[string][2][]byte, len(consumers))}
	b.serverCertPEM, b.serverKeyPEM, err = signLeaf(caCert, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: fmt.Sprintf("%s.%s.svc", svcName, namespace)},
		DNSNames:    metricsServerSANsV2(namespace, svcName),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("sign server cert: %w", err)
	}
	for _, ns := range consumers {
		certPEM, keyPEM, err := signLeaf(caCert, caKey, &x509.Certificate{
			Subject:     pkix.Name{CommonName: ns},
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})
		if err != nil {
			return nil, fmt.Errorf("sign client cert for %q: %w", ns, err)
		}
		b.clients[ns] = [2][]byte{certPEM, keyPEM}
	}
	return b, nil
}

func buildEgressProxyAttributionTLSSecret(ep *gmcv2alpha1.EgressProxy, b *attributionCertBundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyAttributionTLSSecretName(ep), Namespace: ep.Namespace, Labels: egressProxyLabels(ep)},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       b.serverCertPEM,
			corev1.TLSPrivateKeyKey: b.serverKeyPEM,
			metricsCACertKey:        b.caPEM,
		},
	}
}

func buildEgressProxyAttributionClientsSecret(ep *gmcv2alpha1.EgressProxy, b *attributionCertBundle) *corev1.Secret {
	data := map[string][]byte{metricsCACertKey: b.caPEM}
	for ns, pair := range b.clients {
		data[ns+attributionClientCertSuffix] = pair[0]
		data[ns+attributionClientKeySuffix] = pair[1]
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyAttributionClientsSecretName(ep), Namespace: ep.Namespace, Labels: egressProxyLabels(ep)},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// attributionClientNamespaces lists the namespaces a clients Secret holds certs for.
func attributionClientNamespaces(s *corev1.Secret) []string {
	var namespaces []string
	for k := range s.Data {
		if ns, ok := strings.CutSuffix(k, attributionClientCertSuffix); ok && k != metricsCACertKey {
			namespaces = append(namespaces, ns)
		}
	}
	slices.Sort(namespaces)
	return namespaces
}

// ensureAttributionCerts keeps the attribution bundle in step with namespaces and
// returns the CA and the client certificates it holds. It regenerates when either Secret is missing, the two
// disagree on the CA, the clients Secret names a different namespace set, or the
// server cert is near expiry. With no namespaces it empties both Secrets rather
// than deleting them, since the GMC holds no delete verb on Secrets.
func (r *EgressProxyReconciler) ensureAttributionCerts(ctx context.Context, ep *gmcv2alpha1.EgressProxy, namespaces []string) ([]byte, map[string][]byte, error) {
	if len(namespaces) == 0 {
		for _, name := range []string{egressProxyAttributionTLSSecretName(ep), egressProxyAttributionClientsSecretName(ep)} {
			var s corev1.Secret
			if err := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: name}, &s); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, nil, err
			}
			if !slices.ContainsFunc(slices.Collect(maps.Values(s.Data)), func(v []byte) bool { return len(v) > 0 }) {
				continue
			}
			// A kubernetes.io/tls Secret must keep its two keys, so they are blanked.
			s.Data = nil
			if s.Type == corev1.SecretTypeTLS {
				s.Data = map[string][]byte{corev1.TLSCertKey: {}, corev1.TLSPrivateKeyKey: {}}
			}
			if err := r.Update(ctx, &s); err != nil {
				return nil, nil, fmt.Errorf("empty %s: %w", name, err)
			}
		}
		return nil, nil, nil
	}

	var server, clients corev1.Secret
	serverErr := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: egressProxyAttributionTLSSecretName(ep)}, &server)
	if serverErr != nil && !apierrors.IsNotFound(serverErr) {
		return nil, nil, serverErr
	}
	clientsErr := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: egressProxyAttributionClientsSecretName(ep)}, &clients)
	if clientsErr != nil && !apierrors.IsNotFound(clientsErr) {
		return nil, nil, clientsErr
	}

	reason := "secret missing"
	if serverErr == nil && clientsErr == nil {
		reason = "namespaces changed"
		if slices.Equal(attributionClientNamespaces(&clients), namespaces) {
			reason = "unparseable cert"
			if cert, err := parseCertPEM(server.Data[corev1.TLSCertKey]); err == nil {
				reason = "near expiry"
				if time.Until(cert.NotAfter) > metricsCertRenewBefore {
					reason = "CA mismatch"
					if ca := server.Data[metricsCACertKey]; len(ca) > 0 && string(ca) == string(clients.Data[metricsCACertKey]) {
						return ca, clients.Data, nil
					}
				}
			}
		}
	}

	bundle, err := generateAttributionCerts(ep.Namespace, proxyResourceName(ep), namespaces)
	if err != nil {
		return nil, nil, fmt.Errorf("generate attribution certs: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ep, buildEgressProxyAttributionTLSSecret(ep, bundle)); err != nil {
		return nil, nil, fmt.Errorf("attribution server Secret: %w", err)
	}
	clientsSecret := buildEgressProxyAttributionClientsSecret(ep, bundle)
	if err := r.applyOwnedSecret(ctx, ep, clientsSecret); err != nil {
		return nil, nil, fmt.Errorf("attribution clients Secret: %w", err)
	}
	r.recordEvent(ep, corev1.EventTypeNormal, "AttributionCertificateIssued", "EnsureAttributionCerts",
		"issued failure-attribution mTLS certificates for %d namespace(s) (%s)", len(namespaces), reason)
	return bundle.caPEM, clientsSecret.Data, nil
}

// reconcileAttributionService applies the headless read Service while any
// namespace is attributed, and deletes it otherwise.
func (r *EgressProxyReconciler) reconcileAttributionService(ctx context.Context, ep *gmcv2alpha1.EgressProxy, ports map[string]int) error {
	if len(ports) > 0 {
		desired := buildEgressProxyAttributionService(ep)
		obj := &corev1.Service{}
		return applyManagedChild(ctx, r.Client, r.Scheme, ep, obj, desired, func() error {
			// clusterIP is immutable, so it is set only on create.
			if obj.ResourceVersion == "" {
				obj.Spec.ClusterIP = desired.Spec.ClusterIP
			}
			obj.Spec.Selector = desired.Spec.Selector
			obj.Spec.Ports = desired.Spec.Ports
			return nil
		})
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ep.Namespace, Name: egressProxyAttributionServiceName(ep)}}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// reconcileAttribution resolves the attributed namespaces, allocates their ports
// against the live Deployment, keeps the bundle in step, and projects each
// namespace's port and client certificate into it. It returns the port map and
// the CA the Deployment must carry; both are empty when nothing is attributed.
func (r *EgressProxyReconciler) reconcileAttribution(ctx context.Context, ep *gmcv2alpha1.EgressProxy) (map[string]int, []byte, error) {
	namespaces, err := attributedNamespacesFor(ctx, r.Client, ep)
	if err != nil {
		return nil, nil, err
	}
	var live appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: proxyResourceName(ep)}, &live); err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("read proxy Deployment: %w", err)
	}
	current := liveAttributionPorts(&live)
	ports := allocateAttributionPorts(namespaces, current)
	// A pool that attributes nothing now and attributed nothing at its last
	// rollout keeps no bundle, but may still have projections: a recreated
	// Deployment forgets its ports, and a projection left behind would move new
	// jobs onto a port nothing serves.
	if len(ports) == 0 && len(current) == 0 {
		if err := r.reconcileAttributionProjections(ctx, ep, nil, nil, false, nil); err != nil {
			return nil, nil, fmt.Errorf("withdraw attribution: %w", err)
		}
		return nil, nil, nil
	}
	caPEM, clients, err := r.ensureAttributionCerts(ctx, ep, namespaces)
	if err != nil {
		return nil, nil, err
	}
	if err := r.reconcileAttributionService(ctx, ep, ports); err != nil {
		return nil, nil, err
	}
	if err := r.reconcileAttributionProjections(ctx, ep, ports, current, deploymentRolledOut(&live), clients); err != nil {
		return nil, nil, fmt.Errorf("project attribution: %w", err)
	}
	return ports, caPEM, nil
}

// deploymentRolledOut reports whether every replica of dep runs its current pod
// template, so each one listens on the ports that template names.
func deploymentRolledOut(dep *appsv1.Deployment) bool {
	if dep.ResourceVersion == "" || dep.Spec.Replicas == nil || dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	want := *dep.Spec.Replicas
	return dep.Status.UpdatedReplicas == want && dep.Status.Replicas == want && dep.Status.AvailableReplicas == want
}

const (
	// Keys of the ConfigMap projected into an attributed namespace. Its presence
	// moves that namespace's workers onto proxy-port; the rest are what phase 3's
	// reader dials, with the client certificate in the same-named Secret.
	attributionProxyPortKey  = "proxy-port"
	attributionReadHostKey   = "read-host"
	attributionReadPortKey   = "read-port"
	attributionServerNameKey = "server-name"

	labelAttributionManaged = "actions-gateway.com/proxy-attribution"
)

// proxyAttributionName names the ConfigMap and Secret projected into a consumer
// namespace, keyed on the pool's namespace and name as the share ConfigMap is.
func proxyAttributionName(proxyNamespace, proxyName string) string {
	return apinames.Join(apinames.MaxLabelValue, "proxy-attribution", proxyNamespace, proxyName)
}

func attributionProjectionLabels(ep *gmcv2alpha1.EgressProxy) map[string]string {
	return map[string]string{
		labelManagedBy:          labelManagerValue,
		labelAttributionManaged: "true",
		labelShareProxyNS:       ep.Namespace,
		labelShareProxyNam:      ep.Name,
	}
}

func buildProxyAttributionConfigMap(ep *gmcv2alpha1.EgressProxy, consumerNS string, port int) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyAttributionName(ep.Namespace, ep.Name),
			Namespace: consumerNS,
			Labels:    attributionProjectionLabels(ep),
		},
		Data: map[string]string{
			attributionProxyPortKey:  strconv.Itoa(port),
			attributionReadHostKey:   fmt.Sprintf("%s.%s.svc.cluster.local", egressProxyAttributionServiceName(ep), ep.Namespace),
			attributionReadPortKey:   strconv.Itoa(attributionReadPort),
			attributionServerNameKey: egressProxyMetricsServiceDNSName(ep),
			shareProxyNSKey:          ep.Namespace,
			shareProxyNamKey:         ep.Name,
		},
	}
}

func buildProxyAttributionSecret(ep *gmcv2alpha1.EgressProxy, consumerNS string, clients map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyAttributionName(ep.Namespace, ep.Name),
			Namespace: consumerNS,
			Labels:    attributionProjectionLabels(ep),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       clients[consumerNS+attributionClientCertSuffix],
			corev1.TLSPrivateKeyKey: clients[consumerNS+attributionClientKeySuffix],
			metricsCACertKey:        clients[metricsCACertKey],
		},
	}
}

// reconcileAttributionProjections writes each attributed namespace's port and
// client certificate into it, and withdraws the rest. A namespace is projected
// only once it holds a port in both the desired map and the rolled-out
// Deployment: pointing workers at a port before every replica listens on it would
// send some of their connections to a replica that refuses them. Withdrawal is
// immediate, which moves new jobs back to the unattributed port.
//
// Projections carry no owner reference, as share ConfigMaps do not, and are found
// by the ConfigMap's label for pruning; each withdrawn ConfigMap's Secret is
// blanked by name (blankProjectedSecret).
func (r *EgressProxyReconciler) reconcileAttributionProjections(ctx context.Context, ep *gmcv2alpha1.EgressProxy, ports, live map[string]int, rolledOut bool, clients map[string][]byte) error {
	labels := client.MatchingLabels{labelAttributionManaged: "true", labelShareProxyNS: ep.Namespace, labelShareProxyNam: ep.Name}
	var existing corev1.ConfigMapList
	if err := r.shareReader().List(ctx, &existing, labels); err != nil {
		return fmt.Errorf("list attribution projections: %w", err)
	}
	projected := map[string]bool{}
	for i := range existing.Items {
		projected[existing.Items[i].Namespace] = true
	}

	keep := map[string]bool{}
	for ns, port := range ports {
		if live[ns] != port || len(clients[ns+attributionClientCertSuffix]) == 0 {
			continue
		}
		if !rolledOut && !projected[ns] {
			continue
		}
		keep[ns] = true
		if err := r.applyProxyShare(ctx, buildProxyAttributionConfigMap(ep, ns, port)); err != nil {
			return fmt.Errorf("project into %q: %w", ns, err)
		}
		if err := r.applyProjectedSecret(ctx, buildProxyAttributionSecret(ep, ns, clients)); err != nil {
			return fmt.Errorf("project client certificate into %q: %w", ns, err)
		}
	}

	for i := range existing.Items {
		cm := &existing.Items[i]
		if keep[cm.Namespace] {
			continue
		}
		// Blank first: the ConfigMap is what finds this namespace again, so a
		// failed blank is retried on the next reconcile.
		if err := r.blankProjectedSecret(ctx, client.ObjectKeyFromObject(cm)); err != nil {
			return fmt.Errorf("blank the client certificate in %q: %w", cm.Namespace, err)
		}
		if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("withdraw from %q: %w", cm.Namespace, err)
		}
	}
	return nil
}

// blankProjectedSecret empties the client certificate projected under key. The
// GMC reads Secrets only in tenant namespaces and holds no delete on them, so it
// reads the one Secret by name rather than listing, and blanks it in place. A
// namespace that is no longer a tenant's cannot be read; its workers still move
// off the port when the ConfigMap goes, and the key stays where it was written.
func (r *EgressProxyReconciler) blankProjectedSecret(ctx context.Context, key client.ObjectKey) error {
	var s corev1.Secret
	if err := r.shareReader().Get(ctx, key, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if apierrors.IsForbidden(err) {
			logf.FromContext(ctx).Info("cannot read a withdrawn attribution Secret to blank it", "namespace", key.Namespace, "name", key.Name)
			return nil
		}
		return err
	}
	if len(s.Data[corev1.TLSPrivateKeyKey]) == 0 {
		return nil
	}
	s.Data = map[string][]byte{corev1.TLSCertKey: {}, corev1.TLSPrivateKeyKey: {}}
	return r.Update(ctx, &s)
}

// applyProjectedSecret creates or updates a Secret in a consumer namespace. Like
// applyProxyShare it sets no owner reference, which cannot cross namespaces.
func (r *EgressProxyReconciler) applyProjectedSecret(ctx context.Context, desired *corev1.Secret) error {
	var existing corev1.Secret
	err := r.shareReader().Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	return r.Update(ctx, &existing)
}

// namespaceAttributes reports whether any ActionsGateway in ns has opted in.
func namespaceAttributes(ctx context.Context, c client.Client, ns string) (bool, error) {
	var gateways gmcv2alpha1.ActionsGatewayList
	if err := c.List(ctx, &gateways, client.InNamespace(ns)); err != nil {
		return false, fmt.Errorf("list ActionsGateways: %w", err)
	}
	return slices.ContainsFunc(gateways.Items, func(ag gmcv2alpha1.ActionsGateway) bool {
		return ag.Spec.ProxyFailureAttribution == proxyFailureAttributionPerJob
	}), nil
}

// applyAttributionEgress lets an opted-in namespace's workload pods reach the
// attributed port range, and its AGC the read port, on the same proxy peers the
// :8080 rules name. The range is admitted on egress because which port a pool
// gave this namespace is the pool's to decide; its ingress admits each namespace
// on its own port only.
func applyAttributionEgress(workload, agc *networkingv1.NetworkPolicy, remoteProxies []remoteProxy) {
	peers := []networkingv1.NetworkPolicyPeer{{PodSelector: egressProxyPodPeerSelector()}}
	for _, rule := range remoteProxyEgressRules(remoteProxies) {
		peers = append(peers, rule.To...)
	}
	workload.Spec.Egress = append(workload.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{{
			Port:    ptr(intstr.FromInt32(attributionPortBase)),
			EndPort: ptr(int32(attributionPortBase + maxAttributedNamespaces - 1)),
		}},
		To: peers,
	})
	agc.Spec.Egress = append(agc.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(attributionReadPort))}},
		To:    peers,
	})
}
