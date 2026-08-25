package identity

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type OIDCConfig struct {
	Issuer      string
	Audience    string
	JWKSURL     string
	GroupsClaim string
	HTTPClient  *http.Client
}

type StrictOIDCVerifier struct {
	issuer, audience, jwksURL, groupsClaim string
	client                                 *http.Client
	now                                    func() time.Time
	mu                                     sync.Mutex
	keys                                   map[string]*rsa.PublicKey
	keysUntil                              time.Time
}

func NewOIDCVerifier(config OIDCConfig) (*StrictOIDCVerifier, error) {
	issuer, issuerErr := url.Parse(strings.TrimRight(config.Issuer, "/"))
	jwks, jwksErr := url.Parse(config.JWKSURL)
	if issuerErr != nil || jwksErr != nil || issuer.Scheme != "https" || jwks.Scheme != "https" ||
		issuer.Host == "" || jwks.Host == "" || config.Audience == "" {
		return nil, errors.New("OIDC issuer, audience and JWKS HTTPS URL are required")
	}
	if config.GroupsClaim == "" {
		config.GroupsClaim = "groups"
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &StrictOIDCVerifier{
		issuer: strings.TrimRight(config.Issuer, "/"), audience: config.Audience,
		jwksURL: config.JWKSURL, groupsClaim: config.GroupsClaim, client: config.HTTPClient,
		now: time.Now, keys: map[string]*rsa.PublicKey{},
	}, nil
}

func (verifier *StrictOIDCVerifier) Verify(ctx context.Context, token string) (OIDCClaims, error) {
	if len(token) == 0 || len(token) > 16<<10 {
		return OIDCClaims{}, ErrUnauthorized
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return OIDCClaims{}, ErrUnauthorized
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return OIDCClaims{}, ErrUnauthorized
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
		Type      string `json:"typ"`
	}
	if json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "RS256" || header.KeyID == "" ||
		(header.Type != "" && header.Type != "JWT" && header.Type != "at+jwt") {
		return OIDCClaims{}, ErrUnauthorized
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return OIDCClaims{}, ErrUnauthorized
	}
	key, err := verifier.key(ctx, header.KeyID)
	if err != nil {
		return OIDCClaims{}, ErrUnauthorized
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return OIDCClaims{}, ErrUnauthorized
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return OIDCClaims{}, ErrUnauthorized
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return OIDCClaims{}, ErrUnauthorized
	}
	now := verifier.now()
	if claims["iss"] != verifier.issuer || !audienceContains(claims["aud"], verifier.audience) ||
		!numericTimeAfter(claims["exp"], now) || numericTimeInFuture(claims["nbf"], now.Add(30*time.Second)) {
		return OIDCClaims{}, ErrUnauthorized
	}
	subject, _ := claims["sub"].(string)
	if subject == "" || len(subject) > 512 {
		return OIDCClaims{}, ErrUnauthorized
	}
	name, _ := claims["name"].(string)
	groups := stringSliceClaim(claims[verifier.groupsClaim])
	return OIDCClaims{Subject: subject, Name: name, Groups: groups}, nil
}

func (verifier *StrictOIDCVerifier) key(ctx context.Context, id string) (*rsa.PublicKey, error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if key := verifier.keys[id]; key != nil && verifier.now().Before(verifier.keysUntil) {
		return key, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, verifier.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := verifier.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > 1<<20 {
		return nil, ErrUnauthorized
	}
	var set struct {
		Keys []struct {
			KeyID     string `json:"kid"`
			KeyType   string `json:"kty"`
			Use       string `json:"use"`
			Algorithm string `json:"alg"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
		} `json:"keys"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if decoder.Decode(&set) != nil || len(set.Keys) > 100 {
		return nil, ErrUnauthorized
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, jwk := range set.Keys {
		if jwk.KeyID == "" || jwk.KeyType != "RSA" || jwk.Algorithm != "RS256" || (jwk.Use != "" && jwk.Use != "sig") {
			continue
		}
		modulus, nErr := base64.RawURLEncoding.DecodeString(jwk.Modulus)
		exponent, eErr := base64.RawURLEncoding.DecodeString(jwk.Exponent)
		if nErr != nil || eErr != nil || len(modulus) < 256 || len(exponent) == 0 || len(exponent) > 4 {
			continue
		}
		e := 0
		for _, value := range exponent {
			e = e<<8 | int(value)
		}
		if e < 3 || e%2 == 0 {
			continue
		}
		keys[jwk.KeyID] = &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: e}
	}
	verifier.keys, verifier.keysUntil = keys, verifier.now().Add(5*time.Minute)
	key := keys[id]
	if key == nil {
		return nil, ErrUnauthorized
	}
	return key, nil
}

func audienceContains(value any, expected string) bool {
	switch audiences := value.(type) {
	case string:
		return audiences == expected
	case []any:
		for _, audience := range audiences {
			if audience == expected {
				return true
			}
		}
	}
	return false
}
func numericTimeAfter(value any, instant time.Time) bool {
	number, ok := value.(float64)
	return ok && number <= float64(1<<53) && instant.Before(time.Unix(int64(number), 0))
}
func numericTimeInFuture(value any, instant time.Time) bool {
	if value == nil {
		return false
	}
	number, ok := value.(float64)
	return !ok || time.Unix(int64(number), 0).After(instant)
}
func stringSliceClaim(value any) []string {
	values, ok := value.([]any)
	if !ok || len(values) > 128 {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if ok && text != "" && len(text) <= 256 {
			result = append(result, text)
		}
	}
	return result
}
