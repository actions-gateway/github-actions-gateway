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

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
				Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(int32(ports[ns])))}},
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
// returns the CA it holds. It regenerates when either Secret is missing, the two
// disagree on the CA, the clients Secret names a different namespace set, or the
// server cert is near expiry. No namespaces deletes both Secrets.
func (r *EgressProxyReconciler) ensureAttributionCerts(ctx context.Context, ep *gmcv2alpha1.EgressProxy, namespaces []string) ([]byte, error) {
	if len(namespaces) == 0 {
		for _, name := range []string{egressProxyAttributionTLSSecretName(ep), egressProxyAttributionClientsSecretName(ep)} {
			s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ep.Namespace, Name: name}}
			if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
		}
		return nil, nil
	}

	var server, clients corev1.Secret
	serverErr := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: egressProxyAttributionTLSSecretName(ep)}, &server)
	if serverErr != nil && !apierrors.IsNotFound(serverErr) {
		return nil, serverErr
	}
	clientsErr := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: egressProxyAttributionClientsSecretName(ep)}, &clients)
	if clientsErr != nil && !apierrors.IsNotFound(clientsErr) {
		return nil, clientsErr
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
						return ca, nil
					}
				}
			}
		}
	}

	bundle, err := generateAttributionCerts(ep.Namespace, proxyResourceName(ep), namespaces)
	if err != nil {
		return nil, fmt.Errorf("generate attribution certs: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ep, buildEgressProxyAttributionTLSSecret(ep, bundle)); err != nil {
		return nil, fmt.Errorf("attribution server Secret: %w", err)
	}
	if err := r.applyOwnedSecret(ctx, ep, buildEgressProxyAttributionClientsSecret(ep, bundle)); err != nil {
		return nil, fmt.Errorf("attribution clients Secret: %w", err)
	}
	r.recordEvent(ep, corev1.EventTypeNormal, "AttributionCertificateIssued", "EnsureAttributionCerts",
		"issued failure-attribution mTLS certificates for %d namespace(s) (%s)", len(namespaces), reason)
	return bundle.caPEM, nil
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
// against the live Deployment, and keeps the bundle in step. It returns the port
// map and the CA the Deployment must carry; both are empty when nothing is
// attributed.
func (r *EgressProxyReconciler) reconcileAttribution(ctx context.Context, ep *gmcv2alpha1.EgressProxy) (map[string]int, []byte, error) {
	namespaces, err := attributedNamespacesFor(ctx, r.Client, ep)
	if err != nil {
		return nil, nil, err
	}
	var live appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: proxyResourceName(ep)}, &live); err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("read proxy Deployment: %w", err)
	}
	ports := allocateAttributionPorts(namespaces, liveAttributionPorts(&live))
	caPEM, err := r.ensureAttributionCerts(ctx, ep, namespaces)
	if err != nil {
		return nil, nil, err
	}
	return ports, caPEM, nil
}
