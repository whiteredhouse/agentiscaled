// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
)

const testKID = "mock-key"

// newMockOIDCIssuer starts an in-process OIDC discovery/JWKS server backed by
// the returned RSA key, so tests can mint their own tokens with arbitrary
// (including invalid) claims.
func newMockOIDCIssuer(t *testing.T) (issuer string, key *rsa.PrivateKey) {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate rsa key: %v", err)
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   issuer,
			"jwks_uri": issuer + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"alg": "RS256",
					"use": "sig",
					"kid": testKID,
					"n":   base64.RawURLEncoding.EncodeToString(privKey.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privKey.E)).Bytes()),
				},
			},
		})
	})

	return issuer, privKey
}

func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	str, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return str
}

func TestVerifyJWT(t *testing.T) {
	issuer, key := newMockOIDCIssuer(t)
	ctx := context.Background()
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		t.Fatalf("failed to discover mock provider: %v", err)
	}
	providers := map[string]*oidc.Provider{issuer: provider}
	allowedAudiences := []string{"agentmesh-audience"}

	validClaims := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": issuer,
			"aud": "agentmesh-audience",
			"sub": "user-1",
			"exp": time.Now().Add(time.Hour).Unix(),
		}
	}

	t.Run("valid token succeeds", func(t *testing.T) {
		tokenStr := signToken(t, key, testKID, validClaims())
		claims, idToken, err := VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if idToken == nil {
			t.Fatal("expected non-nil ID token")
		}
		if claims["sub"] != "user-1" {
			t.Fatalf("unexpected sub claim: %v", claims["sub"])
		}
	})

	t.Run("malformed token is rejected", func(t *testing.T) {
		_, _, err := VerifyJWT(ctx, "not-a-valid-jwt", allowedAudiences, providers)
		if err == nil {
			t.Fatal("expected error for malformed token")
		}
	})

	t.Run("alg=none downgrade is rejected", func(t *testing.T) {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + issuer + `","aud":"agentmesh-audience"}`))
		tokenStr := header + "." + payload + "."

		_, _, err := VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err == nil {
			t.Fatal("expected error for alg=none token")
		}
	})

	t.Run("missing aud claim is rejected", func(t *testing.T) {
		claims := validClaims()
		delete(claims, "aud")
		tokenStr := signToken(t, key, testKID, claims)

		_, _, err := VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err == nil {
			t.Fatal("expected error for missing aud claim")
		}
	})

	t.Run("untrusted audience is rejected", func(t *testing.T) {
		claims := validClaims()
		claims["aud"] = "some-other-audience"
		tokenStr := signToken(t, key, testKID, claims)

		_, _, err := VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err == nil {
			t.Fatal("expected error for untrusted audience")
		}
	})

	t.Run("allowed audience in any array position succeeds", func(t *testing.T) {
		claims := validClaims()
		claims["aud"] = []string{"some-other-audience", "agentmesh-audience"}
		tokenStr := signToken(t, key, testKID, claims)

		_, _, err := VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err != nil {
			t.Fatalf("expected success for multi-audience token, got: %v", err)
		}
	})

	t.Run("audience array with no allowed entry is rejected", func(t *testing.T) {
		claims := validClaims()
		claims["aud"] = []string{"some-other-audience", "yet-another-audience"}
		tokenStr := signToken(t, key, testKID, claims)

		_, _, err := VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err == nil {
			t.Fatal("expected error for audience array with no allowed entry")
		}
	})

	t.Run("empty audience array is rejected", func(t *testing.T) {
		claims := validClaims()
		claims["aud"] = []string{}
		tokenStr := signToken(t, key, testKID, claims)

		_, _, err := VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err == nil {
			t.Fatal("expected error for empty audience array")
		}
	})

	t.Run("missing iss claim is rejected", func(t *testing.T) {
		claims := validClaims()
		delete(claims, "iss")
		tokenStr := signToken(t, key, testKID, claims)

		_, _, err := VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err == nil || !strings.Contains(err.Error(), "iss claim") {
			t.Fatalf("expected missing iss error, got: %v", err)
		}
	})

	t.Run("unknown issuer is rejected", func(t *testing.T) {
		claims := validClaims()
		claims["iss"] = "https://unknown-issuer.example"
		tokenStr := signToken(t, key, testKID, claims)

		_, _, err := VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err == nil {
			t.Fatal("expected error for unknown issuer")
		}
	})

	t.Run("bad signature is rejected", func(t *testing.T) {
		otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("failed to generate rsa key: %v", err)
		}
		// Sign with a key that does not match the one published in the JWKS
		// for this kid, so signature verification must fail.
		tokenStr := signToken(t, otherKey, testKID, validClaims())

		_, _, err = VerifyJWT(ctx, tokenStr, allowedAudiences, providers)
		if err == nil {
			t.Fatal("expected error for bad signature")
		}
	})

	t.Run("tampered payload yields no claims", func(t *testing.T) {
		// Forge a token by swapping the payload of a validly signed token
		// while keeping the original signature: the routing claims still
		// parse, but verification must reject it and no claims may escape.
		tokenStr := signToken(t, key, testKID, validClaims())
		parts := strings.Split(tokenStr, ".")
		forgedClaims := validClaims()
		forgedClaims["sub"] = "attacker"
		payload, err := json.Marshal(forgedClaims)
		if err != nil {
			t.Fatalf("failed to marshal forged claims: %v", err)
		}
		parts[1] = base64.RawURLEncoding.EncodeToString(payload)
		forged := strings.Join(parts, ".")

		claims, idToken, err := VerifyJWT(ctx, forged, allowedAudiences, providers)
		if err == nil {
			t.Fatal("expected error for tampered payload")
		}
		if claims != nil {
			t.Fatalf("claims leaked from an unverified token: %v", claims)
		}
		if idToken != nil {
			t.Fatal("ID token returned for an unverified token")
		}
	})
}
