package dpop

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// HTTPVerb is a convenience for determining the HTTP method of a request.
// This package defines the all available HTTP verbs which can be used when calling the Parse function.
type HTTPVerb string

// HTTP method supported by the package.
const (
	GET     HTTPVerb = "GET"
	POST    HTTPVerb = "POST"
	PUT     HTTPVerb = "PUT"
	DELETE  HTTPVerb = "DELETE"
	PATCH   HTTPVerb = "PATCH"
	HEAD    HTTPVerb = "HEAD"
	OPTIONS HTTPVerb = "OPTIONS"
	TRACE   HTTPVerb = "TRACE"
	CONNECT HTTPVerb = "CONNECT"
)

const DEFAULT_ALLOWED_PROOF_AGE = time.Minute * 5
const DEFAULT_ALLOWED_TIME_WINDOW = time.Second * 0

// ParseOptions and its contents are optional for the Parse function.
type ParseOptions struct {
	// The expected nonce if the authorization server has issued a nonce.
	Nonce string

	// Used to control if the `iat` field is within allowed clock-skew.
	// If set to true the authorization server has to validate the nonce timestamp itself.
	NonceHasTimestamp bool

	// The allowed clock-skew (into the future) on the `iat` of the proof. If not set proof is rejected if issued in the future.
	TimeWindow *time.Duration

	// The allowed age of a proof. If not set the default is 5 minutes.
	AllowedProofAge *time.Duration

	// dpop_jkt parameter that is optionally sent by the client to the authorization server on token request.
	// If set the proof proof-of-possession public key needs to match or the proof is rejected.
	JKT string
}

// Parse translates a DPoP proof string into a JWT token and parses it with the jwt package (github.com/golang-jwt/jwt/v5).
// It will also validate the proof according to https://datatracker.ietf.org/doc/html/rfc9449#section-4.3
// but not check whether the proof matches a bound access token. It also assumes point 1 is checked by the calling application.
//
// Protected resources should use the 'Validate' function on the returned proof to ensure that the proof matches any bound access token.
func Parse(
	tokenString string,
	httpMethod HTTPVerb,
	httpURL *url.URL,
	opts ParseOptions,
) (*Proof, error) {
	// Parse the token string
	// Ensure that it is a well-formed JWT, that a supported signature algorithm is used,
	// that it contains a public key, and that the signature verifies with the public key.
	// This satisfies point 2, 5, 6 and 7 in https://datatracker.ietf.org/doc/html/rfc9449#section-4.3
	claims := ProofTokenClaims{RegisteredClaims: &jwt.RegisteredClaims{}}
	dpopToken, err := jwt.ParseWithClaims(tokenString, &claims, keyFunc)
	if err != nil {
		return nil, errors.Join(ErrInvalidProof, err)
	}

	// Check that all claims have been populated
	// This satisfies point 3 in https://datatracker.ietf.org/doc/html/rfc9449#section-4.3
	if claims.Method == "" || claims.URL == "" || claims.ID == "" || claims.IssuedAt == nil {
		return nil, errors.Join(ErrInvalidProof, ErrMissingClaims)
	}

	// Check `typ` JOSE header that it is correct
	// This satisfies point 4 in https://datatracker.ietf.org/doc/html/rfc9449#section-4.3
	typeHeader := dpopToken.Header["typ"]
	if typeHeader == nil || typeHeader.(string) != "dpop+jwt" {
		return nil, errors.Join(ErrInvalidProof, ErrUnsupportedJWTType)
	}

	// Don't modify the original httpURL
	httpURL = &(*httpURL)
	// Strip the incoming URI of query and fragment according to point 9 in https://datatracker.ietf.org/doc/html/rfc9449#section-4.3
	httpURL.RawQuery = ""
	httpURL.Fragment = ""

	// Spec-non-compliant hack to work around
	// https://github.com/bluesky-social/atproto/issues/3846
	claimURL, err := url.Parse(claims.URL)
	if err != nil {
		return nil, errors.Join(ErrInvalidProof, ErrIncorrectHTTPTarget)
	}
	claimURL.RawQuery = ""
	claimURL.Fragment = ""

	// Check that `htm` and `htu` claims match the HTTP method and URL of the current request.
	// This satisfies point 8 and 9 in https://datatracker.ietf.org/doc/html/rfc9449#section-4.3
	if httpMethod != claims.Method || httpURL.String() != claimURL.String() {
		return nil, errors.Join(ErrInvalidProof, ErrIncorrectHTTPTarget)
	}

	// Check that `nonce` is correct
	// This satisfies point 10 in https://datatracker.ietf.org/doc/html/rfc9449#section-4.3
	if opts.Nonce != "" && opts.Nonce != claims.Nonce {
		return nil, ErrIncorrectNonce
	}

	// Check that `iat` is within the acceptable window unless `nonce` contains a server managed timestamp.
	// This satisfies point 11 in https://datatracker.ietf.org/doc/html/rfc9449#section-4.3
	if !opts.NonceHasTimestamp {
		// Check that `iat` is not too far into the past.
		past := DEFAULT_ALLOWED_PROOF_AGE
		if opts.AllowedProofAge != nil {
			past = *opts.AllowedProofAge
		}
		if claims.IssuedAt.Before(time.Now().Add(-past)) {
			return nil, errors.Join(ErrInvalidProof, ErrExpired)
		}

		// Check that `iat` is not too far into the future.
		future := DEFAULT_ALLOWED_TIME_WINDOW
		if opts.TimeWindow != nil {
			future = *opts.TimeWindow
		}
		if claims.IssuedAt.After(time.Now().Add(future)) {
			return nil, errors.Join(ErrInvalidProof, ErrFuture)
		}
	}

	// Extract the public key from the proof and hash it.
	// This is done in order to store the public key
	// without the need for extracting and hashing it again.
	jwk, ok := dpopToken.Header["jwk"].(map[string]interface{})
	if !ok {
		// keyFunc used with parseWithClaims should ensure that this can not happen but better safe than sorry.
		return nil, ErrMissingJWK
	}
	jwkJSONbytes, err := getThumbprintableJwkJSONbytes(jwk)
	if err != nil {
		// keyFunc used with parseWithClaims should ensure that this can not happen but better safe than sorry.
		return nil, errors.Join(ErrInvalidProof, err)
	}
	h := sha256.New()
	_, err = h.Write(jwkJSONbytes)
	if err != nil {
		return nil, errors.Join(ErrInvalidProof, err)
	}
	b64URLjwkHash := base64.RawURLEncoding.EncodeToString(h.Sum(nil))

	// Check that `dpop_jkt` is correct if supplied to the authorization server on token request.
	// This satisfies https://datatracker.ietf.org/doc/html/rfc9449#name-authorization-code-binding-
	if opts.JKT != "" {
		if b64URLjwkHash != opts.JKT {
			return nil, errors.Join(ErrInvalidProof, ErrIncorrectJKT)
		}
	}

	return &Proof{
		Token:           dpopToken,
		HashedPublicKey: b64URLjwkHash,
	}, nil
}

func keyFunc(t *jwt.Token) (interface{}, error) {
	// Return the required jwkHeader header. See https://datatracker.ietf.org/doc/html/rfc9449#section-4.2
	// Used to validate the signature of the DPoP proof.
	jwkHeader := t.Header["jwk"]
	if jwkHeader == nil {
		return nil, ErrMissingJWK
	}

	jwkMap, ok := jwkHeader.(map[string]interface{})
	if !ok {
		return nil, ErrMissingJWK
	}

	return parseJwk(jwkMap)
}

// Parses a JWK and inherently strips it of optional fields
func parseJwk(jwkMap map[string]interface{}) (interface{}, error) {
	// Ensure that JWK kty is present and is a string.
	kty, ok := jwkMap["kty"].(string)
	if !ok {
		return nil, ErrInvalidProof
	}
	switch kty {
	case "EC":
		// Ensure that the required fields are present and are strings.
		x, ok := jwkMap["x"].(string)
		if !ok {
			return nil, ErrInvalidProof
		}
		y, ok := jwkMap["y"].(string)
		if !ok {
			return nil, ErrInvalidProof
		}
		crv, ok := jwkMap["crv"].(string)
		if !ok {
			return nil, ErrInvalidProof
		}

		// Decode the coordinates from Base64.
		//
		// According to RFC 7518, they are Base64 URL unsigned integers.
		// https://tools.ietf.org/html/rfc7518#section-6.3
		xCoordinate, err := base64urlTrailingPadding(x)
		if err != nil {
			return nil, err
		}
		yCoordinate, err := base64urlTrailingPadding(y)
		if err != nil {
			return nil, err
		}

		// Read the specified curve of the key.
		var curve elliptic.Curve
		switch crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, ErrUnsupportedCurve
		}

		return &ecdsa.PublicKey{
			X:     big.NewInt(0).SetBytes(xCoordinate),
			Y:     big.NewInt(0).SetBytes(yCoordinate),
			Curve: curve,
		}, nil
	case "RSA":
		// Ensure that the required fields are present and are strings.
		e, ok := jwkMap["e"].(string)
		if !ok {
			return nil, ErrInvalidProof
		}
		n, ok := jwkMap["n"].(string)
		if !ok {
			return nil, ErrInvalidProof
		}

		// Decode the exponent and modulus from Base64.
		//
		// According to RFC 7518, they are Base64 URL unsigned integers.
		// https://tools.ietf.org/html/rfc7518#section-6.3
		exponent, err := base64urlTrailingPadding(e)
		if err != nil {
			return nil, err
		}
		modulus, err := base64urlTrailingPadding(n)
		if err != nil {
			return nil, err
		}
		return &rsa.PublicKey{
			N: big.NewInt(0).SetBytes(modulus),
			E: int(big.NewInt(0).SetBytes(exponent).Uint64()),
		}, nil
	case "OKP":
		// Ensure that the required fields are present and are strings.
		x, ok := jwkMap["x"].(string)
		if !ok {
			return nil, ErrInvalidProof
		}

		publicKey, err := base64urlTrailingPadding(x)
		if err != nil {
			return nil, err
		}

		return ed25519.PublicKey(publicKey), nil
	case "OCT":
		return nil, ErrUnsupportedKeyAlgorithm
	default:
		return nil, ErrUnsupportedKeyAlgorithm
	}
}

// Borrowed from MicahParks/keyfunc See: https://github.com/MicahParks/keyfunc/blob/master/keyfunc.go#L56
//
// base64urlTrailingPadding removes trailing padding before decoding a string from base64url. Some non-RFC compliant
// JWKS contain padding at the end values for base64url encoded public keys.
//
// Trailing padding is required to be removed from base64url encoded keys.
// RFC 7517 Section 1.1 defines base64url the same as RFC 7515 Section 2:
// https://datatracker.ietf.org/doc/html/rfc7517#section-1.1
// https://datatracker.ietf.org/doc/html/rfc7515#section-2
func base64urlTrailingPadding(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}

// Strips eventual optional members of a JWK in order to be able to compute the thumbprint of it
// https://datatracker.ietf.org/doc/html/rfc7638#section-3.2
func getThumbprintableJwkJSONbytes(jwk map[string]interface{}) ([]byte, error) {
	minimalJwk, err := parseJwk(jwk)
	if err != nil {
		return nil, err
	}
	jwkHeaderJSONBytes, err := getKeyStringRepresentation(minimalJwk)
	if err != nil {
		return nil, err
	}
	return jwkHeaderJSONBytes, nil
}

// Returns the string representation of a key in JSON format.
func getKeyStringRepresentation(key interface{}) ([]byte, error) {
	var keyParts interface{}
	switch key := key.(type) {
	case *ecdsa.PublicKey:
		// Calculate the size of the byte array representation of an elliptic curve coordinate
		// and ensure that the byte array representation of the key is padded correctly.
		bits := key.Curve.Params().BitSize
		keyCurveBytesSize := bits/8 + bits%8

		keyParts = map[string]interface{}{
			"kty": "EC",
			"crv": key.Curve.Params().Name,
			"x":   base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, keyCurveBytesSize))),
			"y":   base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, keyCurveBytesSize))),
		}
	case *rsa.PublicKey:
		keyParts = map[string]interface{}{
			"kty": "RSA",
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		}
	case ed25519.PublicKey:
		keyParts = map[string]interface{}{
			"kty": "OKP",
			"crv": "Ed25519",
			"x":   base64.RawURLEncoding.EncodeToString(key),
		}
	default:
		return nil, ErrUnsupportedKeyAlgorithm
	}

	return json.Marshal(keyParts)
}
