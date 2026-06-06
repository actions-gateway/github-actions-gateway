/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

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

	metricsTLSVolumeName = "metrics-tls"
	metricsTLSMountPath  = "/etc/actions-gateway/metrics-tls"

	// metricsCACertKey is the Secret data key under which the metrics CA cert is
	// stored alongside the standard tls.crt/tls.key of a kubernetes.io/tls Secret.
	metricsCACertKey = "ca.crt"

	// metricsScraperCN is the Common Name on the scraper client certificate.
	metricsScraperCN = "actions-gateway-metrics-scraper"

	// metricsCertRenewBefore mirrors proxyCertRenewBefore: the GMC re-issues the
	// whole bundle once the server cert is within this window of expiry.
	metricsCertRenewBefore = 30 * 24 * time.Hour
)

// metricsCertBundle is the full per-tenant metrics PKI: one CA signing a server
// leaf (for the AGC/proxy metrics listeners) and a client leaf (for the scraper).
type metricsCertBundle struct {
	caPEM         []byte
	serverCertPEM []byte
	serverKeyPEM  []byte
	clientCertPEM []byte
	clientKeyPEM  []byte
}

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

// signLeaf fills in the boilerplate fields on tmpl, signs it with the CA, and
// returns the leaf cert + key as PEM. The key is RSA-2048, PKCS#8 encoded.
func signLeaf(caCert *x509.Certificate, caKey *rsa.PrivateKey, tmpl *x509.Certificate) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-1 * time.Minute)
	tmpl.NotAfter = time.Now().Add(365 * 24 * time.Hour)
	tmpl.BasicConstraintsValid = true

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	return encodeCertPEM(der), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// randSerial returns a random 128-bit certificate serial number.
func randSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return serial, nil
}

// encodeCertPEM PEM-encodes a DER certificate.
func encodeCertPEM(der []byte) []byte {
	var buf bytes.Buffer
	// pem.Encode to a bytes.Buffer never errors.
	_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	return buf.Bytes()
}
