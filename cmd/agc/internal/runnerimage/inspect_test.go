package runnerimage

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnerimage/runnerimagetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectReadsTopmostLayer(t *testing.T) {
	f := runnerimagetest.New(t)
	lower := f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.300.0")))
	upper := f.GzipLayer(runnerimagetest.E("etc/motd", "hi"), runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1")))
	m := f.Manifest("acme/runner", "linux", "amd64", lower, upper)
	f.Tag("acme/runner", "custom", m.Digest)

	got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":custom"), nil)
	require.NoError(t, err)
	assert.True(t, got.Found)
	assert.Equal(t, "2.335.1", got.Version, "the topmost copy is what the container sees")
	assert.Equal(t, m.Digest, got.Digest)
	assert.Empty(t, got.Platform, "a single-platform manifest names no platform")
	assert.Equal(t, int64(1), f.BlobGets(), "the scan stops at the first layer that carries the file")
}

func TestInspectStreamsLowerLayerWhenUpperLacksFile(t *testing.T) {
	f := runnerimagetest.New(t)
	lower := f.TarLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.329.0")))
	upper := f.GzipLayer(runnerimagetest.E("etc/motd", "hi"))
	f.Tag("acme/runner", "v", f.Manifest("acme/runner", "linux", "amd64", lower, upper).Digest)

	got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":v"), nil)
	require.NoError(t, err)
	assert.Equal(t, "2.329.0", got.Version)
	assert.Equal(t, int64(2), f.BlobGets())
}

func TestInspectHonoursWhiteouts(t *testing.T) {
	f := runnerimagetest.New(t)
	lower := f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.300.0")))

	t.Run("file whiteout", func(t *testing.T) {
		upper := f.GzipLayer(runnerimagetest.E("home/runner/bin/.wh.Runner.Listener.deps.json", ""))
		f.Tag("acme/runner", "wh", f.Manifest("acme/runner", "linux", "amd64", lower, upper).Digest)
		got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":wh"), nil)
		require.NoError(t, err)
		assert.False(t, got.Found, "a deleted file is not the running version")
	})
	t.Run("opaque directory whiteout", func(t *testing.T) {
		upper := f.GzipLayer(runnerimagetest.E("home/runner/.wh..wh..opq", ""))
		f.Tag("acme/runner", "opq", f.Manifest("acme/runner", "linux", "amd64", lower, upper).Digest)
		got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":opq"), nil)
		require.NoError(t, err)
		assert.False(t, got.Found)
	})
}

func TestInspectNotRunnerDerived(t *testing.T) {
	f := runnerimagetest.New(t)
	f.Tag("acme/runner", "latest", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E("usr/bin/tool", "x"))).Digest)

	got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ""), nil)
	require.NoError(t, err)
	assert.False(t, got.Found)
	assert.Empty(t, got.Version)
}

func TestInspectDigestReference(t *testing.T) {
	f := runnerimagetest.New(t)
	m := f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1"))))

	got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", "@"+m.Digest), nil)
	require.NoError(t, err)
	assert.Equal(t, "2.335.1", got.Version)
	assert.Equal(t, m.Digest, got.Digest)
}

func TestInspectIndexPrefersLinuxAmd64(t *testing.T) {
	f := runnerimagetest.New(t)
	arm := f.Manifest("acme/runner", "linux", "arm64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.330.0"))))
	amd := f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1"))))
	attest := f.Manifest("acme/runner", "unknown", "unknown", f.GzipLayer(runnerimagetest.E("attestation", "{}")))
	idx := f.Index("acme/runner", arm, amd, attest)
	f.Tag("acme/runner", "multi", idx)

	got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":multi"), nil)
	require.NoError(t, err)
	assert.Equal(t, "2.335.1", got.Version)
	assert.Equal(t, "linux/amd64", got.Platform)
	assert.Equal(t, idx, got.Digest, "the digest is the index's, which is what a pin names")

	t.Run("falls back to the first linux entry", func(t *testing.T) {
		f.Tag("acme/runner", "arm-only", f.Index("acme/runner", attest, arm))
		got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":arm-only"), nil)
		require.NoError(t, err)
		assert.Equal(t, "2.330.0", got.Version)
		assert.Equal(t, "linux/arm64", got.Platform)
	})
	t.Run("no linux entry is an error", func(t *testing.T) {
		f.Tag("acme/runner", "attest-only", f.Index("acme/runner", attest))
		_, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":attest-only"), nil)
		require.ErrorContains(t, err, "no linux platform")
	})
}

func TestInspectBearerAuth(t *testing.T) {
	f := runnerimagetest.New(t)
	f.Auth = runnerimagetest.AuthBearer
	f.Tag("acme/runner", "v", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1")))).Digest)

	t.Run("anonymous token exchange", func(t *testing.T) {
		got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":v"), nil)
		require.NoError(t, err)
		assert.Equal(t, "2.335.1", got.Version)
	})
	t.Run("login rides on the token exchange", func(t *testing.T) {
		f.Login = runnerimagetest.Credential{Username: "bot", Password: "s3cret"}
		creds := Credentials{}
		require.NoError(t, creds.ParseDockerConfigJSON(runnerimagetest.DockerConfigJSON(f.Host(), f.Login)))
		got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":v"), creds)
		require.NoError(t, err)
		assert.Equal(t, "2.335.1", got.Version)

		_, err = Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":v"), nil)
		require.ErrorContains(t, err, "token exchange")
		assert.ErrorContains(t, err, "anonymous")
	})
}

func TestInspectBasicAuth(t *testing.T) {
	f := runnerimagetest.New(t)
	f.Auth = runnerimagetest.AuthBasic
	f.Login = runnerimagetest.Credential{Username: "bot", Password: "s3cret"}
	f.Tag("acme/runner", "v", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1")))).Digest)

	creds := Credentials{}
	require.NoError(t, creds.ParseDockerConfigJSON(runnerimagetest.DockerConfigJSON(f.Host(), f.Login)))
	got, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":v"), creds)
	require.NoError(t, err)
	assert.Equal(t, "2.335.1", got.Version)

	_, err = Inspect(context.Background(), f.Client(), f.Image("acme/runner", ":v"), nil)
	require.ErrorContains(t, err, "names no imagePullSecret")
}

func TestInspectErrors(t *testing.T) {
	f := runnerimagetest.New(t)
	f.Tag("acme/runner", "zstd", f.Manifest("acme/runner", "linux", "amd64", runnerimagetest.Descriptor{MediaType: runnerimagetest.MediaOCILayerZstd, Digest: f.Put([]byte("zz")), Size: 2}).Digest)
	f.Tag("acme/runner", "two", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(
		runnerimagetest.E("a/bin/Runner.Listener.deps.json", runnerimagetest.DepsJSON("2.335.1")),
		runnerimagetest.E("b/bin/Runner.Listener.deps.json", runnerimagetest.DepsJSON("2.300.0")))).Digest)
	f.Tag("acme/runner", "bad-deps", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, `{"targets":{}}`))).Digest)

	for _, tc := range []struct{ ref, want string }{
		{":zstd", "zstd-compressed layers are not supported"},
		{":two", "runner installs of different versions"},
		{":bad-deps", "names no Runner.Listener/"},
		{":missing", "HTTP 404"},
	} {
		_, err := Inspect(context.Background(), f.Client(), f.Image("acme/runner", tc.ref), nil)
		require.ErrorContains(t, err, tc.want, tc.ref)
	}
}

func TestInspectUnreachableRegistry(t *testing.T) {
	f := runnerimagetest.New(t)
	hc := f.Client()
	f.Close()
	_, err := Inspect(context.Background(), hc, f.Image("acme/runner", ":v"), nil)
	require.Error(t, err)
}
