package tracing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type countingCollector struct {
	coltrace.UnimplementedTraceServiceServer
	exports atomic.Int32
}

func (c *countingCollector) Export(context.Context, *coltrace.ExportTraceServiceRequest) (*coltrace.ExportTraceServiceResponse, error) {
	c.exports.Add(1)
	return &coltrace.ExportTraceServiceResponse{}, nil
}

// startPrivateCACollector serves OTLP over TLS with a certificate chaining to a CA no
// system root holds, returning the CA's PEM and the collector's address.
func startPrivateCACollector(t *testing.T) (*countingCollector, []byte, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tracing-test-ca"},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	})))
	collector := &countingCollector{}
	coltrace.RegisterTraceServiceServer(server, collector)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return collector, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), net.JoinHostPort("localhost", port)
}

// exportOneSpan runs one span through Init's provider and flushes it.
func exportOneSpan(t *testing.T, endpoint, certificate string) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://"+endpoint)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", certificate)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "3000")
	t.Setenv("OTEL_SDK_DISABLED", "")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shutdown, enabled, err := Init(ctx, "test", nil)
	if err != nil {
		t.Fatalf("Init must not fail on the bundle file, or a lost bundle would stop the AGC: %v", err)
	}
	if !enabled {
		t.Fatal("expected tracing enabled")
	}
	_, span := otel.Tracer(InstrumentationName).Start(ctx, "probe")
	span.End()
	_ = shutdown(ctx)
}

// spec.tracing.caBundleRef mounts optionally, so the AGC can start with the bundle missing
// or unparseable. That trades no security property only if the exporter then keeps
// verifying, which is the SDK's fallback rather than this repo's code. Pinned against
// otlptracegrpc as vendored, since a fallback to no verification would pass every other test.
func TestInit_UnusableCertificateFileKeepsVerifying(t *testing.T) {
	garbage := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{
		"missing file": filepath.Join(t.TempDir(), "absent", "ca.crt"),
		"unparseable":  garbage,
	} {
		t.Run(name, func(t *testing.T) {
			collector, _, endpoint := startPrivateCACollector(t)
			exportOneSpan(t, endpoint, file)
			if got := collector.exports.Load(); got != 0 {
				t.Fatalf("collector received %d exports over a connection the AGC had no CA to verify", got)
			}
		})
	}

	// The control: the same collector and span deliver once the file names the CA, so the
	// zeros above are the verification and not a broken probe.
	t.Run("the CA", func(t *testing.T) {
		collector, caPEM, endpoint := startPrivateCACollector(t)
		file := filepath.Join(t.TempDir(), "ca.crt")
		if err := os.WriteFile(file, caPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		exportOneSpan(t, endpoint, file)
		if collector.exports.Load() == 0 {
			t.Fatal("collector received nothing with the right CA")
		}
	})
}
