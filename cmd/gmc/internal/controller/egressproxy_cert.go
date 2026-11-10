package controller

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// generateEgressProxyCert generates a self-signed 2048-bit RSA TLS certificate for
// a v2 EgressProxy's <ep>-proxy Service. It is the v2 analogue of generateProxyCert
// (cmd/gmc/internal/controller/cert.go): same key size, lifetime, key/ext-key usage,
// and self-signed structure, but keyed on the per-EgressProxy derived Service name
// (proxyResourceName) rather than v1's fixed, one-per-namespace proxy Service. The
// SANs list every in-cluster DNS name for that Service so a consumer can pin to this
// cert without a CA hierarchy. Returns (certPEM, keyPEM, error).
func generateEgressProxyCert(namespace, svcName string) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	dnsNames := egressProxyCertDNSNames(namespace, svcName)

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsNames[len(dnsNames)-1]},
		DNSNames:     dnsNames,
		// Small backdate absorbs clock skew between the GMC and the proxy pods.
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	var certBuf bytes.Buffer
	if err := pem.Encode(&certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return nil, nil, fmt.Errorf("encode cert PEM: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	var keyBuf bytes.Buffer
	if err := pem.Encode(&keyBuf, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
		return nil, nil, fmt.Errorf("encode key PEM: %w", err)
	}

	return certBuf.Bytes(), keyBuf.Bytes(), nil
}

// egressProxyCertDNSNames lists every in-cluster DNS name of the <svcName> Service,
// fully qualified name last.
func egressProxyCertDNSNames(namespace, svcName string) []string {
	return []string{
		svcName,
		fmt.Sprintf("%s.%s", svcName, namespace),
		fmt.Sprintf("%s.%s.svc", svcName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", svcName, namespace),
	}
}

// parseIssuedEgressProxyCert returns the proxy TLS Secret's certificate when the
// Secret holds exactly the shape generateEgressProxyCert issues: one PEM
// certificate and nothing else, not a CA, valid for exactly the Service's DNS names
// and no other SAN, and paired with tls.key. Consumers install the certificate as a
// trust root (Q1324), and anyone who can write the Secret could otherwise append a
// CA or name a GitHub host and have both projected into every consumer namespace.
func parseIssuedEgressProxyCert(namespace, svcName string, data map[string][]byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(data[corev1.TLSCertKey])
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no PEM certificate block found")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("%s holds data after its first certificate", corev1.TLSCertKey)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	if !cert.BasicConstraintsValid || cert.IsCA {
		return nil, fmt.Errorf("certificate is not a basic-constraints leaf")
	}
	if !slices.Equal(cert.DNSNames, egressProxyCertDNSNames(namespace, svcName)) ||
		len(cert.IPAddresses) != 0 || len(cert.URIs) != 0 || len(cert.EmailAddresses) != 0 {
		return nil, fmt.Errorf("certificate SANs are not the proxy Service's DNS names")
	}
	if _, err := tls.X509KeyPair(data[corev1.TLSCertKey], data[corev1.TLSPrivateKeyKey]); err != nil {
		return nil, fmt.Errorf("certificate does not match %s: %w", corev1.TLSPrivateKeyKey, err)
	}
	return cert, nil
}
