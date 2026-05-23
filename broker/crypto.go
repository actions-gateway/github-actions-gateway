package broker

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // SHA-1 required by .NET RSA.Decrypt OAEP default
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
)

// errInvalidPadding is the single sentinel error for any PKCS#7 padding
// violation. Using one value prevents callers from distinguishing failure
// modes and eliminates the timing side-channel that arises when different
// error paths take different amounts of time.
var errInvalidPadding = errors.New("broker: invalid padding")

// DecryptSessionKey decrypts the RSA-encrypted AES session key returned in the
// CreateSession response's encryptionKey.value field.
//
// The .NET runner encrypts the session key with RSA-OAEP (SHA-1 hash, no label),
// which matches rsa.DecryptOAEP with sha1.New(). The result is the raw 32-byte
// AES-256-CBC key passed to DecryptMessageBody.
func DecryptSessionKey(encryptedKey []byte, privateKey *rsa.PrivateKey) ([]byte, error) {
	//nolint:gosec // SHA-1 is mandated by the .NET RSA.Decrypt OAEP default; not our choice
	plain, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, privateKey, encryptedKey, nil)
	if err != nil {
		return nil, fmt.Errorf("broker: DecryptSessionKey: RSA-OAEP decrypt: %w", err)
	}
	return plain, nil
}

// DecryptMessageBody decrypts the AES-256-CBC encrypted body of a TaskAgentMessage.
//
// GitHub encodes the wire value as base64(IV || ciphertext) where:
//   - IV is the first 16 bytes after base64 decoding.
//   - ciphertext is the remainder, padded with PKCS#7 to a 16-byte boundary.
//
// key is the raw 32-byte session key returned by CreateSession (the broker
// returns it base64-encoded; callers must decode it before passing here).
//
// Returns the unpadded plaintext bytes or an error describing the failure.
func DecryptMessageBody(encryptedBody string, key []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encryptedBody)
	if err != nil {
		return nil, fmt.Errorf("broker: DecryptMessageBody: base64 decode: %w", err)
	}

	// Need at least one block (16 bytes) for the IV plus one block of ciphertext.
	const blockSize = aes.BlockSize
	if len(raw) < 2*blockSize {
		return nil, fmt.Errorf("broker: DecryptMessageBody: payload too short (%d bytes)", len(raw))
	}

	iv := raw[:blockSize]
	ciphertext := raw[blockSize:]

	if len(ciphertext)%blockSize != 0 {
		return nil, fmt.Errorf("broker: DecryptMessageBody: ciphertext length %d is not a multiple of block size", len(ciphertext))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("broker: DecryptMessageBody: create cipher: %w", err)
	}

	mode := cipher.NewCBCDecrypter(block, iv)
	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)

	unpadded, err := pkcs7Unpad(plaintext, blockSize)
	if err != nil {
		return nil, fmt.Errorf("broker: DecryptMessageBody: unpad: %w", err)
	}
	return unpadded, nil
}

// pkcs7Unpad removes PKCS#7 padding from a plaintext block. Returns
// errInvalidPadding for any malformed padding. The byte comparison loop runs
// in constant time via crypto/subtle to avoid a timing side-channel.
func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 {
		return nil, errInvalidPadding
	}
	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > blockSize || padLen > len(data) {
		return nil, errInvalidPadding
	}
	ok := 1
	for i := len(data) - padLen; i < len(data); i++ {
		ok &= subtle.ConstantTimeByteEq(data[i], byte(padLen))
	}
	if ok != 1 {
		return nil, errInvalidPadding
	}
	return data[:len(data)-padLen], nil
}
