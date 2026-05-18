package githubapp

// FetchRunnerOAuthToken implements the OAuth2 JWT bearer assertion grant (RFC 7523)
// used by GitHub Actions runner agents to authenticate to the VSTS Task Agent API.
//
// After config.sh registers a runner, GitHub writes two credential files:
//
//   .credentials       — JSON with scheme "OAuth", clientId, and authorizationUrl.
//   .credentials_rsaparams — JSON with the runner's RSA private key in .NET
//                        RSAParameters format (Base64-encoded big-endian components).
//
// The exchange flow:
//  1. Read clientId and authorizationUrl from .credentials.
//  2. Read the RSA private key from .credentials_rsaparams.
//  3. Build a JWT signed with the RSA key (RS256, iss/sub = clientId, aud = authorizationUrl).
//  4. POST the JWT as a client_assertion to the authorizationUrl (form-urlencoded).
//  5. Return the access_token from the JSON response.
//
// The returned token is suitable for Authorization: Bearer in VSTS Task Agent API calls
// (CreateSession, GetMessage, DeleteSession).

import (
	"bytes"
	"context"
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

// FetchRunnerOAuthToken exchanges the runner's RSA credentials for a VSTS Task
// Agent OAuth2 access token using the JWT bearer assertion grant (RFC 7523).
//
// The returned token is used as Authorization: Bearer for broker API calls
// (CreateSession, GetMessage, DeleteSession).
func FetchRunnerOAuthToken(ctx context.Context, creds *RunnerCredentials, privateKey *rsa.PrivateKey, httpClient *http.Client) (string, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	// Build a JWT assertion signed with the runner's RSA private key.
	// VSTS identifies the registered public key via the "kid" header = clientId.
	// Use the full authorizationUrl as the audience (string, not array) and
	// set nbf == iat (no clock-skew offset) to match the .NET runner behaviour.
	now := time.Now()
	tok := jwt.New(jwt.SigningMethodRS256)
	tok.Header["kid"] = creds.ClientID
	tok.Claims = jwt.MapClaims{
		"sub": creds.ClientID,
		"iss": creds.ClientID,
		"aud": creds.AuthorizationURL, // string, not []string; full URL
		"nbf": jwt.NewNumericDate(now),
		"iat": jwt.NewNumericDate(now),
		"exp": jwt.NewNumericDate(now.Add(5 * time.Minute)),
	}
	assertion, err := tok.SignedString(privateKey)
	if err != nil {
		return "", fmt.Errorf("sign runner JWT assertion: %w", err)
	}

	// Print the decoded JWT header and claims to stderr for debugging.
	// Remove once the token exchange is confirmed working.
	if parts := strings.SplitN(assertion, ".", 3); len(parts) == 3 {
		if hdr, e := base64.RawURLEncoding.DecodeString(parts[0]); e == nil {
			fmt.Fprintf(os.Stderr, "DEBUG runner JWT header : %s\n", hdr)
		}
		if clm, e := base64.RawURLEncoding.DecodeString(parts[1]); e == nil {
			fmt.Fprintf(os.Stderr, "DEBUG runner JWT claims : %s\n", clm)
		}
	}

	// POST the assertion to the VSTS token endpoint (form-urlencoded).
	// VSTS uses "assertion" as the JWT parameter name rather than the RFC 7523
	// "client_assertion" field name.
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
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
		return "", fmt.Errorf("runner token endpoint returned %d: %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse runner token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("runner token response missing access_token: %s", body)
	}
	return tokenResp.AccessToken, nil
}
