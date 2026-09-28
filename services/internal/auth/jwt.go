// Task 5.3.1 — JWT access-token issuance and verification.
//
// Spec §8.1: JWT for the REST API — access token 15min, refresh token 7d
// (refresh tokens are opaque bearer strings handled by session.go; this
// file owns only signed access tokens). Spec §8.8 item 3: JWT `kid`
// rotation with RS256/KMS alongside HS256 — implemented as a keyring
// where every key ID is bound to exactly one algorithm and key material,
// so verification can never be tricked into cross-algorithm confusion.
//
// Fail-closed: tokens without kid/exp, with an unknown kid, an algorithm
// that doesn't match the key's registered alg, or any claim-validation
// failure all reject with UNAUTHORIZED.
package auth

import (
	"crypto/ed25519"
	"crypto/rsa"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessTokenTTL is the spec §8.1 access-token lifetime.
const AccessTokenTTL = 15 * time.Minute

// NumericDate claims marshal with TimePrecision (library default:
// seconds). Second-truncation would silently extend every token's
// effective lifetime by up to 999ms — and makes sub-second session
// expiry (WS AUTH_EXPIRED contract, spec §10.5 item 5) unrepresentable.
// Millisecond precision removes both defects for all issuers.
func init() {
	jwt.TimePrecision = time.Millisecond
}

// TokenTypeAccess marks JWTs usable on REST/WS authentication surfaces.
const TokenTypeAccess = "access"

// SigningKey is one keyring entry: key material bound to a single JWS
// algorithm. SignKey nil means verify-only (public-key entries for
// KMS-held signers introduced during rotation windows).
type SigningKey struct {
	KID       string
	Alg       string // "HS256", "RS256", "EdDSA"
	SignKey   any    // []byte | *rsa.PrivateKey | ed25519.PrivateKey
	VerifyKey any    // []byte | *rsa.PublicKey  | ed25519.PublicKey
}

// Issuer mints and verifies access JWTs against a kid-keyed keyring.
type Issuer struct {
	issuer    string
	audience  string
	accessTTL time.Duration
	keys      map[string]SigningKey
	activeKID string
	now       func() time.Time // test seam
}

// NewIssuer returns an Issuer. accessTTL<=0 uses the spec §8.1 15-minute
// lifetime. iss/aud are enforced on verification; empty values disable
// that claim check (tests may pass "").
func NewIssuer(issuer, audience string, accessTTL time.Duration) *Issuer {
	if accessTTL <= 0 {
		accessTTL = AccessTokenTTL
	}
	return &Issuer{
		issuer:    issuer,
		audience:  audience,
		accessTTL: accessTTL,
		keys:      make(map[string]SigningKey),
		now:       time.Now,
	}
}

// AddHMACKey registers an HS256 key. Secrets must be >=32 bytes —
// anything shorter is rejected rather than weakened (fail-closed).
// active marks it the signing key.
func (i *Issuer) AddHMACKey(kid string, secret []byte, active bool) error {
	if len(secret) < 32 {
		return newError(CodeAuthInternal, "HS256 secret must be >= 32 bytes")
	}
	k := SigningKey{KID: kid, Alg: "HS256", SignKey: secret, VerifyKey: secret}
	return i.addKey(k, active)
}

// AddRSAKey registers an RS256 keypair for sign+verify, or the public
// half alone for verify-only rotation overlap. priv may be nil.
func (i *Issuer) AddRSAKey(kid string, priv *rsa.PrivateKey, pub *rsa.PublicKey, active bool) error {
	if pub == nil && priv != nil {
		pub = &priv.PublicKey
	}
	if pub == nil {
		return newError(CodeAuthInternal, "RSA key requires public key material")
	}
	if pub.N.BitLen() < 2048 {
		return newError(CodeAuthInternal, "RSA key must be >= 2048 bits")
	}
	k := SigningKey{KID: kid, Alg: "RS256", SignKey: priv, VerifyKey: pub}
	return i.addKey(k, active)
}

// AddEd25519Key registers an EdDSA keypair or public-only verify key.
func (i *Issuer) AddEd25519Key(kid string, priv ed25519.PrivateKey, pub ed25519.PublicKey, active bool) error {
	if pub == nil && priv != nil {
		pub = priv.Public().(ed25519.PublicKey)
	}
	if len(pub) == 0 {
		return newError(CodeAuthInternal, "Ed25519 key requires public key material")
	}
	k := SigningKey{KID: kid, Alg: "EdDSA", SignKey: priv, VerifyKey: pub}
	return i.addKey(k, active)
}

func (i *Issuer) addKey(k SigningKey, active bool) error {
	if k.KID == "" {
		return newError(CodeAuthInternal, "kid must not be empty")
	}
	i.keys[k.KID] = k
	if active || i.activeKID == "" {
		i.activeKID = k.KID
	}
	return nil
}

// jwtClaims is the wire shape. Registered claims plus the exchange
// extensions pinned by spec §8.8/§12.6 (amr for MFA elevation, sid for
// session binding, scopes for the API-key scope matrix).
type jwtClaims struct {
	jwt.RegisteredClaims
	TokenType string   `json:"typ"`
	SessionID string   `json:"sid,omitempty"`
	AccountID int64    `json:"account_id,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	AMR       []string `json:"amr,omitempty"`
	ClientID  string   `json:"cid,omitempty"` // OAuth2 client_credentials client
}

// IssueOptions carries the optional identity facets for a minted token.
type IssueOptions struct {
	AccountID int64
	SessionID string
	Scopes    []string
	AMR       []string
	ClientID  string
}

// Issue mints a signed access token for subject (a user id, or
// "oauth2:{client_id}" for client-credentials grants) and returns it
// with the parsed Claims view.
func (i *Issuer) Issue(subject string, opts IssueOptions) (string, Claims, error) {
	key, ok := i.keys[i.activeKID]
	if !ok || key.SignKey == nil {
		return "", Claims{}, newError(CodeAuthInternal, "no active signing key configured")
	}
	now := i.now()
	exp := now.Add(i.accessTTL)
	jti, err := randomToken(16)
	if err != nil {
		return "", Claims{}, wrapError(CodeAuthInternal, "jti generation", err)
	}
	jc := jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			NotBefore: jwt.NewNumericDate(now),
		},
		TokenType: TokenTypeAccess,
		SessionID: opts.SessionID,
		AccountID: opts.AccountID,
		Scopes:    opts.Scopes,
		AMR:       opts.AMR,
		ClientID:  opts.ClientID,
	}
	if i.issuer != "" {
		jc.Issuer = i.issuer
	}
	if i.audience != "" {
		jc.Audience = jwt.ClaimStrings{i.audience}
	}
	var method jwt.SigningMethod
	switch key.Alg {
	case "HS256":
		method = jwt.SigningMethodHS256
	case "RS256":
		method = jwt.SigningMethodRS256
	case "EdDSA":
		method = jwt.SigningMethodEdDSA
	default:
		return "", Claims{}, newError(CodeAuthInternal, "unsupported signing algorithm "+key.Alg)
	}
	tok := jwt.NewWithClaims(method, jc)
	tok.Header["kid"] = key.KID
	signed, err := tok.SignedString(key.SignKey)
	if err != nil {
		return "", Claims{}, wrapError(CodeAuthInternal, "jwt sign", err)
	}
	return signed, Claims{
		Subject:   subject,
		AccountID: opts.AccountID,
		Scopes:    opts.Scopes,
		SessionID: opts.SessionID,
		AMR:       opts.AMR,
		ClientID:  opts.ClientID,
		TokenType: TokenTypeAccess,
		KeyID:     key.KID,
		IssuedAt:  now,
		ExpiresAt: exp,
	}, nil
}

// Parse verifies a token: kid lookup, per-key algorithm pinning, and
// iss/aud/exp/nbf validation. Every failure maps to UNAUTHORIZED — the
// gateway must not leak which validation failed (spec §2.7 edge
// rejections give an L3 code, not an oracle).
func (i *Issuer) Parse(tokenStr string) (Claims, error) {
	var jc jwtClaims
	var usedKID string
	opts := []jwt.ParserOption{
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithValidMethods([]string{"HS256", "RS256", "EdDSA"}),
	}
	if i.issuer != "" {
		opts = append(opts, jwt.WithIssuer(i.issuer))
	}
	if i.audience != "" {
		opts = append(opts, jwt.WithAudience(i.audience))
	}
	_, err := jwt.ParseWithClaims(tokenStr, &jc, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, newError(CodeUnauthorized, "missing kid")
		}
		key, ok := i.keys[kid]
		if !ok {
			return nil, newError(CodeUnauthorized, "unknown kid")
		}
		usedKID = kid
		// Algorithm is pinned per key: a token claiming a different alg
		// for this kid is rejected even though the verifier key itself
		// would already fail the signature.
		if t.Method.Alg() != key.Alg {
			return nil, newError(CodeUnauthorized, "algorithm mismatch for kid")
		}
		return key.VerifyKey, nil
	}, opts...)
	if err != nil {
		return Claims{}, wrapError(CodeUnauthorized, "jwt validation", err)
	}
	if jc.TokenType != TokenTypeAccess {
		return Claims{}, newError(CodeUnauthorized, "wrong token type")
	}
	var iat, exp time.Time
	if jc.IssuedAt != nil {
		iat = jc.IssuedAt.Time
	}
	if jc.ExpiresAt != nil {
		exp = jc.ExpiresAt.Time
	}
	return Claims{
		Subject:   jc.Subject,
		AccountID: jc.AccountID,
		Scopes:    jc.Scopes,
		SessionID: jc.SessionID,
		AMR:       jc.AMR,
		ClientID:  jc.ClientID,
		TokenType: jc.TokenType,
		KeyID:     usedKID,
		IssuedAt:  iat,
		ExpiresAt: exp,
	}, nil
}
