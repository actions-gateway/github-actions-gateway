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
