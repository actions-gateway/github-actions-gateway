package controller

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateEgressProxyCert(t *testing.T) {
	certPEM, keyPEM, err := generateEgressProxyCert("team-a", "shared-proxy")
	require.NoError(t, err)
	require.NotEmpty(t, certPEM)
	require.NotEmpty(t, keyPEM)

	cert, err := parseCertPEM(certPEM)
	require.NoError(t, err)

	// SANs cover every in-cluster DNS name for the derived <ep>-proxy Service so a
	// consumer can pin to this cert without a CA hierarchy.
	assert.Contains(t, cert.DNSNames, "shared-proxy")
	assert.Contains(t, cert.DNSNames, "shared-proxy.team-a")
	assert.Contains(t, cert.DNSNames, "shared-proxy.team-a.svc")
	assert.Contains(t, cert.DNSNames, "shared-proxy.team-a.svc.cluster.local")

	// Server-auth EKU and a forward-dated expiry well beyond the renew window.
	assert.Contains(t, cert.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	assert.True(t, cert.NotAfter.After(time.Now().Add(300*24*time.Hour)),
		"freshly issued cert should not be near expiry")
}
