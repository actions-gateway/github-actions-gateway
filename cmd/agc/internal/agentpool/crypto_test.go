package agentpool

import (
	"crypto/ed25519"
	"crypto/rsa"
	"testing"
)

// TestGenerateKeyDefaultIsRSA pins the secure-by-default contract: an empty
// (unspecified) KeyType must yield an RSA-3072 key, never Ed25519. Defaulting
// empty→Ed25519 would silently drop RSA-OAEP session-key encryption for any
// caller that omits the key type (Q109 regression). RSA stays the default;
// Ed25519 is an explicit opt-in only (Q11).
func TestGenerateKeyDefaultIsRSA(t *testing.T) {
	tests := []struct {
		name    string
		keyType KeyType
		wantRSA bool
	}{
		{name: "empty defaults to RSA", keyType: "", wantRSA: true},
		{name: "explicit rsa", keyType: KeyTypeRSA, wantRSA: true},
		{name: "unrecognised value defaults to RSA", keyType: KeyType("bogus"), wantRSA: true},
		{name: "explicit ed25519 opt-in", keyType: KeyTypeEd25519, wantRSA: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			signer, err := generateKey(tc.keyType)
			if err != nil {
				t.Fatalf("generateKey(%q): %v", tc.keyType, err)
			}
			switch key := signer.(type) {
			case *rsa.PrivateKey:
				if !tc.wantRSA {
					t.Fatalf("generateKey(%q) = RSA, want Ed25519", tc.keyType)
				}
				if got := key.N.BitLen(); got != 3072 {
					t.Errorf("RSA key size = %d bits, want 3072", got)
				}
			case ed25519.PrivateKey:
				if tc.wantRSA {
					t.Fatalf("generateKey(%q) = Ed25519, want RSA", tc.keyType)
				}
			default:
				t.Fatalf("generateKey(%q) = %T, want *rsa.PrivateKey or ed25519.PrivateKey", tc.keyType, signer)
			}
		})
	}
}
