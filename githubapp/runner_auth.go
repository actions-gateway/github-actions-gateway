package githubapp

// FetchRunnerOAuthToken implements the OAuth 2.0 client credentials grant with a
// JWT bearer client assertion (RFC 7523 §2.2), matching the VssOAuthCredential
// flow used by the GitHub Actions runner to authenticate to the VSTS Task Agent API.
//
// After config.sh registers a runner, GitHub writes two credential files:
//
//   .credentials           — JSON with scheme "OAuth", clientId, and authorizationUrl.
//   .credentials_rsaparams — JSON with the runner's RSA private key in .NET
//                            RSAParameters format (Base64-encoded big-endian components).
//
// The exchange flow:
//  1. Read clientId and authorizationUrl from .credentials.
//  2. Read the RSA private key from .credentials_rsaparams.
//  3. Build a JWT (RS256): header={alg,typ}, claims={iss,sub,aud,nbf,exp,jti}.
//  4. POST to authorizationUrl with:
//       grant_type=client_credentials
//       client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer
//       client_assertion=<JWT>
//  5. Return the access_token from the JSON response.
//
// The returned token is suitable for Authorization: Bearer in VSTS Task Agent API calls
// (CreateSession, GetMessage, DeleteSession).

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// utf8BOM is the byte-order mark that .NET writes at the start of JSON files.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// stripBOM removes a leading UTF-8 BOM if present. .NET's JSON serializer
// writes BOM-prefixed files; Go's json package does not handle the BOM.
func stripBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, utf8BOM)
}

// RunnerCredentials holds the OAuth2 client credentials from the runner's
// .credentials file. These are written by config.sh during runner registration.
type RunnerCredentials struct {
	ClientID         string
	AuthorizationURL string
}

// ParseRunnerCredentials reads the runner's .credentials JSON file and extracts
// the clientId and authorizationUrl from the OAuth data section.
func ParseRunnerCredentials(path string) (*RunnerCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runner credentials: %w", err)
	}
	var raw struct {
		Scheme string `json:"scheme"`
		Data   struct {
			ClientID         string `json:"clientId"`
			AuthorizationURL string `json:"authorizationUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stripBOM(data), &raw); err != nil {
		return nil, fmt.Errorf("parse runner credentials: %w", err)
	}
	if raw.Data.ClientID == "" || raw.Data.AuthorizationURL == "" {
		return nil, fmt.Errorf("runner credentials missing clientId or authorizationUrl (got scheme=%q)", raw.Scheme)
	}
	return &RunnerCredentials{
		ClientID:         raw.Data.ClientID,
		AuthorizationURL: raw.Data.AuthorizationURL,
	}, nil
}

// dotNetRSAParams matches the JSON written by .NET's RSAParameters serializer.
// All fields are standard Base64 (with padding), big-endian byte arrays.
type dotNetRSAParams struct {
	Exponent string `json:"Exponent"`
	Modulus  string `json:"Modulus"`
	P        string `json:"P"`
	Q        string `json:"Q"`
	DP       string `json:"DP"`
	DQ       string `json:"DQ"`
	InverseQ string `json:"InverseQ"`
	D        string `json:"D"`
}

// ParseRunnerRSAKey reads the runner's .credentials_rsaparams JSON file and
// reconstructs the RSA private key. Both standard Base64 (with padding) and
// Base64URL (without padding) encodings are accepted for forward-compatibility.
func ParseRunnerRSAKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rsa params: %w", err)
	}
	var params dotNetRSAParams
	if err := json.Unmarshal(stripBOM(data), &params); err != nil {
		return nil, fmt.Errorf("parse rsa params: %w", err)
	}

	decodeBigInt := func(fieldName, s string) (*big.Int, error) {
		if s == "" {
			return nil, fmt.Errorf("field %s is empty", fieldName)
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			// Fallback: try base64url (no padding) in case the runner uses JWK format.
			b, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
			if err != nil {
				return nil, fmt.Errorf("field %s: base64 decode: %w", fieldName, err)
			}
		}
		return new(big.Int).SetBytes(b), nil
	}

	n, err := decodeBigInt("Modulus", params.Modulus)
	if err != nil {
		return nil, err
	}
	e, err := decodeBigInt("Exponent", params.Exponent)
	if err != nil {
		return nil, err
	}
	d, err := decodeBigInt("D", params.D)
	if err != nil {
		return nil, err
	}
	p, err := decodeBigInt("P", params.P)
	if err != nil {
		return nil, err
	}
	q, err := decodeBigInt("Q", params.Q)
	if err != nil {
		return nil, err
	}
	priv := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: n, E: int(e.Int64())},
		D:         d,
		Primes:    []*big.Int{p, q},
	}
	// Recompute Dp, Dq, Qinv from D, P, Q rather than trusting the file values,
	// to avoid subtle issues with .NET's serialization of precomputed fields.
	priv.Precompute()

	if err := priv.Validate(); err != nil {
		return nil, fmt.Errorf("rsa key invalid: %w", err)
	}
	return priv, nil
}

// FetchRunnerOAuthToken exchanges the runner's credentials for a VSTS Task
// Agent OAuth2 access token using the JWT bearer assertion grant (RFC 7523).
//
// privateKey may be an ed25519.PrivateKey (EdDSA / OKP JWT) or an
// *rsa.PrivateKey (RS256).  The signing algorithm is chosen automatically.
//
// The returned token is used as Authorization: Bearer for broker API calls
// (CreateSession, GetMessage, DeleteSession).
func FetchRunnerOAuthToken(ctx context.Context, creds *RunnerCredentials, privateKey crypto.Signer, httpClient *http.Client) (string, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	// Choose signing method and the key value jwt.SignedString expects.
	// golang-jwt/jwt/v5:
	//   - SigningMethodRS256.Sign expects *rsa.PrivateKey
	//   - SigningMethodEdDSA.Sign expects ed25519.PrivateKey or crypto.Signer
	var (
		signingMethod jwt.SigningMethod
		signingKey    interface{}
	)
	switch k := privateKey.(type) {
	case ed25519.PrivateKey:
		signingMethod = jwt.SigningMethodEdDSA
		signingKey = k
	case *rsa.PrivateKey:
		signingMethod = jwt.SigningMethodRS256
		signingKey = k
	default:
		return "", fmt.Errorf("unsupported private key type %T; want ed25519.PrivateKey or *rsa.PrivateKey", privateKey)
	}

	// Build a JWT client assertion matching the OAuth 2.0 Private Key JWT
	// client authentication profile (RFC 7523 §2.2):
	//   - Header: {"alg":"<RS256|EdDSA>","typ":"JWT"}  — no "kid"
	//   - Claims: iss=clientId, sub=clientId, aud=authorizationUrl,
	//             nbf=now, exp=now+5m, jti=<unique GUID>
	//             No "iat" claim (the .NET SDK explicitly omits it unless set).
	now := time.Now()
	tok := jwt.New(signingMethod)
	// Do NOT add a "kid" header — the runner SDK does not set one.
	delete(tok.Header, "kid")
	tok.Claims = jwt.MapClaims{
		"sub": creds.ClientID,
		"iss": creds.ClientID,
		"aud": creds.AuthorizationURL, // string, not []string; full URL
		"nbf": jwt.NewNumericDate(now),
		"exp": jwt.NewNumericDate(now.Add(5 * time.Minute)),
		"jti": newUUID(), // required by VSTS token service
		// No "iat" — the .NET runner SDK omits it for client assertions.
	}
	assertion, err := tok.SignedString(signingKey)
	if err != nil {
		return "", fmt.Errorf("sign runner JWT assertion: %w", err)
	}

	// POST the assertion using the OAuth 2.0 client credentials grant with a
	// JWT bearer client assertion — matching the VssOAuthClientCredentialsGrant
	// + VssOAuthJwtBearerClientCredential combination in the runner SDK:
	//   grant_type=client_credentials
	//   client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer
	//   client_assertion=<JWT>
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, creds.AuthorizationURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("runner token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("runner token endpoint returned %d: %s", resp.StatusCode, SanitizeBody(body, 512))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse runner token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("runner token response missing access_token: %s", SanitizeBody(body, 512))
	}
	return tokenResp.AccessToken, nil
}

// newUUID returns a random UUID v4 string (xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx).
// Used to populate the jti claim, which the VSTS token service requires to be
// unique per request.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
