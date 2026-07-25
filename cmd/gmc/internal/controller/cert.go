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

// proxyTLSSecretName is the v1 ActionsGateway's fixed per-namespace proxy TLS
// Secret. v2 derives the name from the EgressProxy (egressProxyTLSSecretName).
// The version-neutral mount layout and certificate primitives are in shared_pki.go.
const proxyTLSSecretName = "actions-gateway-proxy-tls"

// generateProxyCert generates a self-signed 2048-bit RSA TLS certificate for the
// proxy Service. The certificate lists all in-cluster DNS names for the Service as
// SANs so the AGC can pin to this specific cert without a CA hierarchy.
// Returns (certPEM, keyPEM, error).
func generateProxyCert(ag *gmcv1alpha1.ActionsGateway) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", proxyServiceName, ag.Namespace)
	dnsNames := []string{
		proxyServiceName,
		fmt.Sprintf("%s.%s", proxyServiceName, ag.Namespace),
		fmt.Sprintf("%s.%s.svc", proxyServiceName, ag.Namespace),
		fqdn,
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: fqdn},
		DNSNames:     dnsNames,
		// Small backdate absorbs clock skew between the GMC and the AGC/proxy pods.
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
