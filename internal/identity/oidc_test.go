package identity_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/identity"
)

func TestStrictOIDCVerifierPinsAlgorithmIssuerAudienceAndTime(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/jwks" {
			http.NotFound(writer, request)
			return
		}
		exponent := big.NewInt(int64(privateKey.PublicKey.E)).Bytes()
		_ = json.NewEncoder(writer).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "active", "kty": "RSA", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(exponent),
		}}})
	}))
	defer server.Close()
	issuer = server.URL
	verifier, err := identity.NewOIDCVerifier(identity.OIDCConfig{
		Issuer: issuer, Audience: "xirang", JWKSURL: server.URL + "/jwks", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	token := signedJWT(t, privateKey, map[string]any{
		"iss": issuer, "aud": "xirang", "sub": "user-1", "name": "Operator",
		"groups": []string{"xirang-operators"}, "exp": time.Now().Add(time.Minute).Unix(),
	})
	claims, err := verifier.Verify(context.Background(), token)
	if err != nil || claims.Subject != "user-1" || len(claims.Groups) != 1 {
		t.Fatalf("valid token rejected: claims=%#v err=%v", claims, err)
	}
	wrongAudience := signedJWT(t, privateKey, map[string]any{
		"iss": issuer, "aud": "other", "sub": "user-1", "exp": time.Now().Add(time.Minute).Unix(),
	})
	if _, err := verifier.Verify(context.Background(), wrongAudience); err == nil {
		t.Fatal("wrong audience accepted")
	}
	parts := []any{map[string]any{"alg": "none", "kid": "active"}, map[string]any{"iss": issuer, "aud": "xirang", "sub": "user-1", "exp": time.Now().Add(time.Minute).Unix()}}
	header, _ := json.Marshal(parts[0])
	payload, _ := json.Marshal(parts[1])
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
	if _, err := verifier.Verify(context.Background(), unsigned); err == nil {
		t.Fatal("alg none token accepted")
	}
}

func signedJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "active"})
	payload, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}
