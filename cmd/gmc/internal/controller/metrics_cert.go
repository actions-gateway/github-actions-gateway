package controller

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// The v1 ActionsGateway's fixed per-namespace metrics Secret names and its
// proxy+AGC SAN list. v2 derives both per CR (metricsTLSSecretNameV2 /
// metricsServerSANsV2, and the EgressProxy's own pair). The version-neutral
// bundle type, mount layout and signing primitives are in shared_pki.go.
const (
	// metricsTLSSecretName holds the server bundle (ca.crt + tls.crt + tls.key)
	// mounted read-only into both the AGC and proxy pods. They serve /metrics
	// over mTLS using tls.crt/tls.key and verify scraper client certs against
	// ca.crt.
	metricsTLSSecretName = "actions-gateway-metrics-tls"
	// metricsClientSecretName holds the scraper bundle (ca.crt + tls.crt +
	// tls.key). It is published for the monitoring stack to present when
	// scraping; it is never mounted into AGC/proxy pods.
	metricsClientSecretName = "actions-gateway-metrics-client"
)

// generateMetricsCerts builds a self-signed CA and signs a server certificate
// (SANs covering the proxy and AGC Service DNS names; ServerAuth) and a client
// certificate (ClientAuth) for the metrics scraper. The CA private key is not
// returned or persisted — the whole bundle is regenerated together on renewal,
// matching generateProxyCert's no-persisted-CA model.
func generateMetricsCerts(ag *gmcv1alpha1.ActionsGateway) (*metricsCertBundle, error) {
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
		Subject:               pkix.Name{CommonName: fmt.Sprintf("actions-gateway-metrics-ca.%s", ag.Namespace)},
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
	caPEM := encodeCertPEM(caDER)

	serverCertPEM, serverKeyPEM, err := signLeaf(caCert, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: fmt.Sprintf("%s.%s.svc", proxyServiceName, ag.Namespace)},
		DNSNames:    metricsServerSANs(ag),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("sign server cert: %w", err)
	}

	clientCertPEM, clientKeyPEM, err := signLeaf(caCert, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: metricsScraperCN},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("sign client cert: %w", err)
	}

	return &metricsCertBundle{
		caPEM:         caPEM,
		serverCertPEM: serverCertPEM,
		serverKeyPEM:  serverKeyPEM,
		clientCertPEM: clientCertPEM,
		clientKeyPEM:  clientKeyPEM,
	}, nil
}

// metricsServerSANs lists the in-cluster DNS names the metrics server cert must
// be valid for: the proxy Service and the AGC (so a future AGC metrics Service
// scrape verifies without insecureSkipVerify). Both short and FQDN forms are
// included so a scraper can use either.
func metricsServerSANs(ag *gmcv1alpha1.ActionsGateway) []string {
	var sans []string
	for _, svc := range []string{proxyServiceName, agcAppName} {
		sans = append(sans,
			svc,
			fmt.Sprintf("%s.%s", svc, ag.Namespace),
			fmt.Sprintf("%s.%s.svc", svc, ag.Namespace),
			fmt.Sprintf("%s.%s.svc.cluster.local", svc, ag.Namespace),
		)
	}
	return sans
}
