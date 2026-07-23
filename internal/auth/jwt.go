package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwtTTL is the fixed token lifetime (24h) per the contract — a constant for
// now; rotation/expiry policy is a later contract's concern.
const jwtTTL = 24 * time.Hour

// ErrInvalidToken is the generic error for any JWT verification failure
// (bad signature, malformed, expired). It carries no detail so it cannot leak
// which check failed.
var ErrInvalidToken = errors.New("invalid token")

// Claims is the JWT payload: the standard registered claims (sub, exp, iat)
// plus the librarian-specific email and roles. sub holds the user id.
type Claims struct {
	Email string   `json:"email"`
	Roles []string `json:"roles"`
	jwt.RegisteredClaims
}

// IssueJWT signs an HS256 JWT for the given user with the supplied secret. The
// secret is the raw signing key; it is never logged. now is taken as a
// parameter so tests can drive expiry deterministically without touching the
// real clock.
func IssueJWT(secret string, user *User, now time.Time) (string, error) {
	if secret == "" {
		return "", errors.New("jwt secret must not be empty")
	}
	claims := Claims{
		Email: user.Email,
		Roles: user.Roles,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(jwtTTL)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signed, nil
}

// VerifyJWT parses and validates an HS256 token signed with secret, returning
// the claims on success. It rejects any non-HMAC signing method (so a token
// claiming "none" or an asymmetric alg cannot bypass verification). Any
// failure — wrong secret, malformed, expired — yields ErrInvalidToken.
func VerifyJWT(secret, tokenStr string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
