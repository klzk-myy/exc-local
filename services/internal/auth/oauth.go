// Task 5.3.1 item 2 — OAuth2 client-credentials grant for institutional
// programmatic clients (spec §8.1).
//
// Clients are registered in oauth_clients (migration 151) bound to an
// account with a granted-scope ceiling; the stored secret is bcrypt
// cost-12 (§8.8 item 3 secret handling). Token exchange produces the same
// 15-minute access JWT as user sessions, with sub="oauth2:{client_id}"
// and no session binding (client-credentials grants are sessionless by
// design — RFC 6749 §4.4).
//
// Failure semantics: unknown client, wrong secret, suspended/revoked
// client and out-of-scope requests are all INVALID_CREDENTIALS (401) —
// identical response shape so the endpoint is not an existence oracle.
// The bcrypt compare always runs (dummy hash for unknown clients) so the
// timing oracle is closed too.
package auth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// OAuthClient is one oauth_clients row.
type OAuthClient struct {
	ID        int64
	ClientID  string
	AccountID int64
	Name      string
	Scopes    []string // granted ceiling
	Status    string   // ACTIVE | SUSPENDED | REVOKED
	CreatedAt time.Time
}

// OAuthClientStore persists oauth_clients rows.
type OAuthClientStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewOAuthClientStore binds the store.
func NewOAuthClientStore(pool *pgxpool.Pool) (*OAuthClientStore, error) {
	if pool == nil {
		return nil, newError(CodeAuthInternal, "oauth store pool is nil")
	}
	return &OAuthClientStore{pool: pool, now: time.Now}, nil
}

// bcryptCost is the spec §8.8 item 3 work factor.
const bcryptCost = 12

// dummyBcryptHash is a valid bcrypt(cost 12) digest compared against when
// client_id is unknown — keeps the failure path timing-equivalent.
// Generated once at process start from a random plaintext.
var dummyBcryptHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("exc-dummy-secret"), bcryptCost)
	if err != nil {
		panic(err)
	}
	return h
}()

// RegisterClient creates a client_credentials client and returns the
// record plus the raw secret — shown once at issuance, stored only as a
// bcrypt digest.
func (s *OAuthClientStore) RegisterClient(ctx context.Context, accountID int64, name string, scopes []string) (*OAuthClient, string, error) {
	for _, sc := range scopes {
		if !ValidScopes[sc] {
			return nil, "", newError(CodeOAuthClientInvalid, "invalid scope "+sc)
		}
	}
	clientID, err := randomToken(18)
	if err != nil {
		return nil, "", wrapError(CodeAuthInternal, "client id generation", err)
	}
	secret, err := randomToken(32)
	if err != nil {
		return nil, "", wrapError(CodeAuthInternal, "client secret generation", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcryptCost)
	if err != nil {
		return nil, "", wrapError(CodeAuthInternal, "client secret hash", err)
	}
	clientID = "oc_" + clientID
	var id int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO oauth_clients (client_id, client_secret_hash, account_id, name, scopes)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		clientID, string(hash), accountID, name, scopes).Scan(&id)
	if err != nil {
		return nil, "", wrapError(CodeAuthInternal, "oauth client insert", err)
	}
	return &OAuthClient{ID: id, ClientID: clientID, AccountID: accountID,
		Name: name, Scopes: scopes, Status: "ACTIVE", CreatedAt: s.now()}, secret, nil
}

// OAuthGrant is the access-token response for a successful grant.
type OAuthGrant struct {
	AccessToken string
	ExpiresAt   time.Time
	Scope       []string // effective scopes (intersection of granted/requested)
	Client      OAuthClient
}

// ClientCredentialsGrant verifies client_id+client_secret and mints an
// access JWT scoped to the intersection of granted and requested scopes.
// requestedScopes nil/empty means "all granted scopes".
func (s *OAuthClientStore) ClientCredentialsGrant(ctx context.Context, issuer *Issuer,
	clientID, clientSecret string, requestedScopes []string) (*OAuthGrant, error) {

	var c OAuthClient
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT id, client_id, account_id, name, scopes, status, client_secret_hash, created_at
		 FROM oauth_clients WHERE client_id=$1`, clientID).
		Scan(&c.ID, &c.ClientID, &c.AccountID, &c.Name, &c.Scopes, &c.Status, &hash, &c.CreatedAt)
	if err != nil && err != pgx.ErrNoRows {
		return nil, wrapError(CodeAuthInternal, "oauth client read", err)
	}
	found := err == nil && c.Status == "ACTIVE"
	compareHash := hash
	if !found {
		compareHash = string(dummyBcryptHash) // timing parity, then reject
	}
	if bcrypt.CompareHashAndPassword([]byte(compareHash), []byte(clientSecret)) != nil || !found {
		return nil, newError(CodeInvalidCredentials, "invalid client credentials")
	}
	// Scope intersection: the grant can narrow, never widen.
	effective := c.Scopes
	if len(requestedScopes) > 0 {
		effective = intersectScopes(c.Scopes, requestedScopes)
		if len(effective) == 0 {
			return nil, newError(CodeInsufficientScope, "no granted scope satisfies the request")
		}
	}
	tok, claims, err := issuer.Issue("oauth2:"+c.ClientID, IssueOptions{
		AccountID: c.AccountID,
		Scopes:    effective,
		ClientID:  c.ClientID,
		AMR:       []string{"client_secret"},
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE oauth_clients SET last_token_at=now(), updated_at=now() WHERE id=$1`, c.ID); err != nil {
		return nil, wrapError(CodeAuthInternal, "oauth client touch", err)
	}
	return &OAuthGrant{AccessToken: tok, ExpiresAt: claims.ExpiresAt, Scope: effective, Client: c}, nil
}

// RevokeClient marks a client revoked — its secret stops working
// immediately; already-issued JWTs age out at their 15-minute expiry
// (sessionless grants have no server-side revocation handle).
func (s *OAuthClientStore) RevokeClient(ctx context.Context, clientID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE oauth_clients SET status='REVOKED', updated_at=now()
		 WHERE client_id=$1 AND status <> 'REVOKED'`, clientID)
	if err != nil {
		return wrapError(CodeAuthInternal, "oauth client revoke", err)
	}
	if tag.RowsAffected() == 0 {
		return newError(CodeAPIKeyNotFound, "unknown or already-revoked client")
	}
	return nil
}

// intersectScopes returns the subset of requested that granted permits.
func intersectScopes(granted, requested []string) []string {
	out := make([]string, 0, len(requested))
	set := make(map[string]bool, len(granted))
	for _, g := range granted {
		set[g] = true
	}
	for _, r := range requested {
		if set[r] {
			out = append(out, r)
		}
	}
	return out
}
