package runnerimage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReference(t *testing.T) {
	const digest = "sha256:f2387135856decdecbf780a2bfbc9debe9c2dffd742f150302444b3775474681"
	for _, tc := range []struct {
		in   string
		want Reference
	}{
		{"ghcr.io/actions/actions-runner:2.335.1", Reference{"ghcr.io", "actions/actions-runner", "2.335.1", ""}},
		{"ghcr.io/actions/actions-runner:2.335.1@" + digest, Reference{"ghcr.io", "actions/actions-runner", "2.335.1", digest}},
		{"ghcr.io/actions/actions-runner@" + digest, Reference{"ghcr.io", "actions/actions-runner", "", digest}},
		{"registry.example.com:5000/runner:2.329.0", Reference{"registry.example.com:5000", "runner", "2.329.0", ""}},
		{"localhost/runner", Reference{"localhost", "runner", "latest", ""}},
		{"runner", Reference{"docker.io", "library/runner", "latest", ""}},
		{"acme/runner:v3-cuda", Reference{"docker.io", "acme/runner", "v3-cuda", ""}},
		{"docker.io/acme/runner", Reference{"docker.io", "acme/runner", "latest", ""}},
	} {
		got, err := ParseReference(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	for _, in := range []string{"", "ghcr.io/", "ghcr.io/x@sha256:short", "ghcr.io/x@md5:abc"} {
		_, err := ParseReference(in)
		assert.Error(t, err, in)
	}
	assert.Equal(t, "registry-1.docker.io", Reference{Registry: "docker.io"}.apiHost())
	assert.Equal(t, "ghcr.io", Reference{Registry: "ghcr.io"}.apiHost())
	assert.Equal(t, "ghcr.io/a/b:t@"+digest, Reference{"ghcr.io", "a/b", "t", digest}.String())
}

func TestParseDockerConfigJSON(t *testing.T) {
	creds := Credentials{}
	require.NoError(t, creds.ParseDockerConfigJSON([]byte(`{"auths":{
		"ghcr.io":{"auth":"Ym90OnMzY3JldA=="},
		"https://index.docker.io/v1/":{"username":"hub","password":"pw"},
		"registry.example.com:5000/team":{"auth":"dTpw"}}}`)))
	assert.Equal(t, Credential{"bot", "s3cret"}, creds["ghcr.io"])
	assert.Equal(t, Credential{"hub", "pw"}, creds["docker.io"], "the legacy Hub key maps to the registry a reference names")
	assert.Equal(t, Credential{"u", "p"}, creds["registry.example.com:5000"])
	_, ok := creds.lookup("quay.io")
	assert.False(t, ok)

	assert.Error(t, creds.ParseDockerConfigJSON([]byte(`{"auths":{"x":{"auth":"!!"}}}`)))
	assert.Error(t, creds.ParseDockerConfigJSON([]byte(`{"auths":{"x":{"auth":"bm9jb2xvbg=="}}}`)), "auth without a colon")
	assert.Error(t, creds.ParseDockerConfigJSON([]byte(`not json`)))
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull,push"`)
	assert.Equal(t, "Bearer", scheme)
	assert.Equal(t, "https://ghcr.io/token", params["realm"])
	assert.Equal(t, "ghcr.io", params["service"])
	assert.Equal(t, "repository:a/b:pull,push", params["scope"], "a comma inside quotes does not split")

	scheme, params = parseChallenge(`Basic realm="Registry"`)
	assert.Equal(t, "Basic", scheme)
	assert.Equal(t, "Registry", params["realm"])
}
