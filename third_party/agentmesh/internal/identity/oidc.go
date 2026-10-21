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
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
)

// UnknownIssuerError is returned by VerifyJWT when the token names an issuer
// that has no resolved provider: either one that was never configured, or one
// whose OpenID discovery has not succeeded yet. The caller tells the two apart.
type UnknownIssuerError struct {
	Issuer string
}

func (e *UnknownIssuerError) Error() string {
	return fmt.Sprintf("unknown issuer: %s", e.Issuer)
}

// VerifyJWT parses and cryptographically validates a JWT token against a list of allowed audiences
// and resolved OIDC providers.
func VerifyJWT(ctx context.Context, jwtStr string, allowedAudiences []string, providers map[string]*oidc.Provider) (jwt.MapClaims, *oidc.IDToken, error) {
	// The unverified parse exists only to pick the issuer whose keys will
	// verify the token. Nothing read here is trusted: the audience and the
	// returned claims come from the token Verify hands back, and a token that
	// names an issuer it was not signed by fails there.
	iss, err := issuerHint(jwtStr)
	if err != nil {
		return nil, nil, err
	}

	provider, ok := providers[iss]
	if !ok {
		return nil, nil, &UnknownIssuerError{Issuer: iss}
	}

	// SkipClientIDCheck because the verifier only knows how to match a single
	// client ID; the allowed-audience check below runs on the verified token.
	verifier := provider.Verifier(&oidc.Config{
		SkipClientIDCheck: true,
	})

	token, err := verifier.Verify(ctx, jwtStr)
	if err != nil {
		return nil, nil, fmt.Errorf("JWT validation failed: %w", err)
	}

	if err := checkAudience(token.Audience, allowedAudiences); err != nil {
		return nil, nil, err
	}

	var verifiedClaims jwt.MapClaims
	if err := token.Claims(&verifiedClaims); err != nil {
		return nil, nil, fmt.Errorf("failed to decode verified claims: %w", err)
	}

	return verifiedClaims, token, nil
}

// issuerHint reads the iss claim without verifying the signature. It also
// refuses alg=none up front so a downgrade attempt never reaches a verifier.
func issuerHint(jwtStr string) (string, error) {
	jwtParser := jwt.Parser{}
	jwtToken, _, err := jwtParser.ParseUnverified(jwtStr, jwt.MapClaims{})
	if err != nil {
		return "", fmt.Errorf("failed to parse JWT: %w", err)
	}

	alg, ok := jwtToken.Header["alg"].(string)
	if !ok || alg == "" || strings.ToLower(alg) == "none" {
		return "", fmt.Errorf("invalid or missing alg header")
	}

	claims, ok := jwtToken.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("invalid JWT claims")
	}
	iss, ok := claims["iss"].(string)
	if !ok || iss == "" {
		return "", fmt.Errorf("missing or invalid iss claim")
	}
	return iss, nil
}

// checkAudience accepts a token if any of its audiences is allowed: a
// multi-audience token only needs to be intended for us, whatever else it
// names.
func checkAudience(auds, allowedAudiences []string) error {
	if len(auds) == 0 {
		return fmt.Errorf("missing aud claim")
	}
	for _, aud := range auds {
		for _, allowed := range allowedAudiences {
			if aud == allowed {
				return nil
			}
		}
	}
	return fmt.Errorf("untrusted audience(s): %s", strings.Join(auds, ", "))
}
