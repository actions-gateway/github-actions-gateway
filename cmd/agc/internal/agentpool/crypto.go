package agentpool

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// KeyType selects the algorithm used when generating new agent key pairs.
type KeyType string

const (
	// KeyTypeEd25519 generates an Ed25519 key (default).
	KeyTypeEd25519 KeyType = "ed25519"
	// KeyTypeRSA generates an RSA-3072 key.
	KeyTypeRSA KeyType = "rsa"
)

// generateKey returns a new private key of the requested type.
// An empty KeyType defaults to KeyTypeEd25519.
func generateKey(kt KeyType) (crypto.Signer, error) {
	switch kt {
	case KeyTypeRSA:
		return rsa.GenerateKey(rand.Reader, 3072)
	default:
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		return priv, err
	}
}

// marshalPrivateKey encodes key as a PKCS#8 DER blob wrapped in PEM.
// Both Ed25519 and RSA keys are supported.
func marshalPrivateKey(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal PKCS8 private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// parsePrivateKeySigner decodes a PEM-encoded private key and returns it as
// a crypto.Signer.  Accepted formats:
//   - PKCS#8 "PRIVATE KEY" — Ed25519 or RSA (new format)
//   - PKCS#1 "RSA PRIVATE KEY" — RSA only (legacy format for existing Secrets)
func parsePrivateKeySigner(data []byte) (crypto.Signer, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty PEM data")
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS8 private key: %w", err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key type %T does not implement crypto.Signer", key)
		}
		return signer, nil
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS1 private key: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}
}
