package controller

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// The certificate primitives and mount layout shared by every GMC-issued PKI: the
// v1 ActionsGateway's proxy and metrics bundles, the v2 ActionsGateway's per-gateway
// metrics bundle, and the standalone EgressProxy's proxy and metrics bundles. The
// per-version pieces are only the Secret *names* (v1 fixed, v2 derived per CR) and
// the SAN lists, which stay with their version's builder.

const (
	// proxyTLSVolumeName / proxyTLSMountPath project the proxy's own TLS cert+key
	// into the proxy pod.
	proxyTLSVolumeName = "proxy-tls"
	proxyTLSMountPath  = "/etc/actions-gateway/proxy-tls"

	// proxyCACertVolumeName / proxyCACertMountPath project the proxy's public cert
	// (never its key) into the AGC pod, which pins it for outbound CONNECTs.
	proxyCACertVolumeName = "proxy-ca"
	proxyCACertMountPath  = "/etc/actions-gateway/proxy-ca"

	// proxyCertRenewBefore is the lead time before cert expiry at which the GMC
	// re-issues the cert. 30 days gives operators ample time to notice and restart pods.
	proxyCertRenewBefore = 30 * 24 * time.Hour

	// metricsTLSVolumeName / metricsTLSMountPath project the metrics mTLS server
	// bundle (ca.crt + tls.crt + tls.key) into the AGC and proxy pods.
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

// parseCertPEM decodes the first PEM certificate block and returns the parsed cert.
func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}
