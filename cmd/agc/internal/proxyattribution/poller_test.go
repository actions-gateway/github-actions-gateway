package proxyattribution

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	ns         = "team-a"
	serverName = "shared-proxy.provider.svc"
)

var t0 = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func workerPod(name, uid, set, ip string, start time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), Labels: map[string]string{provisioner.LabelRunnerSet: set}},
		Status: corev1.PodStatus{
			PodIP: ip,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(start),
			}},
		},
	}
}

func TestAddressWindows_AttributeOnlyInsideOneWindow(t *testing.T) {
	w := newAddressWindows()
	w.open(workerPod("first", "u1", "set", "10.1.0.5", t0))
	w.close("u1", t0.Add(time.Minute))
	w.open(workerPod("second", "u2", "set", "10.1.0.5", t0.Add(time.Minute+time.Second)))

	for name, tc := range map[string]struct {
		at   time.Time
		pod  string
		want bool
	}{
		"inside the first":        {t0.Add(30 * time.Second), "first", true},
		"inside the open second":  {t0.Add(5 * time.Minute), "second", true},
		"at the first's start":    {t0.Add(time.Second), "", false},
		"near the first's end":    {t0.Add(time.Minute - time.Second), "", false},
		"near the second's start": {t0.Add(time.Minute + 2*time.Second), "", false},
		"before any window":       {t0.Add(-time.Minute), "", false},
	} {
		t.Run(name, func(t *testing.T) {
			pod, ok := w.attribute("10.1.0.5", tc.at)
			assert.Equal(t, tc.want, ok)
			if tc.want {
				assert.Equal(t, tc.pod, pod)
			}
		})
	}
	w.open(workerPod("third", "u3", "set", "10.1.0.7", t0))
	w.open(workerPod("fourth", "u4", "set", "10.1.0.7", t0.Add(time.Minute)))
	_, ok := w.attribute("10.1.0.7", t0.Add(2*time.Minute))
	assert.False(t, ok, "a time inside two windows names nobody")

	_, ok = w.attribute("10.1.0.6", t0.Add(30*time.Second))
	assert.False(t, ok, "an address no own pod held names nobody")

	w.sweep(t0.Add(time.Minute + closedWindowRetention))
	_, ok = w.attribute("10.1.0.5", t0.Add(30*time.Second))
	assert.False(t, ok, "a window closed longer than the proxy's retention is dropped")
	pod, ok := w.attribute("10.1.0.5", t0.Add(5*time.Minute))
	assert.True(t, ok)
	assert.Equal(t, "second", pod, "an open window is never swept")
}

// pki is a CA with a server certificate for serverName and a client certificate
// naming ns, as the GMC's attribution bundle holds.
type pki struct {
	caPEM                 []byte
	server, wrongName     tls.Certificate
	clientCert, clientKey []byte
}

func newPKI(t *testing.T) pki {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "attribution-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	issue := func(cn string, dns []string, usage x509.ExtKeyUsage) ([]byte, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, DNSNames: dns,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		require.NoError(t, err)
		keyDER, err := x509.MarshalECPrivateKey(key)
		require.NoError(t, err)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}
	srvCert, srvKey := issue("server", []string{serverName}, x509.ExtKeyUsageServerAuth)
	server, err := tls.X509KeyPair(srvCert, srvKey)
	require.NoError(t, err)
	wrongCert, wrongKey := issue("server", []string{"other-proxy.provider.svc"}, x509.ExtKeyUsageServerAuth)
	wrongName, err := tls.X509KeyPair(wrongCert, wrongKey)
	require.NoError(t, err)
	clientCert, clientKey := issue(ns, nil, x509.ExtKeyUsageClientAuth)
	return pki{
		caPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		server: server, wrongName: wrongName, clientCert: clientCert, clientKey: clientKey,
	}
}

// fakeReplica serves GET /v1/failures as the proxy does: the records after the
// requested sequence, under an epoch, to a client certificate naming ns.
type fakeReplica struct {
	epoch   atomic.Value
	records atomic.Value
	hits    atomic.Int32
}

func (f *fakeReplica) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	if len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != ns {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	var out []record
	for _, rec := range f.records.Load().([]record) {
		if rec.Seq > after {
			out = append(out, rec)
		}
	}
	_ = json.NewEncoder(w).Encode(response{Epoch: f.epoch.Load().(string), Records: out})
}

func startReplica(t *testing.T, p pki, epoch string, recs ...record) (*fakeReplica, string) {
	t.Helper()
	return startReplicaServing(t, p.server, p, epoch, recs...)
}

// startReplicaServing serves cert while trusting clients of p's CA.
func startReplicaServing(t *testing.T, cert tls.Certificate, p pki, epoch string, recs ...record) (*fakeReplica, string) {
	t.Helper()
	f := &fakeReplica{}
	f.epoch.Store(epoch)
	f.records.Store(recs)
	clients := x509.NewCertPool()
	require.True(t, clients.AppendCertsFromPEM(p.caPEM))
	srv := httptest.NewUnstartedServer(f)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clients}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return f, srv.Listener.Addr().String()
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v2alpha1.AddToScheme(s))
	return s
}

// harness wires a Poller for gateway "gw" in ns to one replica of provider/shared.
type harness struct {
	p   *Poller
	c   client.Client
	reg *prometheus.Registry
	gw  *v2alpha1.ActionsGateway
}

func newHarness(t *testing.T, p pki, replicaAddr string) *harness {
	t.Helper()
	gw := &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}}
	gw.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: "shared", Namespace: "provider"}
	gw.Spec.ProxyFailureAttribution = optInPerJob
	own := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: ns}}
	own.Spec.GatewayRef.Name = "gw"
	sibling := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "sibling", Namespace: ns}}
	sibling.Spec.GatewayRef.Name = "other-gw"
	name := "proxy-attribution-provider-shared"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: map[string]string{
		readHostKey: "shared-proxy-attr.provider.svc.cluster.local", readPortKey: "8444", serverNameKey: serverName,
	}}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: map[string][]byte{
		corev1.TLSCertKey: p.clientCert, corev1.TLSPrivateKeyKey: p.clientKey, caCertKey: p.caPEM,
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(gw, own, sibling, cm, sec).Build()
	reg := prometheus.NewRegistry()
	poller := NewPoller(c, c, nil, ns, "gw", NewMetrics(reg), slog.New(slog.DiscardHandler))
	poller.resolve = func(_ context.Context, host string) ([]string, error) {
		require.Equal(t, "shared-proxy-attr.provider.svc.cluster.local", host)
		return []string{"10.2.0.7"}, nil
	}
	poller.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		require.Equal(t, "10.2.0.7:8444", addr)
		return (&net.Dialer{}).DialContext(ctx, network, replicaAddr)
	}
	poller.now = func() time.Time { return t0.Add(2 * time.Minute) }
	return &harness{p: poller, c: c, reg: reg, gw: gw}
}

func (h *harness) records(outcome, result string) float64 {
	return testutil.ToFloat64(h.p.Metrics.Records.WithLabelValues(ns, outcome, result))
}

// Each record is joined to the own pod that held its address at the time, and a
// sibling gateway's worker's record is discarded.
func TestPoller_AttributesRecordsToItsOwnPods(t *testing.T) {
	p := newPKI(t)
	_, addr := startReplica(t, p, "e1",
		record{Seq: 1, Time: t0.Add(30 * time.Second), SourceIP: "10.1.0.5", Outcome: "denied", Host: "evil.example", Port: "443"},
		record{Seq: 2, Time: t0.Add(40 * time.Second), SourceIP: "10.1.0.6", Outcome: "dial-failed", DialClass: "timeout", Host: "api.github.com", Port: "443"},
	)
	h := newHarness(t, p, addr)
	ctx := context.Background()
	h.p.onPod(ctx, workerPod("own-1", "u1", "own", "10.1.0.5", t0))
	h.p.onPod(ctx, workerPod("sib-1", "u2", "sibling", "10.1.0.6", t0))

	require.NoError(t, h.p.poll(ctx))
	got := h.p.Lookup("own-1")
	require.Len(t, got, 1)
	assert.Equal(t, Failure{Time: t0.Add(30 * time.Second), Outcome: "denied", Host: "evil.example", Port: "443"}, got[0])
	assert.Empty(t, h.p.Lookup("sib-1"), "a sibling gateway's worker is not this AGC's to attribute")
	assert.Equal(t, 1.0, h.records("denied", "attributed"))
	assert.Equal(t, 1.0, h.records("dial-failed", "discarded"))
	assert.Equal(t, 1.0, testutil.ToFloat64(h.p.Metrics.Reads.WithLabelValues(ns, "ok")))
}

// The cursor moves past what was read, and a changed epoch rereads from the start.
func TestPoller_ResumesFromItsCursorAndResetsOnANewEpoch(t *testing.T) {
	p := newPKI(t)
	rec := func(seq uint64) record {
		return record{Seq: seq, Time: t0.Add(30 * time.Second), SourceIP: "10.1.0.5", Outcome: "denied", Host: "h", Port: "443"}
	}
	f, addr := startReplica(t, p, "e1", rec(1), rec(2))
	h := newHarness(t, p, addr)
	ctx := context.Background()
	h.p.onPod(ctx, workerPod("own-1", "u1", "own", "10.1.0.5", t0))

	require.NoError(t, h.p.poll(ctx))
	require.NoError(t, h.p.poll(ctx))
	assert.Len(t, h.p.Lookup("own-1"), 2, "a second poll reads nothing already read")

	f.epoch.Store("e2")
	f.records.Store([]record{rec(1)})
	require.NoError(t, h.p.poll(ctx))
	assert.Len(t, h.p.Lookup("own-1"), 3, "a restarted replica's sequence 1 is a new record")
}

// A replica the AGC cannot verify, as during the CA roll, is a failed read that
// keeps its cursor rather than an error. Each server accepts the AGC's client
// certificate, so the refusal is the AGC's own check.
func TestPoller_RefusesAnUnverifiedReplicaAndKeepsItsCursor(t *testing.T) {
	p := newPKI(t)
	for name, server := range map[string]tls.Certificate{
		"another CA": newPKI(t).server,
		"wrong name": p.wrongName,
	} {
		t.Run(name, func(t *testing.T) {
			f, addr := startReplicaServing(t, server, p, "e1", record{Seq: 8, Time: t0.Add(30 * time.Second), SourceIP: "10.1.0.5", Outcome: "denied"})
			h := newHarness(t, p, addr)
			key := replica{pool: pool{"provider", "shared"}, addr: "10.2.0.7"}
			h.p.cursors[key] = cursor{epoch: "e1", after: 7}

			require.NoError(t, h.p.poll(context.Background()))
			assert.Zero(t, f.hits.Load(), "no request reaches a replica the AGC did not verify")
			assert.Equal(t, 1.0, testutil.ToFloat64(h.p.Metrics.Reads.WithLabelValues(ns, "failed")))
			assert.Equal(t, cursor{epoch: "e1", after: 7}, h.p.cursors[key])
		})
	}
}

// A gateway that is not opted in reads nothing and drops what it held.
func TestPoller_ReadsNothingWhileTheGatewayIsOptedOut(t *testing.T) {
	p := newPKI(t)
	f, addr := startReplica(t, p, "e1", record{Seq: 1, Time: t0.Add(30 * time.Second), SourceIP: "10.1.0.5", Outcome: "denied"})
	h := newHarness(t, p, addr)
	ctx := context.Background()
	h.p.onPod(ctx, workerPod("own-1", "u1", "own", "10.1.0.5", t0))
	require.NoError(t, h.p.poll(ctx))
	require.Len(t, h.p.Lookup("own-1"), 1)

	h.gw.Spec.ProxyFailureAttribution = "Disabled"
	require.NoError(t, h.c.Update(ctx, h.gw))
	hits := f.hits.Load()
	require.NoError(t, h.p.poll(ctx))
	assert.Equal(t, hits, f.hits.Load(), "no replica is read")
	assert.Empty(t, h.p.Lookup("own-1"), "what was held is dropped")
	assert.Empty(t, h.p.cursors)

	h.p.windows.close("u1", t0.Add(time.Minute))
	h.p.now = func() time.Time { return t0.Add(time.Minute + closedWindowRetention) }
	require.NoError(t, h.p.poll(ctx))
	assert.Empty(t, h.p.windows.byIP, "closed windows are swept while opted out too")
}
