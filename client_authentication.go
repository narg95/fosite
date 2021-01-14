/*
 * Copyright © 2017-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * @author		Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @Copyright 	2017-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @license 	Apache-2.0
 *
 */

package fosite

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ory/x/errorsx"

	jwt "github.com/dgrijalva/jwt-go"
	"github.com/pkg/errors"
	jose "gopkg.in/square/go-jose.v2"
)

const clientAssertionJWTBearerType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

func (f *Fosite) findClientPublicJWK(oidcClient OpenIDConnectClient, t *jwt.Token, expectsRSAKey bool) (interface{}, error) {
	if set := oidcClient.GetJSONWebKeys(); set != nil {
		return findPublicKey(t, set, expectsRSAKey)
	}

	if location := oidcClient.GetJSONWebKeysURI(); len(location) > 0 {
		keys, err := f.JWKSFetcherStrategy.Resolve(location, false)
		if err != nil {
			return nil, err
		}

		if key, err := findPublicKey(t, keys, expectsRSAKey); err == nil {
			return key, nil
		}

		keys, err = f.JWKSFetcherStrategy.Resolve(location, true)
		if err != nil {
			return nil, err
		}

		return findPublicKey(t, keys, expectsRSAKey)
	}

	return nil, errorsx.WithStack(ErrInvalidClient.WithHint("The OAuth 2.0 Client has no JSON Web Keys set registered, but they are needed to complete the request."))
}

func (f *Fosite) AuthenticateClient(ctx context.Context, r *http.Request, form url.Values) (Client, error) {
	if assertionType := form.Get("client_assertion_type"); assertionType == clientAssertionJWTBearerType {
		assertion := form.Get("client_assertion")
		if len(assertion) == 0 {
			return nil, errorsx.WithStack(ErrInvalidRequest.WithHintf("The client_assertion request parameter must be set when using client_assertion_type of '%s'.", clientAssertionJWTBearerType))
		}

		var clientID string
		var client Client

		token, err := jwt.ParseWithClaims(assertion, new(jwt.MapClaims), func(t *jwt.Token) (interface{}, error) {
			var err error
			clientID, _, err = clientCredentialsFromRequestBody(form, false)
			if err != nil {
				return nil, err
			}

			if clientID == "" {
				if claims, ok := t.Claims.(*jwt.MapClaims); !ok {
					return nil, errorsx.WithStack(ErrRequestUnauthorized.WithHint("Unable to type assert claims from client_assertion.").WithDebugf(`Expected claims to be of type '*jwt.MapClaims' but got '%T'.`, t.Claims))
				} else if sub, ok := (*claims)["sub"].(string); !ok {
					return nil, errorsx.WithStack(ErrInvalidClient.WithHint("The claim 'sub' from the client_assertion JSON Web Token is undefined."))
				} else {
					clientID = sub
				}
			}

			client, err = f.Store.GetClient(ctx, clientID)
			if err != nil {
				return nil, errorsx.WithStack(ErrInvalidClient.WithWrap(err).WithDebug(err.Error()))
			}

			oidcClient, ok := client.(OpenIDConnectClient)
			if !ok {
				return nil, errorsx.WithStack(ErrInvalidRequest.WithHint("The server configuration does not support OpenID Connect specific authentication methods."))
			}

			switch oidcClient.GetTokenEndpointAuthMethod() {
			case "private_key_jwt":
				break
			case "none":
				return nil, errorsx.WithStack(ErrInvalidClient.WithHint("This requested OAuth 2.0 client does not support client authentication, however 'client_assertion' was provided in the request."))
			case "client_secret_post":
				fallthrough
			case "client_secret_basic":
				return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("This requested OAuth 2.0 client only supports client authentication method '%s', however 'client_assertion' was provided in the request.", oidcClient.GetTokenEndpointAuthMethod()))
			case "client_secret_jwt":
				fallthrough
			default:
				return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("This requested OAuth 2.0 client only supports client authentication method '%s', however that method is not supported by this server.", oidcClient.GetTokenEndpointAuthMethod()))
			}

			if oidcClient.GetTokenEndpointAuthSigningAlgorithm() != fmt.Sprintf("%s", t.Header["alg"]) {
				return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("The 'client_assertion' uses signing algorithm '%s' but the requested OAuth 2.0 Client enforces signing algorithm '%s'.", t.Header["alg"], oidcClient.GetTokenEndpointAuthSigningAlgorithm()))
			}

			if _, ok := t.Method.(*jwt.SigningMethodRSA); ok {
				return f.findClientPublicJWK(oidcClient, t, true)
			} else if _, ok := t.Method.(*jwt.SigningMethodECDSA); ok {
				return f.findClientPublicJWK(oidcClient, t, false)
			} else if _, ok := t.Method.(*jwt.SigningMethodRSAPSS); ok {
				return f.findClientPublicJWK(oidcClient, t, true)
			} else if _, ok := t.Method.(*jwt.SigningMethodHMAC); ok {
				return nil, errorsx.WithStack(ErrInvalidClient.WithHint("This authorization server does not support client authentication method 'client_secret_jwt'."))
			}

			return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("The 'client_assertion' request parameter uses unsupported signing algorithm '%s'.", t.Header["alg"]))
		})
		if err != nil {
			// Do not re-process already enhanced errors
			var e *jwt.ValidationError
			if errors.As(err, &e) {
				if e.Inner != nil {
					return nil, e.Inner
				}
				return nil, errorsx.WithStack(ErrInvalidClient.WithHint("Unable to verify the integrity of the 'client_assertion' value.").WithWrap(err).WithDebug(err.Error()))
			}
			return nil, err
		} else if err := token.Claims.Valid(); err != nil {
			return nil, errorsx.WithStack(ErrInvalidClient.WithHint("Unable to verify the request object because its claims could not be validated, check if the expiry time is set correctly.").WithWrap(err).WithDebug(err.Error()))
		}

		claims, ok := token.Claims.(*jwt.MapClaims)
		if !ok {
			return nil, errorsx.WithStack(ErrInvalidClient.WithHint("Unable to type assert claims from request parameter 'client_assertion'.").WithDebugf("Got claims of type %T but expected type '*jwt.MapClaims'.", token.Claims))
		}

		var jti string
		if !claims.VerifyIssuer(clientID, true) {
			return nil, errorsx.WithStack(ErrInvalidClient.WithHint("Claim 'iss' from 'client_assertion' must match the 'client_id' of the OAuth 2.0 Client."))
		} else if f.TokenURL == "" {
			return nil, errorsx.WithStack(ErrMisconfiguration.WithHint("The authorization server's token endpoint URL has not been set."))
		} else if sub, ok := (*claims)["sub"].(string); !ok || sub != clientID {
			return nil, errorsx.WithStack(ErrInvalidClient.WithHint("Claim 'sub' from 'client_assertion' must match the 'client_id' of the OAuth 2.0 Client."))
		} else if jti, ok = (*claims)["jti"].(string); !ok || len(jti) == 0 {
			return nil, errorsx.WithStack(ErrInvalidClient.WithHint("Claim 'jti' from 'client_assertion' must be set but is not."))
		} else if f.Store.ClientAssertionJWTValid(ctx, jti) != nil {
			return nil, errorsx.WithStack(ErrJTIKnown.WithHint("Claim 'jti' from 'client_assertion' MUST only be used once."))
		}

		// type conversion according to jwt.MapClaims.VerifyExpiresAt
		var expiry int64
		err = nil
		switch exp := (*claims)["exp"].(type) {
		case float64:
			expiry = int64(exp)
		case json.Number:
			expiry, err = exp.Int64()
		default:
			err = ErrInvalidClient.WithHint("Unable to type assert the expiry time from claims. This should not happen as we validate the expiry time already earlier with token.Claims.Valid()")
		}

		if err != nil {
			return nil, errorsx.WithStack(err)
		}
		if err := f.Store.SetClientAssertionJWT(ctx, jti, time.Unix(expiry, 0)); err != nil {
			return nil, err
		}

		if auds, ok := (*claims)["aud"].([]interface{}); !ok {
			if !claims.VerifyAudience(f.TokenURL, true) {
				return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("Claim 'audience' from 'client_assertion' must match the authorization server's token endpoint '%s'.", f.TokenURL))
			}
		} else {
			var found bool
			for _, aud := range auds {
				if a, ok := aud.(string); ok && a == f.TokenURL {
					found = true
					break
				}
			}

			if !found {
				return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("Claim 'audience' from 'client_assertion' must match the authorization server's token endpoint '%s'.", f.TokenURL))
			}
		}

		return client, nil
	} else if len(assertionType) > 0 {
		return nil, errorsx.WithStack(ErrInvalidRequest.WithHintf("Unknown client_assertion_type '%s'.", assertionType))
	}

	// TODO: validate that client and server were configured to accept tls auth
	if ok := isTLSAuth(r, form); ok {
		return f.authenticateClientWithTLS(ctx, r, form)
	}

	clientID, clientSecret, err := clientCredentialsFromRequest(r, form)
	if err != nil {
		return nil, err
	}

	client, err := f.Store.GetClient(ctx, clientID)
	if err != nil {
		return nil, errorsx.WithStack(ErrInvalidClient.WithWrap(err).WithDebug(err.Error()))
	}

	if oidcClient, ok := client.(OpenIDConnectClient); !ok {
		// If this isn't an OpenID Connect client then we actually don't care about any of this, just continue!
	} else if ok && form.Get("client_id") != "" && form.Get("client_secret") != "" && oidcClient.GetTokenEndpointAuthMethod() != "client_secret_post" {
		return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("The OAuth 2.0 Client supports client authentication method '%s', but method 'client_secret_post' was requested. You must configure the OAuth 2.0 client's 'token_endpoint_auth_method' value to accept 'client_secret_post'.", oidcClient.GetTokenEndpointAuthMethod()))
	} else if _, _, basicOk := r.BasicAuth(); basicOk && ok && oidcClient.GetTokenEndpointAuthMethod() != "client_secret_basic" {
		return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("The OAuth 2.0 Client supports client authentication method '%s', but method 'client_secret_basic' was requested. You must configure the OAuth 2.0 client's 'token_endpoint_auth_method' value to accept 'client_secret_basic'.", oidcClient.GetTokenEndpointAuthMethod()))
	} else if ok && oidcClient.GetTokenEndpointAuthMethod() != "none" && client.IsPublic() {
		return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("The OAuth 2.0 Client supports client authentication method '%s', but method 'none' was requested. You must configure the OAuth 2.0 client's 'token_endpoint_auth_method' value to accept 'none'.", oidcClient.GetTokenEndpointAuthMethod()))
	}

	if client.IsPublic() {
		return client, nil
	}

	// Enforce client authentication
	if err := f.Hasher.Compare(ctx, client.GetHashedSecret(), []byte(clientSecret)); err != nil {
		return nil, errorsx.WithStack(ErrInvalidClient.WithWrap(err).WithDebug(err.Error()))
	}

	return client, nil
}

func (f *Fosite) authenticateClientWithTLS(ctx context.Context, r *http.Request, form url.Values) (Client, error) {
	clientID := form.Get("client_id")
	if len(clientID) == 0 {
		return nil, errorsx.WithStack(ErrInvalidRequest.WithHint("The client_id was not given"))
	}
	client, err := f.Store.GetClient(ctx, clientID)
	if err != nil {
		return nil, err
	}

	// TODO: validate if this validation makes sense,
	// that is, a OpenIDConnectClient with tls auth
	if oidcClient, ok := client.(OpenIDConnectClient); ok &&
		oidcClient.GetTokenEndpointAuthMethod() != "tls_client_auth" {
		return nil, errorsx.WithStack(ErrInvalidRequest.WithHintf(
			"This requested OAuth 2.0 client only supports client authentication method '%s', but TLS authentication was requested.", oidcClient.GetTokenEndpointAuthMethod()))
	}

	// TODO: Support SAN Fields.
	// Check this for impl. https://github.com/golang/go/blob/2a26f5809e4e80e7d8d4e20b9965efb2eefe71c5/src/crypto/x509/x509.go#L1439-L1456
	// This first version only supports the DN Subject field
	IDField := client.GetCertificateSubjectFieldName()
	if IDField != DNField {
		return nil, errorsx.WithStack(ErrInvalidClient.WithHintf("Client certificate field not supported: %s", IDField))
	}

	// this is expected to exists, validation already in isTLS method
	cert := r.TLS.PeerCertificates[0]

	// TODO: Implement a stronger matching using a RDN Sequence instead
	// of strings comparisons, which can be error prone or could
	// provide false positives.
	// For that the client certificate value must be parsed into
	// a RDN Sequence based on https://www.ietf.org/rfc/rfc4514.txt,
	// currently there is no a library, it must be by ourselfs.
	// Then check if the parsed RDNs are contained in cert.Subject.Names
	expStr := client.GetCertificateSubjectValue()
	if !strings.Contains(cert.Subject.String(), expStr) {
		return nil, errorsx.WithStack(ErrInvalidRequest.
			WithDebugf("Certificate does not contain expected subject. Given(%s), Expected(%s)",
				cert.Subject.Names,
				expStr))
	}

	// bingo!
	return client, nil
}

func isTLSAuth(r *http.Request, form url.Values) bool {
	// This implementation expects the client certificate in
	// r.TLS.PeerCertificates[0], which in a conventional mTLS,
	// is the certificate the connection is verified against.
	//
	// Here, it does not matter where the TLS was terminated
	// as long as the client certificate is set
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return false
	}

	return true
}

func findPublicKey(t *jwt.Token, set *jose.JSONWebKeySet, expectsRSAKey bool) (interface{}, error) {
	keys := set.Keys
	if len(keys) == 0 {
		return nil, errorsx.WithStack(ErrInvalidRequest.WithHintf("The retrieved JSON Web Key Set does not contain any keys."))
	}

	kid, ok := t.Header["kid"].(string)
	if ok {
		keys = set.Key(kid)
	}

	if len(keys) == 0 {
		return nil, errorsx.WithStack(ErrInvalidRequest.WithHintf("The JSON Web Token uses signing key with kid '%s', which could not be found.", kid))
	}

	for _, key := range keys {
		if key.Use != "sig" {
			continue
		}
		if expectsRSAKey {
			if k, ok := key.Key.(*rsa.PublicKey); ok {
				return k, nil
			}
		} else {
			if k, ok := key.Key.(*ecdsa.PublicKey); ok {
				return k, nil
			}
		}
	}

	if expectsRSAKey {
		return nil, errorsx.WithStack(ErrInvalidRequest.WithHintf("Unable to find RSA public key with use='sig' for kid '%s' in JSON Web Key Set.", kid))
	} else {
		return nil, errorsx.WithStack(ErrInvalidRequest.WithHintf("Unable to find ECDSA public key with use='sig' for kid '%s' in JSON Web Key Set.", kid))
	}
}

func clientCredentialsFromRequest(r *http.Request, form url.Values) (clientID, clientSecret string, err error) {
	if id, secret, ok := r.BasicAuth(); !ok {
		return clientCredentialsFromRequestBody(form, true)
	} else if clientID, err = url.QueryUnescape(id); err != nil {
		return "", "", errorsx.WithStack(ErrInvalidRequest.WithHint("The client id in the HTTP authorization header could not be decoded from 'application/x-www-form-urlencoded'.").WithWrap(err).WithDebug(err.Error()))
	} else if clientSecret, err = url.QueryUnescape(secret); err != nil {
		return "", "", errorsx.WithStack(ErrInvalidRequest.WithHint("The client secret in the HTTP authorization header could not be decoded from 'application/x-www-form-urlencoded'.").WithWrap(err).WithDebug(err.Error()))
	}

	return clientID, clientSecret, nil
}

func clientCredentialsFromRequestBody(form url.Values, forceID bool) (clientID, clientSecret string, err error) {
	clientID = form.Get("client_id")
	clientSecret = form.Get("client_secret")

	if clientID == "" && forceID {
		return "", "", errorsx.WithStack(ErrInvalidRequest.WithHint("Client credentials missing or malformed in both HTTP Authorization header and HTTP POST body."))
	}

	return clientID, clientSecret, nil
}
