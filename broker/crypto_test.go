package broker_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/karlkfi/github-actions-gateway/broker"
)

// cryptoFixture mirrors the shape of testdata/crypto_fixture.json.
type cryptoFixture struct {
	KeyBase64     string          `json:"key_base64"`
	EncryptedBody string          `json:"encrypted_body"`
	Plaintext     json.RawMessage `json:"plaintext"`
}

// loadFixture reads testdata/crypto_fixture.json. Go tests run with the
// working directory set to the package directory (broker/), so the path is
// relative to that.
func loadFixture(t *testing.T) cryptoFixture {
	t.Helper()
	data, err := os.ReadFile("../testdata/crypto_fixture.json")
	require.NoError(t, err, "testdata/crypto_fixture.json must exist")
	var f cryptoFixture
	require.NoError(t, json.Unmarshal(data, &f))
	return f
}

func TestDecryptMessageBody_HappyPath(t *testing.T) {
	f := loadFixture(t)
	key, err := base64.StdEncoding.DecodeString(f.KeyBase64)
	require.NoError(t, err)

	plaintext, err := broker.DecryptMessageBody(f.EncryptedBody, key)
	require.NoError(t, err)

	// The decrypted bytes must be valid JSON equal to the fixture's plaintext.
	assert.JSONEq(t, string(f.Plaintext), string(plaintext))
}

func TestDecryptMessageBody_WrongKey(t *testing.T) {
	f := loadFixture(t)
	wrongKey := make([]byte, 32) // all-zero key
	_, err := broker.DecryptMessageBody(f.EncryptedBody, wrongKey)
	// Wrong key produces garbage that fails PKCS#7 unpadding.
	require.Error(t, err)
}

func TestDecryptMessageBody_TruncatedPayload(t *testing.T) {
	f := loadFixture(t)
	key, err := base64.StdEncoding.DecodeString(f.KeyBase64)
	require.NoError(t, err)

	// Decode, drop the last 16 bytes, re-encode.
	raw, err := base64.StdEncoding.DecodeString(f.EncryptedBody)
	require.NoError(t, err)
	require.Greater(t, len(raw), 16, "fixture must be long enough to truncate")

	truncated := base64.StdEncoding.EncodeToString(raw[:len(raw)-16])
	_, err = broker.DecryptMessageBody(truncated, key)
	require.Error(t, err)
}

func TestDecryptMessageBody_InvalidBase64(t *testing.T) {
	key := make([]byte, 32)
	_, err := broker.DecryptMessageBody("!!!not-valid-base64!!!", key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base64")
}
